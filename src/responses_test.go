package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func TestResponsesStreamTermination(t *testing.T) {
	const completed = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
	for _, tc := range []struct{ name, media, wire, code string }{
		{"declared SSE", "text/event-stream; charset=utf-8", "data: {\"type\":\"response.incomplete\",\r\ndata: \"response\":{\"output\":[],\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\r\n\r\n", ""},
		{"missing workspace header", "", completed, ""},
		{"proxy default", "text/plain; charset=utf-8", "\xef\xbb\xbf\r\n: keep-alive\r\n\r\n" + completed, ""},
		{"binary default", "application/octet-stream", completed, ""},
		{"HTML page without header", "", "<!DOCTYPE html>\n", "responses_protocol_error"},
		{"JSON instead of SSE", "application/json", `{"type":"response.completed"}`, "responses_protocol_error"},
		{"truncated stream", "", "data: {\"type\":\"response.created\",\"response\":{}}\n\n", "responses_stream_incomplete"},
		{"missing response", "text/event-stream", "data: [DONE]\n\n", "responses_protocol_error"},
		{"upstream rejection", "", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n", "rate_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.wire))}
			if tc.media != "" {
				resp.Header.Set("Content-Type", tc.media)
			}
			out := responseStream{reply: &generationResponse{w: httptest.NewRecorder()}, id: "resp_test"}
			err := out.read(resp)
			if tc.code == "" {
				if err != nil || out.terminal == nil {
					t.Fatalf("no terminal event: %v", err)
				}
				return
			}
			var failure *responsesError
			if !errors.As(err, &failure) || failure.code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
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
	mockAccount := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/accounts/check") {
			jsonOut(w, 200, M{"accounts": []M{{"account_id": "loopback-test", "id": "loopback-test", "workspace_backend_origin": "https://chatgpt.com", "account_routing_override": "us"}}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		// The legacy thread calls the loopback HTTP provider directly.
		req := r.Clone(r.Context())
		req.URL.Scheme = "https"
		req.URL.Host = "api.openai.com"
		resp, err := a.workspaceTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		io.Copy(w, resp.Body)
	}))
	defer mockAccount.Close()
	writeMockProviderConfig(t, a.base.home, mockAccount.URL)
	if err := os.WriteFile(filepath.Join(a.base.home, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-loopback-only"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	a.workspaceTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.String() != "https://api.openai.com/v1/responses" || r.Header.Get("Authorization") != "Bearer sk-loopback-only" || r.Header.Get("Cookie") != "" {
			return nil, fmt.Errorf("unexpected Responses destination or authorization")
		}
		var b struct {
			Input         responseItems
			Store, Stream bool
			Model         string
			Tools         []json.RawMessage
			Previous      string `json:"previous_response_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		if !b.Stream || b.Store || b.Previous != "" {
			return nil, fmt.Errorf("invalid upstream request")
		}
		latest := string(b.Input[len(b.Input)-1])
		full := string(rawJSON(b.Input))
		if strings.Contains(latest, "fixture-followup") && (!strings.Contains(full, "fixture-first") || !strings.Contains(full, "fixture-answer")) {
			return nil, fmt.Errorf("missing session history")
		}
		if strings.Contains(latest, "fixture-tool-output") && (!strings.Contains(full, "fixture-call-id") || !strings.Contains(full, "fixture-tool-start")) {
			return nil, fmt.Errorf("missing call in history")
		}
		item := M{"id": "msg_x", "type": "message", "role": "assistant", "phase": "final_answer", "status": "completed", "content": []M{{"type": "output_text", "text": "fixture-answer", "annotations": []any{}}}}
		var s strings.Builder
		event := func(typ string, v M) { v["type"] = typ; fmt.Fprintf(&s, "event: %s\ndata: %s\n\n", typ, rawJSON(v)) }
		event("response.created", M{"response": M{"id": "upstream", "status": "in_progress"}})
		if strings.Contains(latest, "fixture-tool-start") {
			if len(b.Tools) != 1 {
				return nil, fmt.Errorf("missing tool definition")
			}
			item = M{"type": "function_call", "id": "fc_x", "call_id": "fixture-call-id", "name": "forecast", "arguments": `{"city":"Paris"}`, "status": "completed"}
		} else {
			if strings.Contains(latest, "fixture-cli-wire") && len(b.Tools) == 0 {
				return nil, fmt.Errorf("CLI tool definitions missing")
			}
			event("response.output_item.added", M{"output_index": 0, "item": M{"type": "message", "id": "msg_x", "role": "assistant", "phase": "final_answer", "content": []any{}}})
			event("response.output_text.delta", M{"output_index": 0, "content_index": 0, "item_id": "msg_x", "delta": "fixture-answer"})
		}
		event("response.output_item.done", M{"output_index": 0, "item": item})
		event("response.completed", M{"response": M{"id": "upstream", "model": b.Model, "usage": M{"input_tokens": 17, "output_tokens": 101, "total_tokens": 118, "input_tokens_details": M{"cached_tokens": 4}, "output_tokens_details": M{"reasoning_tokens": 2}}}})
		// Workspace HTTP/2 responses can omit Content-Type on a successful stream.
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(s.String())), Request: r}, nil
	})
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
	first := send("fixture-tool-start", M{"tools": []M{{"type": "function", "name": "forecast", "parameters": M{"type": "object"}}}}, "")
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
	if err := a.db.QueryRow(`SELECT count(*) FROM requests JOIN request_metrics ON requests.id=request_id WHERE status=200 AND input=17 AND output=101 AND cached=4 AND reasoning=2 AND speed>0`).Scan(&n); err != nil || n != int(requests.Load()) {
		t.Fatal(n, requests.Load(), err)
	}

	// Exercise an actual Codex client with its normal tools, role-tagged input
	// and streamed events. Its home, credentials and both servers are isolated.
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	home := t.TempDir()
	writeMockProviderConfig(t, home, srv.URL)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), rawJSON(M{"OPENAI_API_KEY": other, "auth_mode": "apikey"}), 0600); err != nil {
		t.Fatal(err)
	}
	client := runner{bin: a.base.bin, home: home, work: home}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := client.command(ctx, client.bin, "exec", "--skip-git-repo-check", "--sandbox", "read-only", "fixture-cli-wire")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "fixture-answer") {
		t.Fatalf("loopback CLI: %v %s", err, output)
	}
	legacy := prop(hit(t, a, "POST", "/v1/sessions", other, `{}`, "", 201), "id")
	hit(t, a, "POST", "/v1/chat/completions", other, `{"model":"gpt-5.4","messages":[{"role":"user","content":"fixture-first"}]}`, legacy, 200)
	hit(t, a, "POST", path, other, `{"model":"gpt-5.4","input":"fixture-followup"}`, legacy, 200)
	if x := hit(t, a, "GET", "/v1/sessions/"+legacy+"/history", other, "", "", 200); prop(x, "next_cursor") != "cli:" {
		t.Fatal(x)
	}
	if x := hit(t, a, "GET", "/v1/sessions/"+legacy+"/history?cursor=cli%3A", other, "", "", 200); !strings.Contains(x, "fixture-first") {
		t.Fatal(x)
	}
	enc := base64.RawURLEncoding.EncodeToString
	jwt := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"sub":"mock","https://api.openai.com/auth":{"chatgpt_plan_type":"plus","chatgpt_account_id":"loopback-test"}}`)) + ".x"
	auth := M{"auth_mode": "chatgpt", "tokens": M{"id_token": jwt, "access_token": "workspace-fixture", "refresh_token": "unused", "account_id": "loopback-test"}, "last_refresh": time.Now().UTC()}
	if err := os.WriteFile(filepath.Join(a.base.home, "auth.json"), rawJSON(auth), 0600); err != nil {
		t.Fatal(err)
	}
	a.workspaceTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com"+responsesPath || r.Header.Get("Authorization") != "Bearer workspace-fixture" || r.Header.Get("ChatGPT-Account-Id") != "loopback-test" || r.Header.Get("x-openai-account-routing-override") != "us" {
			return nil, fmt.Errorf("unexpected workspace route or credential")
		}
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`)), Request: r}, nil
	})
	if x := hit(t, a, "POST", path, other, `{"model":"gpt-5.4","input":"workspace"}`, "", 429); !strings.Contains(x, "rate_limit_exceeded") {
		t.Fatal(x)
	}

}
