package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAdmin = "admin-test-secret-for-gateway-only"

func testApp(t *testing.T, dir string) *app {
	t.Helper()
	db, e := openDB(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	home := filepath.Join(dir, "codex")
	work := filepath.Join(dir, "work")
	for _, p := range []string{home, work} {
		if e = os.MkdirAll(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	return &app{db: db, admin: digest(testAdmin), base: runner{os.Getenv("TEST_CODEX_BIN"), home, work}, slots: make(chan struct{}, 2)}
}
func hit(t *testing.T, a *app, method, path, key, payload, sid string, status int) string {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Session-ID", sid)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: %d != %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w.Body.String()
}
func prop(j, k string) string {
	var m map[string]any
	json.Unmarshal([]byte(j), &m)
	v, _ := m[k].(string)
	return v
}
func newKey(t *testing.T, a *app) (string, string) {
	j := hit(t, a, "POST", "/admin/keys", testAdmin, `{"name":"integration"}`, "", 201)
	return prop(j, "key"), prop(j, "id")
}
func TestAccessAndPersistence(t *testing.T) {
	d := t.TempDir()
	a := testApp(t, d)
	key, kid := newKey(t, a)
	other, _ := newKey(t, a)
	hit(t, a, "GET", "/admin/keys", key, "", "", 401)
	hit(t, a, "POST", "/v1/sessions", testAdmin, `{}`, "", 401)
	sid := prop(hit(t, a, "POST", "/v1/sessions", key, `{"name":"persist"}`, "", 201), "id")
	a.db.Close()
	a = testApp(t, d)
	if !strings.Contains(hit(t, a, "GET", "/v1/sessions", key, "", "", 200), sid) {
		t.Fatal("session lost")
	}
	if strings.Contains(hit(t, a, "GET", "/v1/sessions", other, "", "", 200), sid) {
		t.Fatal("session leaked")
	}
	hit(t, a, "GET", "/v1/sessions/"+sid+"/history", other, "", "", 404)
	hit(t, a, "DELETE", "/v1/sessions/"+sid, other, "", "", 404)
	chat := `{"messages":[{"role":"user","content":"hello"}]}`
	hit(t, a, "POST", "/v1/chat/completions", other, chat, sid, 404)
	a.busy.Store(sid, true)
	hit(t, a, "POST", "/v1/chat/completions", key, chat, sid, 409)
	hit(t, a, "DELETE", "/admin/sessions/"+sid, testAdmin, "", "", 409)
	a.busy.Delete(sid)
	a.slots <- struct{}{}
	a.slots <- struct{}{}
	hit(t, a, "POST", "/v1/chat/completions", key, chat, sid, 429)
	a.release()
	a.release()
	hit(t, a, "DELETE", "/v1/sessions/"+sid, key, "", "", 200)
	hit(t, a, "DELETE", "/admin/keys/"+kid, testAdmin, "", "", 200)
	hit(t, a, "GET", "/v1/usage", key, "", "", 401)
}
func TestInvalidRequests(t *testing.T) {
	a := testApp(t, t.TempDir())
	k, _ := newKey(t, a)
	for _, b := range []string{`{}`, `{"messages":[],"tools":[]}`, `{"messages":[{"role":"tool","content":"ok"}]}`, `{} {}`} {
		hit(t, a, "POST", "/v1/chat/completions", k, b, "", 400)
	}
	hit(t, a, "POST", "/v1/images/generations", k, `{"prompt":"test","response_format":"url"}`, "", 400)
	hit(t, a, "POST", "/admin/cli/update", testAdmin, `{"version":"bad"}`, "", 403)
}

const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a5c8AAAAASUVORK5CYII="

func TestImageScope(t *testing.T) {
	a := testApp(t, t.TempDir())
	r := a.base
	dir := filepath.Join(r.home, "generated_images", "abc")
	os.MkdirAll(dir, 0700)
	b, _ := base64.StdEncoding.DecodeString(png)
	os.WriteFile(filepath.Join(dir, "out.png"), b, 0600)
	os.WriteFile(filepath.Join(r.home, "other.png"), b, 0600)
	os.Symlink(filepath.Join(r.home, "other.png"), filepath.Join(dir, "link"))
	for _, p := range []string{filepath.Join(r.home, "other.png"), filepath.Join(dir, "link")} {
		if _, e := r.imageData("abc", "", p); e == nil {
			t.Fatal("unconfined file read")
		}
	}
	if x, e := r.imageData("abc", "", filepath.Join(dir, "out.png")); e != nil || x != png {
		t.Fatal(x, e)
	}
}

// Optional protocol integration: the actual pinned CLI, an empty HOME, and a local mock
// Responses/Images provider. No developer credentials or paid generation are used.
func TestCLIIntegration(t *testing.T) {
	if os.Getenv("TEST_CODEX_BIN") == "" {
		t.Skip("set TEST_CODEX_BIN to run the CLI protocol suite")
	}
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	d := t.TempDir()
	a := testApp(t, d)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/usage") {
			jsonOut(w, 200, M{"account_id": "loopback-test", "plan_type": "plus", "rate_limit": M{"allowed": true, "limit_reached": false, "primary_window": M{"used_percent": 42, "limit_window_seconds": 3600, "reset_after_seconds": 120, "reset_at": now() + 120}}})
			return
		}
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/v1/images/generations" {
			jsonOut(w, 200, M{"created": 1, "data": []M{{"b64_json": png}}})
			return
		}
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var q struct {
			Tools []struct{ Name, Type string }
			Input json.RawMessage
		}
		if e := json.NewDecoder(r.Body).Decode(&q); e != nil {
			t.Error(e)
			return
		}
		draw := false
		for _, tool := range q.Tools {
			if tool.Name != "image_gen" {
				t.Errorf("unexpected enabled tool %s/%s", tool.Type, tool.Name)
			} else {
				draw = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(k string, v M) {
			v["type"] = k
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", k, b)
		}
		event("response.created", M{"response": M{"id": "resp_1"}})
		if draw && !strings.Contains(string(q.Input), "function_call_output") {
			event("response.output_item.done", M{"item": M{"type": "function_call", "call_id": "draw_1", "namespace": "image_gen", "name": "imagegen", "arguments": `{"prompt":"test"}`}})
		} else {
			m := M{"type": "message", "id": "msg_1", "role": "assistant", "phase": "final_answer", "content": []M{{"type": "output_text", "text": "hello"}}}
			event("response.output_item.added", M{"item": M{"type": "message", "id": "msg_1", "role": "assistant", "phase": "final_answer", "content": []M{}}})
			event("response.output_text.delta", M{"item_id": "msg_1", "delta": "hello"})
			event("response.output_item.done", M{"item": m})
		}
		event("response.completed", M{"response": M{"id": "resp_1", "status": "completed", "usage": M{"input_tokens": 12, "output_tokens": 3, "total_tokens": 15}}})
	}))
	defer mock.Close()
	config := fmt.Sprintf(`model="gpt-5.4"
chatgpt_base_url=%q
model_provider="mock"
[features]
enable_request_compression=false
[model_providers.mock]
name="OpenAI"
requires_openai_auth=true
supports_websockets=false
base_url=%q
wire_api="responses"
request_max_retries=0
stream_max_retries=0
`, mock.URL, mock.URL+"/v1")
	if e := os.WriteFile(filepath.Join(a.base.home, "config.toml"), []byte(config), 0600); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(a.base.home, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-local-mock-only","auth_mode":"apikey"}`), 0600)
	key, _ := newKey(t, a)
	sid := prop(hit(t, a, "POST", "/v1/sessions", key, `{}`, "", 201), "id")
	chat := `{"messages":[{"role":"user","content":"Say hello"}]}`
	out := hit(t, a, "POST", "/v1/chat/completions", key, chat, sid, 200)
	if !strings.Contains(out, `"total_tokens":15`) {
		t.Fatal(out)
	}
	a.db.Close()
	a = testApp(t, d)
	stream := `{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"again"}]}`
	out = hit(t, a, "POST", "/v1/chat/completions", key, stream, sid, 200)
	for _, want := range []string{"hello", "data: [DONE]", `"total_tokens":15`} {
		if !strings.Contains(out, want) {
			t.Fatal(out)
		}
	}
	out = hit(t, a, "GET", "/v1/sessions/"+sid+"/history", key, "", "", 200)
	if !strings.Contains(out, "again") || !strings.Contains(out, "Say hello") {
		t.Fatal(out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, e := a.base.models(ctx); e != nil {
		t.Fatal(e)
	}
	// The pinned CLI offers the image tool only for a Codex-backend login. This
	// deliberately unsigned, non-functional identity is confined to our loopback provider.
	enc := base64.RawURLEncoding.EncodeToString
	jwt := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"sub":"mock","email":"test@example.invalid"}`)) + ".x"
	auth, _ := json.Marshal(M{"auth_mode": "chatgpt", "tokens": M{"id_token": jwt, "access_token": "mock-only", "refresh_token": "mock-only", "account_id": "loopback-test"}, "last_refresh": time.Now().UTC()})
	if e := os.WriteFile(filepath.Join(a.base.home, "auth.json"), auth, 0600); e != nil {
		t.Fatal(e)
	}
	out = hit(t, a, "POST", "/v1/images/generations", key, `{"prompt":"draw"}`, "", 200)
	if !strings.Contains(out, png) {
		t.Fatal(out)
	}
	quota := hit(t, a, "GET", "/admin/quota", testAdmin, "", "", 200)
	if !strings.Contains(quota, `"usedPercent":42`) {
		t.Fatal(quota)
	}
	usage := hit(t, a, "GET", "/v1/usage", key, "", "", 200)
	for _, want := range []string{`"images":1`, `"requests":3`} {
		if !strings.Contains(usage, want) {
			t.Fatal(usage)
		}
	}
	other, _ := newKey(t, a)
	if s := hit(t, a, "GET", "/v1/usage", other, "", "", 200); !strings.Contains(s, `"requests":0`) {
		t.Fatal(s)
	}

}
