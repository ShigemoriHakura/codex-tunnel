package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConsoleSessionLifecycle(t *testing.T) {
	dir := t.TempDir()
	a := testApp(t, dir)
	call := func(method, path, token string, cookie *http.Cookie, console bool, expected int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if console {
			r.Header.Set("X-Desk-Console", "1")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != expected {
			t.Fatalf("%s %s: got %d, want %d", method, path, w.Code, expected)
		}
		return w
	}
	const endpoint = "/admin/session"
	state := func(cookie *http.Cookie, want bool) {
		t.Helper()
		var v struct{ Authenticated bool }
		decode(t, call("GET", endpoint, "", cookie, false, 200).Body.String(), &v)
		if v.Authenticated != want {
			t.Errorf("authenticated = %t, want %t", v.Authenticated, want)
		}
	}
	login := func(cookie *http.Cookie) *http.Cookie {
		t.Helper()
		w := call("POST", "https://console.example"+endpoint, testAdmin, cookie, true, 200)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("expected one cookie")
		}
		c := cookies[0]
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "" || c.Domain != "" || c.MaxAge != int(consoleTTL.Seconds()) || c.Expires.Before(time.Now()) || len(c.Value) != 48 {
			t.Fatal("invalid session cookie")
		}
		if strings.Contains(w.Body.String(), c.Value) {
			t.Fatal("response exposed session ID")
		}
		return c
	}
	state(nil, false)
	call("POST", endpoint, "wrong", nil, true, 401)
	first := login(nil)
	for _, prefix := range []string{"", "/codex"} {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		public := &url.URL{Scheme: "https", Host: "console.example", Path: prefix + endpoint}
		jar.SetCookies(public, []*http.Cookie{first})
		public.Path = prefix + "/admin/quota"
		if len(jar.Cookies(public)) != 1 {
			t.Fatal("browser cannot use the session behind this relay", prefix)
		}
		public.Path = prefix + "/v1/models"
		if len(jar.Cookies(public)) != 0 {
			t.Fatal("console cookie escaped the admin directory")
		}
	}
	state(first, true)
	var stored string
	if err := a.db.QueryRow("SELECT digest FROM console_sessions").Scan(&stored); err != nil || stored != a.consoleDigest(first.Value) {
		t.Fatal("invalid session lookup")
	}
	call("GET", "/admin/keys", "", first, false, 200)
	call("GET", "/admin/keys", "wrong", first, false, 401)
	call("GET", "/v1/usage", "", first, true, 401)
	call("GET", "/admin/usage", testAdmin, nil, false, 200)
	call("POST", endpoint, "", first, true, 401)
	call("DELETE", endpoint, "", first, false, 403)
	call("POST", "/admin/diagnostics/errors", "", first, false, 403)
	call("POST", "/admin/diagnostics/errors", "", first, true, 200)
	a.db.Close()
	a = testApp(t, dir)
	state(first, true)
	second := login(first)
	state(first, false)
	state(second, true)
	third := login(nil)
	w := call("DELETE", endpoint, "", second, true, 200)
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].MaxAge != -1 || c[0].Path != second.Path {
		t.Fatal("logout did not expire matching cookie")
	}
	state(second, false)
	call("GET", "/admin/usage", "", second, false, 401)
	state(third, true)
	a.admin = digest("rotated admin secret")
	state(third, false)
	a.admin = digest(testAdmin)
	if _, err := a.db.Exec("UPDATE console_sessions SET expires=?", now()); err != nil {
		t.Fatal(err)
	}
	state(third, false)
	call("DELETE", endpoint, "", third, true, 200)
	httpLogin := call("POST", endpoint, testAdmin, nil, true, 200)
	if httpLogin.Result().Cookies()[0].Secure {
		t.Fatal("direct HTTP deployment requires an HTTP cookie")
	}
}
