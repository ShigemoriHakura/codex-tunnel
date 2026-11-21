package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsesValidation(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	for _, body := range []string{`{}`, `{"model":"fixture","input":[{"type":"item_reference","id":"shared"}]}`, `{"model":"fixture","input":"x","tools":[{"type":"file_search"}]}`, `{"model":"fixture","input":[{"role":"user","content":[{"type":"input_image","file_id":"other"}]}]}`, `{"model":"fixture","input":"x","background":true}`} {
		hit(t, a, "POST", "/v1/responses", key, body, "", 400)
	}
	hit(t, a, "POST", "/v1/responses", "bad", `{}`, "", 401)
}

func TestResponsesParallelToolCallsOption(t *testing.T) {
	for _, option := range []string{"true", "false", "null", `"false"`, "1", "[]", "{}"} {
		_, err := parseResponses(responseFields{"model": rawJSON("fixture"), "input": rawJSON("x"), "parallel_tool_calls": json.RawMessage(option)})
		valid := option == "true" || option == "false" || option == "null"
		if valid != (err == nil) {
			t.Errorf("parallel_tool_calls=%s: %v", option, err)
		}
	}
}

func TestResponsesIncludeValidation(t *testing.T) {
	for _, option := range []string{
		"null", "[]", `["reasoning.encrypted_content","reasoning.encrypted_content"]`,
		`["web_search_call.action.sources","web_search_call.results","file_search_call.results","message.input_image.image_url","message.output_text.logprobs","computer_call_output.output.image_url","code_interpreter_call.outputs"]`,
	} {
		_, err := parseResponses(responseFields{"model": rawJSON("fixture"), "input": rawJSON("x"), "include": json.RawMessage(option)})
		if err != nil {
			t.Errorf("include=%s: %v", option, err)
		}
	}
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	for _, option := range []string{`"reasoning.encrypted_content"`, "{}", "[null]", "[7]", `["reasoning.encrypted_content","unknown.selector"]`} {
		hit(t, a, "POST", "/v1/responses", key, `{"model":"fixture","input":"x","include":`+option+"}", "", 400)
	}
}

