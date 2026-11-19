package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var errNoImage = errors.New("no generated image returned")

type runner struct{ bin, home, work string }

func (r runner) command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = r.work
	c.Env = []string{"HOME=" + r.home, "CODEX_HOME=" + r.home, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	for _, k := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE"} {
		if v := os.Getenv(k); v != "" {
			c.Env = append(c.Env, k+"="+v)
		}
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	return c
}

type packet struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int
		Message string
	} `json:"error"`
}
type rpc struct {
	ctx    context.Context
	cmd    *exec.Cmd
	in     io.WriteCloser
	scan   *bufio.Scanner
	seq    int
	cancel context.CancelFunc
}

func (r runner) open(ctx context.Context, image bool) (*rpc, error) {
	ctx, cancel := context.WithCancel(ctx)
	args := []string{"app-server", "-c", `web_search="disabled"`, "-c", "agents.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false", "-c", `cli_auth_credentials_store="file"`}
	for _, f := range []string{"shell_tool", "unified_exec", "code_mode", "apps", "plugins", "hooks", "goals", "multi_agent_v2", "memories", "view_image"} {
		args = append(args, "--disable", f)
	}
	if image {
		args = append(args, "--enable", "image_generation")
	} else {
		args = append(args, "--disable", "image_generation")
	}
	c := r.command(ctx, r.bin, args...)
	in, e := c.StdinPipe()
	if e != nil {
		cancel()
		return nil, e
	}
	out, e := c.StdoutPipe()
	if e != nil {
		cancel()
		return nil, e
	}
	if e = c.Start(); e != nil {
		cancel()
		return nil, e
	}
	p := &rpc{ctx: ctx, cmd: c, in: in, scan: bufio.NewScanner(out), cancel: cancel}
	p.scan.Buffer(make([]byte, 65536), 36<<20)
	_, e = p.call("initialize", M{"clientInfo": M{"name": "desk_tunnel", "title": "Desk Tunnel", "version": "1.0.0"}, "capabilities": M{"experimentalApi": true}}, nil)
	if e == nil {
		e = p.send(M{"method": "initialized"})
	}
	if e != nil {
		p.close()
		return nil, e
	}
	return p, nil
}
func (p *rpc) close()           { p.cancel(); p.in.Close(); p.cmd.Wait() }
func (p *rpc) send(v any) error { return json.NewEncoder(p.in).Encode(v) }
func (p *rpc) next() (packet, error) {
	var v packet
	for p.scan.Scan() {
		if len(p.scan.Bytes()) == 0 {
			continue
		}
		if e := json.Unmarshal(p.scan.Bytes(), &v); e != nil {
			return v, e
		}
		if v.Method != "" && len(v.ID) > 0 {
			if e := p.send(M{"id": v.ID, "error": M{"code": -32601, "message": "Client actions are disabled"}}); e != nil {
				return v, e
			}
			v = packet{}
			continue
		}
		return v, nil
	}
	if e := p.scan.Err(); e != nil {
		return v, e
	}
	return v, io.ErrUnexpectedEOF
}
func (p *rpc) call(method string, params any, event func(packet) error) (reply json.RawMessage, err error) {
	if t := tracing(p.ctx); t != nil && t.kind == "" && method != "initialize" {
		start := time.Now()
		defer func() {
			recordHop(p.ctx, "stdio", method, start, err)
		}()
	}
	p.seq++
	seq := fmt.Sprint(p.seq)
	if e := p.send(M{"id": p.seq, "method": method, "params": params}); e != nil {
		return nil, e
	}
	for {
		v, e := p.next()
		if e != nil {
			return nil, e
		}
		if string(v.ID) == seq {
			if v.Error != nil {
				return nil, fmt.Errorf("%s: rpc error %d", method, v.Error.Code)
			}
			return v.Result, nil
		}
		if event != nil {
			if e = event(v); e != nil {
				return nil, e
			}
		}
	}
}
func (r runner) models(ctx context.Context) ([]M, error) {
	p, e := r.open(ctx, false)
	if e != nil {
		return nil, e
	}
	defer p.close()
	out := []M{}
	var cursor string
	for i := 0; i < 64; i++ {
		args := M{"limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		b, e := p.call("model/list", args, nil)
		if e != nil {
			return nil, e
		}
		var page struct {
			Data []struct {
				Model, ID, DisplayName string
				IsDefault, Hidden      bool
			}
			NextCursor string
		}
		if e = json.Unmarshal(b, &page); e != nil {
			return nil, e
		}
		for _, m := range page.Data {
			if m.Hidden {
				continue
			}
			if m.Model == "" {
				m.Model = m.ID
			}
			out = append(out, M{"id": m.Model, "object": "model", "owned_by": "openai", "name": m.DisplayName, "is_default": m.IsDefault})
		}
		cursor = page.NextCursor
		if cursor == "" {
			return out, nil
		}
	}
	return nil, errors.New("excessive model pages")
}

type counts struct {
	Input     int64  `json:"inputTokens"`
	Output    int64  `json:"outputTokens"`
	Cached    *int64 `json:"cachedInputTokens"`
	Reasoning *int64 `json:"reasoningOutputTokens"`
}
type result struct {
	firstToken  time.Time
	text, model string
	image       []byte
	imageMeta   imageMetadata
	total       counts
	metered     bool
}
type turn struct {
	thread, model  string
	input          []turnInput
	persist, image bool
	onThread       func(string, string) error
	delta          func(string) error
}

func (r runner) run(ctx context.Context, t turn) (res result, err error) {
	p, e := r.open(ctx, t.image)
	if e != nil {
		return res, e
	}
	defer p.close()
	args := M{"cwd": r.work, "sandbox": "read-only", "approvalPolicy": "never", "developerInstructions": "Answer the user's request directly. You have no local execution environment. Do not use filesystem, command, or connector tools."}
	method := "thread/start"
	if t.image {
		args["developerInstructions"] = "Generate one image using the built-in image generation tool. Do not use filesystem or command tools."
	}
	if t.model != "" {
		args["model"] = t.model
	}
	if t.thread != "" {
		method = "thread/resume"
		args["threadId"] = t.thread
		args["excludeTurns"] = true
	} else {
		args["ephemeral"] = !t.persist
		args["environments"] = []any{}
	}
	b, e := p.call(method, args, nil)
	if e != nil {
		return res, e
	}
	var start struct {
		Thread struct{ ID string }
		Model  string
	}
	if e = json.Unmarshal(b, &start); e != nil || start.Thread.ID == "" {
		return res, errors.New("invalid thread response")
	}
	thread := start.Thread.ID
	res.model = start.Model
	if t.onThread != nil {
		if e = t.onThread(thread, start.Model); e != nil {
			return res, e
		}
	}
	var turnID string
	done := false
	commentary := map[string]bool{}
	event := func(v packet) error {
		if v.Method != "item/started" && v.Method != "item/completed" && v.Method != "item/agentMessage/delta" && v.Method != "thread/tokenUsage/updated" && v.Method != "turn/completed" {
			return nil
		}
		var n struct {
			ThreadID, TurnID, ItemID, Delta string
			Item                            struct{ ID, Type, Text, Phase, Result, SavedPath, Status string }
			Turn                            struct{ ID, Status string }
			TokenUsage                      struct{ Total counts }
		}
		if e := json.Unmarshal(v.Params, &n); e != nil {
			return e
		}
		if n.ThreadID != thread || (turnID != "" && n.TurnID != "" && n.TurnID != turnID) {
			return nil
		}
		switch v.Method {
		case "item/started":
			if n.Item.Type == "agentMessage" && n.Item.Phase == "commentary" {
				commentary[n.Item.ID] = true
			}
		case "item/agentMessage/delta":
			if !commentary[n.ItemID] && n.Delta != "" && res.firstToken.IsZero() {
				res.firstToken = time.Now()
			}
			if t.delta != nil && !commentary[n.ItemID] {
				return t.delta(n.Delta)
			}
		case "thread/tokenUsage/updated":
			res.total = n.TokenUsage.Total
			res.metered = true
		case "item/completed":
			switch n.Item.Type {
			case "agentMessage":
				if n.Item.Phase != "commentary" {
					res.text = n.Item.Text
				}
			case "imageGeneration":
				if t.image && n.Item.Status != "failed" {
					res.image, err = r.imageData(thread, n.Item.Result, n.Item.SavedPath)
					return err
				}
			}
		case "turn/completed":
			if turnID != "" && n.Turn.ID != turnID {
				return nil
			}
			done = true
			if n.Turn.Status != "completed" {
				return errors.New("upstream turn " + n.Turn.Status)
			}
		}
		return nil
	}
	b, e = p.call("turn/start", M{"threadId": thread, "environments": []any{}, "input": t.input}, event)
	if e != nil {
		return res, e
	}
	var started struct{ Turn struct{ ID string } }
	if e = json.Unmarshal(b, &started); e != nil {
		return res, e
	}
	turnID = started.Turn.ID
	for !done {
		v, e := p.next()
		if e != nil {
			return res, e
		}
		if e = event(v); e != nil {
			return res, e
		}
	}
	if t.image && len(res.image) == 0 {
		return res, errNoImage
	}
	if !t.image && res.text == "" {
		return res, errors.New("no final message returned")
	}
	return res, nil
}
func (r runner) imageData(thread, inline, path string) ([]byte, error) {
	const limit = maxGeneratedImageBytes
	var data []byte
	var err error
	if inline != "" {
		if len(inline) > base64.StdEncoding.EncodedLen(limit) {
			return nil, errors.New("image too large")
		}
		data, err = base64.StdEncoding.DecodeString(inline)
	} else {
		if thread == "" || thread == "." || thread == ".." || filepath.Base(thread) != thread {
			return nil, errors.New("invalid thread")
		}
		root := filepath.Join(r.home, "generated_images", thread)
		rel, e := filepath.Rel(root, path)
		if e != nil || !filepath.IsLocal(rel) {
			return nil, errors.New("image unavailable outside thread directory")
		}
		dir, e := os.OpenRoot(root)
		if e != nil {
			return nil, e
		}
		defer dir.Close()
		f, e := dir.Open(rel)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, limit+1))
	}
	if err != nil || len(data) > limit || !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return nil, errors.New("invalid generated image")
	}
	return data, nil
}
