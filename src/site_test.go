package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSiteConfigurationAndMount(t *testing.T) {
	dir := t.TempDir()
	a := testApp(t, dir)
	key, _ := newKey(t, a)
	hit(t, a, "PUT", "/admin/site", key, `{"directory":"/dashboard"}`, "", 401)
	for _, setting := range []string{`{"origin":"ftp://example.com"}`, `{"origin":"https://name@example.com"}`, `{"origin":"https://example.com/path"}`, `{"directory":"/admin/more"}`, `{"directory":"//example.org"}`, `{"directory":"/example/../second"}`, `{"origin":"http://localhost:99999"}`} {
		hit(t, a, "PUT", "/admin/site", testAdmin, setting, "", 400)
	}
	hit(t, a, "PUT", "/admin/site", testAdmin, `{"origin":"Gateway.Example:56439","directory":"ops/codex/"}`, "", 200)
	a.db.Close()
	a = testApp(t, dir)
	conf := hit(t, a, "GET", "/admin/site", testAdmin, "", "", 200)
	if prop(conf, "origin") != "https://gateway.example:56439" || prop(conf, "directory") != "/ops/codex" {
		t.Fatal(conf)
	}
	hit(t, a, "GET", "/ops/codex?test=1", "", "", "", 307)
	for _, path := range []string{"/ops/codex/", "/ops/codex/app.js", "/ops/codex/utilities.css", "/ops/codex/locales/en.mjs", "/ops/codex/healthz"} {
		hit(t, a, "GET", path, "", "", "", 200)
	}
	hit(t, a, "GET", "/ops/codex/admin/usage", "", "", "", 401)
	hit(t, a, "GET", "/ops/codex/v1/usage", key, "", "", 200)
	hit(t, a, "GET", "/ops/codex-other/healthz", "", "", "", 404)

	cookieCall := func(method, path, payload string, c *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(payload))
		r.Header.Set("X-Desk-Console", "1")
		if c != nil {
			r.AddCookie(c)
		} else {
			r.Header.Set("Authorization", "Bearer "+testAdmin)
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d", method, path, w.Code)
		}
		return w
	}
	old := cookieCall("POST", "/ops/codex/admin/session", "", nil).Result().Cookies()[0]
	if old.Path != "" {
		t.Fatal("login cookie must inherit the browser-visible admin directory")
	}
	next := cookieCall("PUT", "/ops/codex/admin/site", `{"directory":"/new"}`, old).Result().Cookies()[0]
	if next.Path != "/new/admin" || next.Value != old.Value {
		t.Fatal("session did not follow the new mount")
	}
	cookieCall("GET", "/new/admin/usage", "", next)
	hit(t, a, "GET", "/ops/codex/healthz", "", "", "", 404)
	hit(t, a, "GET", "/healthz", "", "", "", 200)
	hit(t, a, "PUT", "/admin/site", testAdmin, `{"origin":"","directory":"/"}`, "", 200)
	hit(t, a, "GET", "/new/healthz", "", "", "", 404)
}
