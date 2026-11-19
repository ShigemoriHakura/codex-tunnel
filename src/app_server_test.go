package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitFor[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

// Exercise the actual pinned CLI against a Responses endpoint on loopback. Every
// launch records its own PID, and the provider holds each response independently.
func TestResidentCLI(t *testing.T) {
	cli := os.Getenv("TEST_CODEX_BIN")
	if cli == "" {
		t.Skip("set TEST_CODEX_BIN for the real app-server multiplexing test")
	}
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	type exchange struct {
		label  string
		finish chan struct{}
		closed chan struct{}
	}
	arrivals := make(chan exchange, 12)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Input []struct {
				Role    string
				Content []struct{ Text string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var label string
		for _, item := range body.Input {
			if item.Role == "user" {
				for _, content := range item.Content {
					if strings.HasPrefix(content.Text, "resident-") {
						label = content.Text
					}
				}
			}
		}
		if label == "" {
			t.Error("missing test turn input")
			return
		}
		x := exchange{label, make(chan struct{}), make(chan struct{})}
		defer close(x.closed)
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, params M) {
			params["type"] = kind
			body, _ := json.Marshal(params)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, body)
		}
		event("response.created", M{"response": M{"id": label}})
		event("response.output_item.added", M{"item": M{"id": label, "type": "message", "role": "assistant", "phase": "final_answer", "content": []M{}}})
		event("response.output_text.delta", M{"item_id": label, "delta": label})
		w.(http.Flusher).Flush()
		arrivals <- x
		select {
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		case <-x.finish:
		}
		event("response.output_item.done", M{"item": M{"id": label, "type": "message", "role": "assistant", "phase": "final_answer", "content": []M{{"type": "output_text", "text": label}}}})
		event("response.completed", M{"response": M{"id": label, "status": "completed", "usage": M{"input_tokens": len(label), "output_tokens": 1, "total_tokens": len(label) + 1}}})
	}))
	defer mock.Close()
	d := t.TempDir()
	r := runner{filepath.Join(d, "cli-wrapper"), d, d}
	if err := os.Symlink(cli, filepath.Join(d, "real-codex")); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho $$ >> \"$CODEX_HOME/pids\"\nexec \"$CODEX_HOME/real-codex\" \"$@\"\n"
	if err := os.WriteFile(r.bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-loopback","auth_mode":"apikey"}`), 0600); err != nil {
		t.Fatal(err)
	}
	writeMockProviderConfig(t, d, mock.URL)
	m := newAppServers(ctx)
	defer m.close()

	type outcome struct {
		thread, deltas string
		res            result
		err            error
	}
	launch := func(ctx context.Context, label, thread string) <-chan outcome {
		ended := make(chan outcome, 1)
		go func() {
			var stream strings.Builder
			var out outcome
			out.res, out.err = r.run(ctx, m, turn{
				thread: thread, persist: true, input: []turnInput{{Type: "text", Text: label}},
				onThread: func(id, _ string) error { out.thread = id; return nil },
				delta:    func(delta string) error { stream.WriteString(delta); return nil },
			})
			out.deltas = stream.String()
			ended <- out
		}()
		return ended
	}
	getPIDs := func(want int) []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(d, "pids"))
		pids := strings.Fields(string(data))
		if err != nil || len(pids) != want {
			t.Fatalf("expected %d CLI starts, got %v (%v)", want, pids, err)
		}
		return pids
	}
	expect := func(ch <-chan outcome, label string) outcome {
		t.Helper()
		out := waitFor(t, ctx, ch)
		if out.err != nil || out.res.text != label || out.deltas != label || !out.res.metered || out.res.total.Output < 1 || out.thread == "" {
			t.Fatalf("incorrect isolated result for %s: %q / %q, usage %+v, error %v", label, out.res.text, out.deltas, out.res.total, out.err)
		}
		return out
	}
	accept := func(label string) exchange {
		t.Helper()
		x := waitFor(t, ctx, arrivals)
		if x.label != label {
			t.Fatalf("expected %s, provider got %s", label, x.label)
		}
		return x
	}

	one, cancelOne := context.WithCancel(ctx)
	defer cancelOne()
	first := launch(one, "resident-cancel", "")
	x := accept("resident-cancel")
	second := launch(ctx, "resident-keep", "")
	y := accept("resident-keep")
	getPIDs(1)
	cancelOne()
	if out := waitFor(t, ctx, first); !errors.Is(out.err, context.Canceled) {
		t.Fatalf("canceled turn: %v", out.err)
	}
	waitFor(t, ctx, x.closed)
	close(y.finish)
	keep := expect(second, y.label)
	if keep.res.total.Input != int64(len(y.label)) {
		t.Fatal("received usage for the wrong request", keep.res.total)
	}

	next := launch(ctx, "resident-resume", keep.thread)
	z := accept("resident-resume")
	parallel := launch(ctx, "resident-independent", "")
	side := accept("resident-independent")
	close(side.finish)
	isolated := expect(parallel, side.label)
	if isolated.thread == keep.thread || isolated.res.total.Input != int64(len(side.label)) {
		t.Fatal("independent requests shared a thread or usage")
	}
	close(z.finish)
	if resumed := expect(next, z.label); resumed.thread != keep.thread {
		t.Fatal("session changed threads")
	}

	crash := launch(ctx, "resident-interrupted", keep.thread)
	x = accept("resident-interrupted")
	other := launch(ctx, "resident-other", "")
	y = accept("resident-other")
	pid, err := strconv.Atoi(getPIDs(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan outcome{crash, other} {
		if out := waitFor(t, ctx, ch); !errors.Is(out.err, errServerExited) {
			t.Fatalf("crashed turn: %v", out.err)
		}
	}
	waitFor(t, ctx, x.closed)
	waitFor(t, ctx, y.closed)
	recovered := launch(ctx, "resident-recovered", keep.thread)
	z = accept("resident-recovered")
	close(z.finish)
	if out := expect(recovered, z.label); out.thread != keep.thread {
		t.Fatal("restart lost the persisted session")
	}
	getPIDs(2)

	// A configuration change rotates the process but lets the old lease finish.
	draining := launch(ctx, "resident-drain", "")
	old := accept("resident-drain")
	config, err := os.OpenFile(filepath.Join(d, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.WriteString("\n# reloaded\n")
	config.Close()
	if err != nil {
		t.Fatal(err)
	}
	fresh := launch(ctx, "resident-new-config", "")
	newConfig := accept("resident-new-config")
	getPIDs(3)
	close(old.finish)
	expect(draining, old.label)
	close(newConfig.finish)
	expect(fresh, newConfig.label)
}

// A stalled HTTP consumer and an unsolicited client-action request must not
// block delivery on a different thread or steal a pending JSON-RPC reply.
func TestAppServerDispatch(t *testing.T) {
	s := &appServer{threads: make(map[string]*turnInbox), pending: make(map[uint64]chan packet), denials: make(chan M, 1)}
	slow, _ := s.subscribe("slow")
	other, _ := s.subscribe("other")
	notify := func(thread string) {
		data, _ := json.Marshal(M{"threadId": thread, "turnId": "turn", "delta": thread})
		if !s.dispatch(packet{Method: "item/agentMessage/delta", Params: data}) {
			t.Fatal("stalled consumer stopped the reader")
		}
	}
	for i := 0; i <= cap(slow.events); i++ {
		notify("slow")
	}
	if _, err := slow.next(t.Context(), t.Context()); !errors.Is(err, errSlowConsumer) {
		t.Fatal("lost overflow error", err)
	}
	notify("other")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	v, err := other.next(ctx, ctx)
	if err != nil || !strings.Contains(string(v.Params), `"delta":"other"`) {
		t.Fatal("lost independent notification", err)
	}
	reply := make(chan packet, 1)
	s.pending[42] = reply
	s.dispatch(packet{ID: json.RawMessage("42"), Method: "item/commandExecution/requestApproval"})
	denial := waitFor(t, ctx, s.denials)
	if denial["error"].(M)["code"] != -32601 {
		t.Fatal("client action was not rejected")
	}
	s.dispatch(packet{ID: json.RawMessage("42"), Result: json.RawMessage(`"ok"`)})
	if got := waitFor(t, ctx, reply); string(got.Result) != `"ok"` {
		t.Fatal("wrong reply")
	}
}

func TestLateTurnAcknowledgement(t *testing.T) {
	ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
	defer stop()
	s := &appServer{ctx: ctx, writes: make(chan M), pending: make(map[uint64]chan packet)}
	req, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		_, err := s.call(req, "turn/start", M{"threadId": "one"})
		ended <- err
	}()
	start := waitFor(t, ctx, s.writes)
	cancel()
	if err := waitFor(t, ctx, ended); !errors.Is(err, errUnconfirmedRPC) || !errors.Is(err, context.Canceled) {
		t.Fatal("lost uncertain turn state", err)
	}
	ack := func(frame M, result string) {
		id, _ := json.Marshal(frame["id"])
		s.dispatch(packet{ID: id, Result: json.RawMessage(result)})
	}
	ack(start, `{"turn":{"id":"late"}}`)
	interrupt := waitFor(t, ctx, s.writes)
	params := interrupt["params"].(M)
	if interrupt["method"] != "turn/interrupt" || params["threadId"] != "one" || params["turnId"] != "late" {
		t.Fatal("late acknowledgement did not interrupt its original turn")
	}
	ack(interrupt, "{}")
}
