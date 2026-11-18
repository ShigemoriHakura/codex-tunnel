package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var semver = regexp.MustCompile(`^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$`)

func (a *app) current() (runner, error) {
	r := a.base
	if a.cliHome == "" {
		return r, nil
	}
	var v string
	e := a.db.QueryRow(`SELECT v FROM settings WHERE k='cli'`).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		return r, nil
	}
	if e != nil {
		return r, e
	}
	if !semver.MatchString(v) {
		return r, errors.New("invalid stored version")
	}
	r.bin = filepath.Join(a.cliHome, v, "node_modules/.bin/codex")
	return r, nil
}
func latest(ctx context.Context) (string, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", "https://registry.npmjs.org/@openai%2fcodex/latest", nil)
	if e != nil {
		return "", e
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	var pkg struct{ Name, Version string }
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&pkg) != nil || pkg.Name != "@openai/codex" || !semver.MatchString(pkg.Version) {
		return "", errors.New("npm registry unavailable")
	}
	return pkg.Version, nil
}
func (r runner) version(ctx context.Context) (string, error) {
	b, e := r.command(ctx, r.bin, "--version").Output()
	return strings.TrimSpace(strings.TrimPrefix(string(b), "codex-cli ")), e
}
func (a *app) cliStatus(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	run, e := a.current()
	if e != nil {
		internal(w, e)
		return
	}
	v, e := run.version(ctx)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	out := M{"installed": v, "managed": a.cliHome != ""}
	if r.URL.Query().Get("check") == "1" {
		l, e := latest(ctx)
		if e != nil {
			out["check_error"] = "无法连接 npm 官方 registry"
		} else {
			out["latest"] = l
		}
	}
	jsonOut(w, 200, out)
}
func (a *app) cliUpdate(w http.ResponseWriter, r *http.Request, _ string) {
	if a.cliHome == "" {
		fail(w, 403, "unmanaged_cli", "CLI_INSTALL_DIR not enabled")
		return
	}
	if !a.installing.TryLock() {
		fail(w, 409, "update_busy", "An update is running")
		return
	}
	defer a.installing.Unlock()
	var b struct {
		Version string
		Reset   bool
	}
	if !body(w, r, &b) {
		return
	}
	if b.Reset {
		_, e := a.db.Exec(`DELETE FROM settings WHERE k='cli'`)
		if e != nil {
			internal(w, e)
			return
		}
		jsonOut(w, 200, M{"ok": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	v, e := latest(ctx)
	if e != nil {
		fail(w, 502, "registry_error", "Cannot read official release")
		return
	}
	if b.Version != v {
		fail(w, 409, "release_changed", "Check the latest stable version first")
		return
	}
	if e = a.install(ctx, v); e != nil {
		fail(w, 502, "update_failed", "CLI installation or protocol check failed; previous version remains active")
		return
	}
	_, e = a.db.Exec(`INSERT INTO settings(k,v) VALUES('cli',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, v)
	if e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 200, M{"installed": v})
}
func (a *app) install(ctx context.Context, v string) error {
	if !semver.MatchString(v) {
		return errors.New("invalid version")
	}
	if e := os.MkdirAll(a.cliHome, 0700); e != nil {
		return e
	}
	dest := filepath.Join(a.cliHome, v)
	probe := func(dir string) error {
		r := a.base
		r.bin = filepath.Join(dir, "node_modules/.bin/codex")
		got, e := r.version(ctx)
		if e != nil || got != v {
			return fmt.Errorf("version mismatch")
		}
		if e = r.checkProtocol(ctx); e != nil {
			return e
		}
		p, e := r.open(ctx, false)
		if e == nil {
			p.close()
		}
		return e
	}
	if _, e := os.Stat(dest); e == nil {
		return probe(dest)
	}
	tmp, e := os.MkdirTemp(a.cliHome, ".install-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	npm, e := exec.LookPath("npm")
	if e != nil {
		return e
	}
	c := a.base.command(ctx, npm, "install", "--prefix", tmp, "--registry=https://registry.npmjs.org", "--ignore-scripts", "--no-audit", "--no-fund", "@openai/codex@"+v)
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "npm_config_cache=" + filepath.Join(tmp, ".npm")}
	if e = c.Run(); e != nil {
		return e
	}
	os.RemoveAll(filepath.Join(tmp, ".npm"))
	if e = probe(tmp); e != nil {
		return e
	}
	return os.Rename(tmp, dest)
}

// Fail closed if a CLI cannot explicitly disable environment access on both threads and turns.
func (r runner) checkProtocol(ctx context.Context) error {
	tmp, e := os.MkdirTemp("", "desk-schema-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	if e = r.command(ctx, r.bin, "app-server", "generate-json-schema", "--experimental", "--out", tmp).Run(); e != nil {
		return e
	}
	for _, name := range []string{"TurnStartParams", "ThreadStartParams"} {
		b, e := os.ReadFile(filepath.Join(tmp, "v2", name+".json"))
		var schema struct{ Properties map[string]json.RawMessage }
		if e != nil || json.Unmarshal(b, &schema) != nil || schema.Properties["environments"] == nil {
			return errors.New("missing environment isolation support")
		}
	}

	return nil
}
