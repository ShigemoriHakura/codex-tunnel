package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type siteAddress struct {
	Origin    string `json:"origin"`
	Directory string `json:"directory"`
}

func (a *app) siteAddress(ctx context.Context) (siteAddress, error) {
	var setting siteAddress
	var raw string
	err := a.db.QueryRowContext(ctx, "SELECT v FROM settings WHERE k='site_address'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return setting, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &setting)
	}
	return setting, err
}

var directoryPattern = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)+$`)
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func (s *siteAddress) normalize() bool {
	s.Directory = strings.TrimSuffix(strings.TrimSpace(s.Directory), "/")
	if s.Directory != "" {
		if !strings.HasPrefix(s.Directory, "/") {
			s.Directory = "/" + s.Directory
		}
		first, _, _ := strings.Cut(s.Directory[1:], "/")
		if len(s.Directory) > 240 || !directoryPattern.MatchString(s.Directory) || first == "admin" || first == "v1" || first == "healthz" || first == "locales" {
			return false
		}
	}
	s.Origin = strings.TrimSpace(s.Origin)
	if s.Origin == "" {
		return true
	}
	if !strings.Contains(s.Origin, "://") {
		s.Origin = "https://" + s.Origin
	}
	u, err := url.Parse(s.Origin)
	if err != nil || len(s.Origin) > 300 || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || (u.EscapedPath() != "" && u.EscapedPath() != "/") || strings.ContainsAny(s.Origin, "?#") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if strings.HasPrefix(u.Host, "[") && (net.ParseIP(u.Hostname()) == nil || !strings.Contains(u.Hostname(), ":")) {
		return false
	}
	if net.ParseIP(u.Hostname()) == nil {
		for _, part := range strings.Split(u.Hostname(), ".") {
			if !hostnamePattern.MatchString(part) {
				return false
			}
		}
	}
	s.Origin = u.Scheme + "://" + strings.ToLower(u.Host)
	return true
}

func (a *app) siteSettings(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method == http.MethodGet {
		setting, err := a.siteAddress(r.Context())
		if err != nil {
			internal(w, err)
			return
		}
		jsonOut(w, 200, setting)
		return
	}
	var s siteAddress
	if !body(w, r, &s) {
		return
	}
	if !s.normalize() {
		fail(w, 400, "invalid_site_address", "Invalid public origin or URL directory")
		return
	}
	raw, _ := json.Marshal(s)
	if _, err := a.db.ExecContext(r.Context(), "INSERT INTO settings(k,v) VALUES('site_address',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", string(raw)); err != nil {
		internal(w, err)
		return
	}
	// Let the administrator who changes the mount keep this login on the new path.
	if s.Directory != mountPath(r) {
		if c, err := r.Cookie(consoleCookie); err == nil {
			var expires int64
			if err := a.db.QueryRowContext(r.Context(), "SELECT expires FROM console_sessions WHERE digest=? AND expires>?", a.presentedSession(r), now()).Scan(&expires); err == nil {
				cookie := sessionCookie(r, s.Directory+"/admin")
				cookie.Value = c.Value
				cookie.Expires = time.Unix(expires, 0).UTC()
				cookie.MaxAge = max(1, int(expires-now()))
				http.SetCookie(w, cookie)
			}
		}
	}
	jsonOut(w, 200, s)
}

type mountKey struct{}

func mountPath(r *http.Request) string {
	prefix, _ := r.Context().Value(mountKey{}).(string)
	return prefix
}

// Keep the root entry available for existing clients and recovery if the public URL changes.
func (a *app) serveMount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch first {
		case "", "admin", "v1", "healthz", "locales":
			next.ServeHTTP(w, r)
			return
		}
		if strings.Contains(first, ".") {
			next.ServeHTTP(w, r)
			return
		}
		s, err := a.siteAddress(r.Context())
		if err != nil {
			internal(w, err)
			return
		}
		if s.Directory != "" {
			if r.URL.Path == s.Directory {
				target := s.Directory + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				return
			}
			if strings.HasPrefix(r.URL.Path, s.Directory+"/") {
				req := r.WithContext(context.WithValue(r.Context(), mountKey{}, s.Directory))
				http.StripPrefix(s.Directory, next).ServeHTTP(w, req)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