func TestResponsesCLI(t *testing.T) {
	if os.Getenv("TEST_CODEX_BIN") == "" {
		t.Skip("requires the pinned CLI")
	}
	for _, k := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	a := testApp(t, t.TempDir())
	var requests atomic.Int32
	mockAccount := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer sk-loopback-only" {
			t.Errorf("unexpected CLI request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		requests.Add(1)
		var b struct {
			Input         responseItems
			Store, Stream bool
			Model         string
			Instructions  string
			Tools         []json.RawMessage
			Previous      string `json:"previous_response_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
			return
		}
		if !b.Stream || b.Store || b.Previous != "" {
			t.Errorf("invalid upstream request")
		}
		latest := string(b.Input[len(b.Input)-1])
		full := string(rawJSON(b.Input))
		if strings.Contains(latest, "fixture-followup") && (!strings.Contains(full, "fixture-first") || !strings.Contains(full, "fixture-answer")) {
			t.Errorf("missing session history")
		}
		if strings.Contains(full, "fixture-tool-output") && (!strings.Contains(full, "fixture-call-id") || !strings.Contains(full, "fixture-tool-start")) {
			t.Errorf("missing call in history")
		}
		var s strings.Builder
		event := func(typ string, v M) { v["type"] = typ; fmt.Fprintf(&s, "event: %s\ndata: %s\n\n", typ, rawJSON(v)) }
		event("response.created", M{"response": M{"id": "upstream", "status": "in_progress"}})
		var calls []M
		if strings.Contains(full, "fixture-two-cities") {
			for _, city := range []string{"Oslo", "Lima"} {
				callID := "call_" + city
				matches := strings.Count(full, `"call_id":"`+callID+`"`)
				if strings.Contains(full, "weather-for-"+city) {
					if matches != 2 {
						t.Errorf("%s: expected one call and its result, got %d matching IDs", city, matches)
					}
					continue
				}
				if matches != 0 {
					t.Errorf("%s: replayed a call the client did not answer", city)
				}
				calls = append(calls, M{"type": "function_call", "id": "fc_" + city, "call_id": callID, "name": "forecast", "arguments": string(rawJSON(M{"city": city})), "status": "completed"})
			}
		} else if strings.Contains(latest, "fixture-tool-start") {
			if !strings.Contains(string(rawJSON(b.Tools)), "forecast") {
				t.Errorf("missing tool definition")
			}
			calls = append(calls, M{"type": "function_call", "id": "fc_x", "call_id": "fixture-call-id", "name": "forecast", "arguments": `{"city":"Paris"}`, "status": "completed"})
		}
		if len(calls) > 0 && !strings.Contains(b.Instructions+full, "Request at most one client function at a time") {
			t.Error("CLI did not receive the serial tool instruction")
		}
		if len(calls) == 0 {
			if strings.Contains(latest, "fixture-cli-wire") && len(b.Tools) == 0 {
				t.Errorf("CLI tool definitions missing")
			}
			index := 0
			part := M{"type": "output_text", "text": "fixture-answer", "annotations": []any{}}
			if strings.Contains(latest, "fixture-includes") {
				event("response.output_item.done", M{"output_index": index, "item": M{"type": "reasoning", "id": "rs_fixture", "encrypted_content": "fixture-opaque", "summary": []any{}}})
				part["logprobs"] = []M{{"token": "fixture-answer", "logprob": -0.1, "top_logprobs": []any{}}}
				index++
			}
			event("response.output_item.added", M{"output_index": index, "item": M{"type": "message", "id": "msg_x", "role": "assistant", "phase": "final_answer", "content": []any{}}})
			event("response.output_text.delta", M{"output_index": index, "content_index": 0, "item_id": "msg_x", "delta": "fixture-answer"})
			event("response.output_item.done", M{"output_index": index, "item": M{"id": "msg_x", "type": "message", "role": "assistant", "phase": "final_answer", "status": "completed", "content": []M{part}}})
		}
		for i, call := range calls {
			event("response.output_item.done", M{"output_index": i, "item": call})
		}
		event("response.completed", M{"response": M{"id": "upstream", "model": b.Model, "usage": M{"input_tokens": 17, "output_tokens": 101, "total_tokens": 118, "input_tokens_details": M{"cached_tokens": 4}, "output_tokens_details": M{"reasoning_tokens": 2}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, s.String())
	}))
	defer mockAccount.Close()
	writeMockProviderConfig(t, a.base.home, mockAccount.URL)
	if err := os.WriteFile(filepath.Join(a.base.home, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-loopback-only"}`), 0600); err != nil {
		t.Fatal(err)
	}
	key, kid := newKey(t, a)
	other, _ := newKey(t, a)
	const path = "/v1/responses"
	send := func(input string, options M, sid string) string {
		b := M{"model": "gpt-5.4", "input": input}
		for k, v := range options {
			b[k] = v
		}
		return hit(t, a, "POST", path, key, string(rawJSON(b)), sid, 200)
	}
	first := send("fixture-tool-start", M{
		"parallel_tool_calls": false,
		"tools":               []M{{"type": "function", "name": "forecast", "parameters": M{"type": "object"}}},
	}, "")
	id := prop(first, "id")
	if !strings.HasPrefix(id, "resp_") || !strings.Contains(first, "fixture-call-id") {
		t.Fatal(first)
	}
	payload := string(rawJSON(M{"model": "gpt-5.4", "previous_response_id": id, "input": []M{{"type": "function_call_output", "call_id": "fixture-call-id", "output": "fixture-tool-output"}}, "stream": true}))
	hit(t, a, "POST", path, other, payload, "", 404)
	sse := hit(t, a, "POST", path, key, payload, "", 200)
	if !strings.Contains(sse, "event: response.output_text.delta") || strings.Count(sse, "event: response.completed") != 1 || strings.Contains(sse, "[DONE]") {
		t.Fatal(sse)
	}
	unsaved := send("fixture-first", M{"store": false}, "")
	hit(t, a, "POST", path, key, strings.Replace(payload, id, prop(unsaved, "id"), 1), "", 404)
	sid := prop(hit(t, a, "POST", "/v1/sessions", key, `{}`, "", 201), "id")
	hit(t, a, "PUT", "/admin/keys/"+kid+"/binding", testAdmin, string(rawJSON(M{"session_id": sid})), "", 200)
	send("fixture-first", M{"store": false}, "")
	hit(t, a, "POST", "/v1/chat/completions", key, `{"messages":[{"role":"user","content":"fixture-followup"}]}`, "", 200)
	send("fixture-followup", nil, "")
	history := hit(t, a, "GET", "/v1/sessions/"+sid+"/history", key, "", "", 200)
	if !strings.Contains(history, "fixture-followup") || !strings.Contains(history, "fixture-first") {
		t.Fatal(history)
	}
	hit(t, a, "POST", path, key, payload, "sess_other", 403)
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM requests JOIN request_metrics ON requests.id=request_id WHERE status=200 AND input=17 AND output=101 AND cached=4 AND reasoning=2 AND speed>0`).Scan(&n); err != nil || n < int(requests.Load())-1 || n > int(requests.Load()) {
		t.Fatal(n, requests.Load(), err)
	}

	t.Run("optional include fields", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			fields := M{"model": "gpt-5.4", "input": "fixture-includes", "store": false, "stream": stream, "parallel_tool_calls": true,
				"include": []string{"reasoning.encrypted_content", "message.output_text.logprobs", "reasoning.encrypted_content"}}
			req := httptest.NewRequest("POST", path, bytes.NewReader(rawJSON(fields)))
			req.Header.Set("Authorization", "Bearer "+other)
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, req)
			if got := w.Result().Header.Get("X-Codex-Tunnel-Omitted-Include"); got != "reasoning.encrypted_content, message.output_text.logprobs" {
				t.Fatalf("stream=%v: omitted includes %q", stream, got)
			}
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, "fixture-answer") || strings.Contains(body, "fixture-opaque") || strings.Contains(body, `"logprobs"`) {
				t.Fatalf("stream=%v: %d %s", stream, w.Code, body)
			}
			if stream && !strings.Contains(body, "event: response.completed") || !stream && prop(body, "status") != "completed" {
				t.Fatalf("stream=%v: incomplete response: %s", stream, body)
			}
		}
	})

	t.Run("client permits parallel calls", func(t *testing.T) {
		body := M{"model": "gpt-5.4", "input": "fixture-two-cities", "parallel_tool_calls": true, "tools": []M{{"type": "function", "name": "forecast", "parameters": M{"type": "object"}}}}
		seen := map[string]bool{}
		for range 2 {
			raw := hit(t, a, "POST", path, other, string(rawJSON(body)), "", 200)
			var response struct {
				ID       string
				Parallel *bool `json:"parallel_tool_calls"`
				Output   []struct {
					Type, Arguments string
					CallID          string `json:"call_id"`
				}
			}
			if err := json.Unmarshal([]byte(raw), &response); err != nil || response.Parallel == nil || *response.Parallel || len(response.Output) != 1 {
				t.Fatalf("expected exactly one item in serial mode: %s", raw)
			}
			call := response.Output[0]
			city := prop(call.Arguments, "city")
			if call.Type != "function_call" || city != "Oslo" && city != "Lima" || call.CallID != "call_"+city || seen[city] {
				t.Fatalf("unexpected tool call: %+v", call)
			}
			seen[city] = true
			body["previous_response_id"] = response.ID
			body["input"] = []M{{"type": "function_call_output", "call_id": call.CallID, "output": "weather-for-" + city}}
		}
		body["stream"] = true
		delete(body, "tools")
		sse := hit(t, a, "POST", path, other, string(rawJSON(body)), "", 200)
		if strings.Count(sse, `"parallel_tool_calls":false`) != 3 || !strings.Contains(sse, "fixture-answer") || !strings.Contains(sse, "event: response.completed") {
			t.Fatal(sse)
		}
	})

	legacy := prop(hit(t, a, "POST", "/v1/sessions", other, `{}`, "", 201), "id")
	hit(t, a, "POST", "/v1/chat/completions", other, `{"model":"gpt-5.4","messages":[{"role":"user","content":"fixture-first"}]}`, legacy, 200)
	hit(t, a, "POST", path, other, `{"model":"gpt-5.4","input":"fixture-followup"}`, legacy, 200)
	if x := hit(t, a, "GET", "/v1/sessions/"+legacy+"/history", other, "", "", 200); prop(x, "next_cursor") != "cli:" {
		t.Fatal(x)
	}
	if x := hit(t, a, "GET", "/v1/sessions/"+legacy+"/history?cursor=cli%3A", other, "", "", 200); !strings.Contains(x, "fixture-first") {
		t.Fatal(x)
	}
	before := requests.Load()
	body := hit(t, a, "POST", "/v1/images/generations", other, `{"prompt":"draw"}`, "", 503)
	if !strings.Contains(body, "image_tool_unavailable") || requests.Load() != before {
		t.Fatal(body)
	}
}

// Hold a full wave at the upstream while a second wave queues. A per-request
// label detects wrong-thread delivery even when the two HTTP protocols interleave.
func TestMixedRouteConcurrency(t *testing.T) {
	if os.Getenv("TEST_CODEX_BIN") == "" {
		t.Skip("requires the pinned CLI")
	}
	for _, k := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	a := testApp(t, t.TempDir())
	const limit = 12
	a.initWorkers(limit)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	var active, peak atomic.Int32
	arrived := make(chan string, 2*limit)
	release := make(chan struct{})
	firstStarted := make(chan struct{})
	finishFirst := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/accounts/check") {
			jsonOut(w, 200, M{"accounts": []M{{"account_id": "loopback-test", "id": "loopback-test", "workspace_backend_origin": "https://" + r.Host, "account_routing_override": "NO_CONSTRAINT"}}})
			return
		}
		var b struct {
			Input []struct {
				Role    string
				Content []struct{ Text string }
			}
		}
		if r.URL.Path != "/v1/responses" || json.NewDecoder(r.Body).Decode(&b) != nil {
			http.Error(w, "unexpected request", 400)
			return
		}
		var label string
		for _, m := range b.Input {
			if m.Role == "user" {
				for _, v := range m.Content {
					if strings.HasPrefix(v.Text, "mixed-") {
						label = v.Text
					}
				}
			}
		}
		if label == "" {
			http.Error(w, "missing label", 400)
			return
		}
		switch label {
		case "mixed-session-first":
			close(firstStarted)
			select {
			case <-finishFirst:
			case <-ctx.Done():
				return
			}
		case "mixed-session-second":
			var hasPriorAnswer bool
			for _, m := range b.Input {
				for _, v := range m.Content {
					if m.Role == "assistant" && v.Text == "mixed-session-first" {
						hasPriorAnswer = true
					}
				}
			}
			if !hasPriorAnswer {
				t.Error("queued Responses request did not receive committed Chat history")
			}
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		arrived <- label
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []M{
			{"type": "response.created", "response": M{"id": label}},
			{"type": "response.output_item.added", "item": M{"type": "message", "id": label, "role": "assistant", "phase": "final_answer", "content": []any{}}},
			{"type": "response.output_text.delta", "item_id": label, "delta": label},
			{"type": "response.output_item.done", "item": M{"type": "message", "id": label, "role": "assistant", "phase": "final_answer", "content": []M{{"type": "output_text", "text": label}}}},
			{"type": "response.completed", "response": M{"id": label, "status": "completed", "usage": M{"input_tokens": 12, "output_tokens": 3, "total_tokens": 15}}},
		} {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], rawJSON(e))
		}
	}))
	defer up.Close()
	writeMockProviderConfig(t, a.base.home, up.URL)
	writeMockPlanLogin(t, a.base.home)
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	defer cancel()
	keys := make([]string, 2*limit)
	for i := range keys {
		keys[i], _ = newKey(t, a)
	}
	done := make(chan error, len(keys))
	request := func(key, label string, useChat bool) {
		path := "/v1/responses"
		body := M{"model": "gpt-5.4", "input": label}
		if useChat {
			path = "/v1/chat/completions"
			body = M{"model": "gpt-5.4", "messages": []M{{"role": "user", "content": label}}}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", srv.URL+path, bytes.NewReader(rawJSON(body)))
		if err != nil {
			done <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := srv.Client().Do(req)
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		var obj struct {
			Choices []struct{ Message message }
			Output  []struct{ Content []struct{ Text string } }
		}
		err = json.NewDecoder(resp.Body).Decode(&obj)
		var got string
		if !useChat && len(obj.Output) == 1 && len(obj.Output[0].Content) == 1 {
			got = obj.Output[0].Content[0].Text
		}
		if useChat && len(obj.Choices) == 1 {
			got = obj.Choices[0].Message.Content
		}
		if err != nil || resp.StatusCode != 200 || got != label {
			done <- fmt.Errorf("%s returned %d %q: %v", label, resp.StatusCode, got, err)
			return
		}
		done <- nil
	}
	for i, key := range keys {
		go request(key, fmt.Sprintf("mixed-%02d", i), i%2 != 0)
	}
	for range limit {
		select {
		case <-arrived:
		case err := <-done:
			t.Fatalf("request finished before release: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	for range keys {
		if err := waitFor(t, ctx, done); err != nil {
			t.Fatal(err)
		}
	}
	var owners int
	err := a.db.QueryRow("SELECT count(DISTINCT owner) FROM requests WHERE status=200 AND input=12 AND output=3").Scan(&owners)
	if peak.Load() != limit || owners != len(keys) || err != nil {
		t.Fatalf("peak=%d metered owners=%d err=%v", peak.Load(), owners, err)
	}
	t.Logf("%d mixed requests, peak %d, %d isolated responses", len(keys), peak.Load(), owners)
	key, kid := newKey(t, a)
	sid := prop(hit(t, a, "POST", "/v1/sessions", key, `{}`, "", 201), "id")
	hit(t, a, "PUT", "/admin/keys/"+kid+"/binding", testAdmin, string(rawJSON(M{"session_id": sid})), "", 200)
	go request(key, "mixed-session-first", true)
	waitFor(t, ctx, firstStarted)
	go request(key, "mixed-session-second", false)
	for {
		a.sessionTurns.mu.Lock()
		queued := len(a.sessionTurns.lanes[sid]) == 2
		a.sessionTurns.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(finishFirst)
	for range 2 {
		if err := waitFor(t, ctx, done); err != nil {
			t.Fatal(err)
		}
	}
	hit(t, a, "GET", "/v1/sessions/"+sid+"/history", keys[0], "", "", 404)
	t.Log("bound session serialized across routes and denied a foreign key")
}
