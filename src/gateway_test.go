package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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
	a := &app{db: db, admin: digest(testAdmin), base: runner{os.Getenv("TEST_CODEX_BIN"), home, work}}
	a.servers = newAppServers(context.Background())
	t.Cleanup(a.servers.close)
	a.initWorkers(2)
	return a
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
	a.servers.close()
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
	_, leave := a.sessionTurns.join(sid, 1)
	hit(t, a, "DELETE", "/admin/sessions/"+sid, testAdmin, "", "", 409)
	leave()
	for range cap(a.admitted) {
		a.admitted <- struct{}{}
	}
	hit(t, a, "POST", "/v1/chat/completions", key, chat, sid, 429)
	for range cap(a.admitted) {
		<-a.admitted
	}
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

func TestAdminDiagnostics(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	for _, path := range []string{"/admin/account", "/admin/quota", "/admin/memory"} {
		hit(t, a, "GET", path, key, "", "", 401)
		hit(t, a, "POST", path, testAdmin, "{}", "", 405)
	}
	var memory struct{ HeapBytes, RssBytes, PeakRssBytes, SampledAt int64 }
	out := hit(t, a, "GET", "/admin/memory", testAdmin, "", "", 200)
	if err := json.Unmarshal([]byte(out), &memory); err != nil || memory.HeapBytes <= 0 || memory.SampledAt <= 0 {
		t.Fatal(out)
	}
	if runtime.GOOS == "linux" && (memory.RssBytes <= 0 || memory.PeakRssBytes < memory.RssBytes) {
		t.Fatal(out)
	}
}

const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGPoKDcGAAK9ATNQgxqrAAAAAElFTkSuQmCC"

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
	if x, e := r.imageData("abc", "", filepath.Join(dir, "out.png")); e != nil || base64.StdEncoding.EncodeToString(x) != png {
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
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")
	var smallImage, largeImage atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/accounts/check") {
			origin := "https://" + r.Host
			jsonOut(w, 200, M{"accounts": []M{{
				"account_id": "loopback-test", "id": "loopback-test",
				"workspace_backend_origin": origin, "account_routing_override": "NO_CONSTRAINT",
			}}})
			return
		}
		if strings.Contains(r.URL.Path, "rate-limit-reset-credits") {
			if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits") {
				t.Errorf("unexpected reset credit operation: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			jsonOut(w, 200, M{"available_count": 2, "credits": []M{{
				"id": "fixture", "reset_type": "codex_rate_limits", "status": "available",
				"granted_at": "2023-11-14T22:13:20Z", "expires_at": nil, "title": "Test credit",
			}}})
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/usage") {
			jsonOut(w, 200, M{"account_id": "loopback-test", "plan_type": "plus",
				"rate_limit_reset_credits": M{"available_count": 2},
				"rate_limit": M{"allowed": true, "limit_reached": false,
					"primary_window": M{"used_percent": 42, "limit_window_seconds": 3600, "reset_after_seconds": 120, "reset_at": now() + 120}}})
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
			Model string
			Tools []struct{ Name, Type string }
			Input json.RawMessage
		}
		if e := json.NewDecoder(r.Body).Decode(&q); e != nil {
			t.Error(e)
			return
		}
		var inputs []struct {
			Role    string
			Content []struct {
				Type, Text string
				Image      string `json:"image_url"`
			}
		}
		if err := json.Unmarshal(q.Input, &inputs); err != nil {
			t.Error(err)
		}
		for _, m := range inputs {
			if m.Role != "user" {
				continue
			}
			size := 0
			images := []string{}
			for _, p := range m.Content {
				switch p.Text {
				case "Say hello":
					size = 1
				case "again":
					size = 600
				}
				if strings.Contains(p.Text, "previous answer") {
					t.Error("client replay was forwarded to an existing thread")
				}
				if p.Type == "input_image" {
					images = append(images, p.Image)
				}
			}
			if size == 0 {
				continue
			}
			wantCount := 1
			if size == 1 {
				wantCount = 2
			}
			if len(images) != wantCount {
				t.Errorf("expected %d typed images on %d pixel turn, got %d", wantCount, size, len(images))
				continue
			}
			if size == 1 && images[1] != images[0] {
				t.Error("second fixture image changed")
			}
			_, encoded, ok := strings.Cut(images[0], ",")
			if !ok {
				t.Error("expected a data URL")
				continue
			}
			cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
			if err != nil || cfg.Width != size || cfg.Height != size {
				t.Errorf("unexpected upstream image dimensions %dx%d, expected %dx%d: %v", cfg.Width, cfg.Height, size, size, err)
			} else if size == 1 {
				smallImage.Store(true)
			} else {
				largeImage.Store(true)
			}
		}
		draw := false
		for _, tool := range q.Tools {
			if tool.Name != "image_gen" {
				t.Errorf("unexpected enabled tool %s/%s", tool.Type, tool.Name)
			} else {
				draw = true
				if q.Model == "gpt-image-2" {
					t.Error("image engine was used as a thread model")
				}
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
		event("response.completed", M{"response": M{"id": "resp_1", "status": "completed", "usage": M{"input_tokens": 12, "output_tokens": 3, "total_tokens": 15, "input_tokens_details": M{"cached_tokens": 4}, "output_tokens_details": M{"reasoning_tokens": 2}}}})
	}))
	defer mock.Close()
	writeMockProviderConfig(t, a.base.home, mock.URL)
	if out := hit(t, a, "GET", "/admin/account", testAdmin, "", "", 200); !strings.Contains(out, `"account":null`) {
		t.Fatal(out)
	}
	os.WriteFile(filepath.Join(a.base.home, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-local-mock-only","auth_mode":"apikey"}`), 0600)
	if out := hit(t, a, "GET", "/admin/account", testAdmin, "", "", 200); !strings.Contains(out, `"type":"apiKey"`) {
		t.Fatal(out)
	}
	key, kid := newKey(t, a)
	sid := prop(hit(t, a, "POST", "/v1/sessions", key, `{}`, "", 201), "id")
	chat := visionPayload(t, "Say hello", false, "data:image/png;base64,"+png, "data:image/png;base64,"+png)
	out := hit(t, a, "POST", "/v1/chat/completions", key, chat, sid, 200)
	if !strings.Contains(out, `"total_tokens":15`) {
		t.Fatal(out)
	}
	a.servers.close()
	a.db.Close()
	a = testApp(t, d)
	hit(t, a, "PATCH", "/admin/diagnostics/settings", testAdmin, `{"capture_body":true}`, "", 200)
	hit(t, a, "PUT", "/admin/keys/"+kid+"/binding", testAdmin, `{"session_id":"`+sid+`"}`, "", 200)
	stream := visionPayload(t, "again", true, "data:image/png;base64,"+fixtureImage(t))
	out = hit(t, a, "POST", "/v1/chat/completions", key, stream, "", 200)
	for _, want := range []string{"hello", "data: [DONE]", `"total_tokens":15`} {
		if !strings.Contains(out, want) {
			t.Fatal(out)
		}
	}
	if !smallImage.Load() || !largeImage.Load() {
		t.Fatalf("both new images must reach the provider: small=%v large=%v", smallImage.Load(), largeImage.Load())
	}
	out = hit(t, a, "GET", "/v1/sessions/"+sid+"/history", key, "", "", 200)
	if !strings.Contains(out, "[Image]") || !strings.Contains(out, "again") || !strings.Contains(out, "Say hello") {
		t.Fatal(out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, e := a.base.models(ctx, a.servers); e != nil {
		t.Fatal(e)
	}
	// Only the loopback account reader sees this nonfunctional plan identity.
	writeMockPlanLogin(t, a.base.home)
	out = hit(t, a, "POST", "/v1/images/generations", key, `{"model":"gpt-image-2","prompt":"draw","output_format":"webp","stream":true}`, "", 200)
	if !strings.Contains(out, "event: image_generation.completed") || !strings.Contains(out, `"output_format":"webp"`) || !strings.Contains(out, `"model":"gpt-image-2"`) {
		t.Fatal(out)
	}
	if out := hit(t, a, "GET", "/admin/account", testAdmin, "", "", 200); !strings.Contains(out, `"type":"chatgpt"`) {
		t.Fatal(out)
	}
	quota := hit(t, a, "GET", "/admin/quota", testAdmin, "", "", 200)
	if !strings.Contains(quota, `"usedPercent":42`) || !strings.Contains(quota, `"availableCount":2`) || !strings.Contains(quota, `"id":"fixture"`) {
		t.Fatal(quota)
	}
	usage := hit(t, a, "GET", "/v1/usage", key, "", "", 200)
	for _, want := range []string{`"images":1`, `"requests":3`} {
		if !strings.Contains(usage, want) {
			t.Fatal(usage)
		}
	}
	var logs struct {
		Data []struct {
			Seq       int
			RequestID string `json:"request_id"`
			Direction string
		}
	}
	decode(t, hit(t, a, "GET", "/admin/diagnostics/logs?session="+sid, testAdmin, "", "", 200), &logs)
	if len(logs.Data) != 4 {
		t.Fatalf("expected two chat turns with two directions: %+v", logs)
	}
	for i, row := range logs.Data {
		var detail struct {
			Cached, Reasoning *int64
			TTFT              *int64 `json:"ttft_ms"`
			Cost              *float64
			Response          *string `json:"response_body"`
		}
		raw := hit(t, a, "GET", fmt.Sprintf("/admin/diagnostics/logs/%d", row.Seq), testAdmin, "", "", 200)
		if strings.Contains(raw, "data:image/") || strings.Contains(raw, png[:16]) {
			t.Fatal("image leaked to diagnostics")
		}
		decode(t, raw, &detail)
		if detail.Cached == nil || *detail.Cached != 4 || detail.Reasoning == nil || *detail.Reasoning != 2 || detail.TTFT == nil || detail.Cost == nil {
			t.Fatalf("invalid turn metrics: %+v", detail)
		}
		if (i >= 2) != (detail.Response == nil) {
			t.Fatalf("capture opt-in mismatch, row %d", i)
		}
		if i == 0 && (row.Direction != "ingress" || !strings.Contains(*detail.Response, "data: [DONE]")) {
			t.Fatal("missing incoming stream preview")
		}
		if i%2 == 1 && (row.Direction != "egress" || row.RequestID != logs.Data[i-1].RequestID) {
			t.Fatal("outbound request correlation failed")
		}
	}
	var stats struct {
		Summary struct {
			Requests, Cached, Reasoning int
			Priced                      int
		}
	}
	decode(t, hit(t, a, "GET", "/admin/analytics", testAdmin, "", "", 200), &stats)
	if stats.Summary.Requests != 3 || stats.Summary.Priced != 2 || stats.Summary.Cached < 8 {
		t.Fatalf("unexpected ledger metrics: %+v", stats)
	}
	// Reuse the same plan login for both text APIs; all inference hits the
	// loopback CLI provider, including Chat continuing a Responses session.
	hit(t, a, "POST", "/v1/responses", key, `{"model":"gpt-5.4","input":"continued"}`, "", 200)
	hit(t, a, "POST", "/v1/chat/completions", key, `{"messages":[{"role":"user","content":"continue"}]}`, "", 200)
	other, _ := newKey(t, a)
	if s := hit(t, a, "GET", "/v1/usage", other, "", "", 200); !strings.Contains(s, `"requests":0`) {
		t.Fatal(s)
	}

}

func writeMockProviderConfig(t *testing.T, home, baseURL string) {
	t.Helper()
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
`, baseURL, baseURL+"/v1")
	if e := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); e != nil {
		t.Fatal(e)
	}
}

func writeMockPlanLogin(t *testing.T, home string) {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	jwt := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"sub":"mock","email":"test@example.invalid","https://api.openai.com/auth":{"chatgpt_plan_type":"plus","chatgpt_account_id":"loopback-test"}}`)) + ".x"
	auth, _ := json.Marshal(M{"auth_mode": "chatgpt", "tokens": M{"id_token": jwt, "access_token": "mock-only", "refresh_token": "mock-only", "account_id": "loopback-test"}, "last_refresh": time.Now().UTC()})
	if e := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0600); e != nil {
		t.Fatal(e)
	}
}
