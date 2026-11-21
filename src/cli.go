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
	ctx     context.Context
	cmd     *exec.Cmd
	in      io.WriteCloser
	scan    *bufio.Scanner
	seq     int
	version string
	cancel  context.CancelFunc
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
	handshake, e := p.call("initialize", M{"clientInfo": M{"name": "desk_tunnel", "title": "Codex Tunnel", "version": "1.0.0"}, "capabilities": M{"experimentalApi": true}}, nil)
	if e == nil {
		var hello struct{ UserAgent string }
		if e = json.Unmarshal(handshake, &hello); e == nil {
			product, _, _ := strings.Cut(hello.UserAgent, " ")
			_, p.version, _ = strings.Cut(product, "/")
		}
	}
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
func (p *rpc) read() (packet, error) {
	var v packet
	for p.scan.Scan() {
		if len(p.scan.Bytes()) == 0 {
			continue
		}
		if e := json.Unmarshal(p.scan.Bytes(), &v); e != nil {
			return v, e
		}
		return v, nil
	}
	if e := p.scan.Err(); e != nil {
		return v, e
	}
	return v, io.ErrUnexpectedEOF
}
func (p *rpc) next() (packet, error) {
	for {
		v, err := p.read()
		if err != nil || v.Method == "" || len(v.ID) == 0 {
			return v, err
		}
		if err := p.send(disabledAction(v)); err != nil {
			return v, err
		}
	}
}
func disabledAction(v packet) M {
	return M{"id": v.ID, "error": M{"code": -32601, "message": "Client actions are disabled"}}
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
func (r runner) models(ctx context.Context, servers *appServers) ([]M, error) {
	p, e := servers.acquire(ctx, r, false)
	if e != nil {
		return nil, e
	}
	defer servers.release(p, false)
	out := []M{}
	var cursor string
	for i := 0; i < 64; i++ {
		args := M{"limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		b, e := p.call(ctx, "model/list", args)
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
	imageModel  string
	total       counts
	metered     bool
}
type turn struct {
	thread, model  string
	input          []turnInput
	persist, image bool
	imageModel     string
	history        responseItems
	instructions   string
	effort         string
	summary        string
	verbosity      string
	outputSchema   json.RawMessage
	tools          []M
	clientTools    map[clientToolName]clientTool
	webSearch      string
	onCyberPolicy  func()
	onWebSearch    func(string, bool, json.RawMessage, string, json.RawMessage) error
	onTool         func(string, string, string, json.RawMessage) error
	onSummary      func(string, []string) error
	onSummaryDelta func(string, int, string) error
	onThread       func(string, string) error
	delta          func(string) error
}

func (r runner) run(ctx context.Context, servers *appServers, t turn) (res result, err error) {
	p, e := servers.acquire(ctx, r, t.image)
	if e != nil {
		return res, e
	}
	retire := false
	defer func() { servers.release(p, retire || errors.Is(err, errUnconfirmedRPC)) }()
	if t.image {
		switch p.version {
		case "0.170.0", "0.171.0":
			res.imageModel = pinnedImageModel
		}
		if t.imageModel != "" && t.imageModel != res.imageModel {
			return res, &responsesError{400, "invalid_image_option", "Cannot verify the requested image engine with this CLI version"}
		}
		// Both verified releases also gate their built-in tool on the login type.
		if res.imageModel == pinnedImageModel {
			raw, err := p.call(ctx, "account/read", M{"refreshToken": false})
			if err != nil {
				return res, err
			}
			var login struct{ Account *struct{ Type string } }
			if err := json.Unmarshal(raw, &login); err != nil {
				return res, err
			}
			if login.Account != nil && login.Account.Type == "apiKey" {
				return res, &responsesError{503, "image_tool_unavailable", "Codex CLI " + p.version + " does not expose its built-in image tool with a standard API-key login"}
			}
		}
		raw, err := p.call(ctx, "modelProvider/capabilities/read", M{})
		if err != nil {
			return res, err
		}
		var caps struct{ ImageGeneration bool }
		if err := json.Unmarshal(raw, &caps); err != nil {
			return res, err
		}
		if !caps.ImageGeneration {
			return res, &responsesError{503, "image_tool_unavailable", "The active CLI provider does not offer its built-in image generation tool"}
		}
	}
	args := M{"cwd": r.work, "sandbox": "read-only", "approvalPolicy": "never", "developerInstructions": "Answer the user's request directly. You have no gateway execution environment. Do not use gateway filesystem, command, or connector tools."}
	method := "thread/start"
	config := M{"web_search": "disabled"}
	if t.webSearch != "" {
		config["web_search"] = t.webSearch
	}
	if t.verbosity != "" {
		config["model_verbosity"] = t.verbosity
	}
	args["config"] = config
	if t.image {
		args["developerInstructions"] = "Generate one image using the built-in image generation tool. Do not use filesystem or command tools."
	}
	if t.instructions != "" {
		args["developerInstructions"] = args["developerInstructions"].(string) + "\n" + t.instructions
	}
	if len(t.tools) > 0 {
		args["dynamicTools"] = t.tools
		args["developerInstructions"] = args["developerInstructions"].(string) + "\nThe declared client functions run on the requesting client. Request at most one client function at a time. Wait for its result before requesting another."
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
	b, e := p.call(ctx, method, args)
	if e != nil {
		return res, e
	}
	var start struct {
		Thread struct{ ID string }
		Model  string
	}
	if e = json.Unmarshal(b, &start); e != nil || start.Thread.ID == "" {
		return res, errUnconfirmedRPC
	}
	thread := start.Thread.ID
	inbox, e := p.subscribe(thread, t.onTool != nil, t.onSummaryDelta != nil)
	if e != nil {
		return res, e
	}
	var turnID string
	done := false
	defer func() {
		if errors.Is(err, errCyberPolicy) && t.onCyberPolicy != nil {
			t.onCyberPolicy()
		}
		if !p.finishThread(thread, turnID, done, inbox) {
			retire = true
		}
	}()
	if e = ctx.Err(); e != nil {
		return res, e
	}
	res.model = start.Model
	if t.onThread != nil {
		if e = t.onThread(thread, start.Model); e != nil {
			return res, e
		}
	}
	commentary := map[string]bool{}
	event := func(v packet) error {
		var n struct {
			ThreadID, TurnID, ItemID, Delta string
			SummaryIndex                    int
			Error                           cliTurnError
			Reason                          string
			Item                            struct {
				ID, Type, Text, Phase, Result, SavedPath, Status string
				Summary                                          []string
				Action                                           json.RawMessage
				Query                                            string
				Results                                          json.RawMessage
			}
			Turn struct {
				ID, Status string
				Error      cliTurnError
			}
			TokenUsage struct{ Total counts }
		}
		if e := json.Unmarshal(v.Params, &n); e != nil {
			return e
		}
		if n.ThreadID != thread || (n.TurnID != "" && n.TurnID != turnID) {
			return nil
		}
		switch v.Method {
		case "error":
			if n.TurnID == turnID && n.Error.isCyberPolicy() {
				return errCyberPolicy
			}
		case "model/rerouted":
			if n.TurnID == turnID && n.Reason == "highRiskCyberActivity" {
				return errCyberPolicy
			}
		case "item/reasoning/summaryTextDelta":
			if t.onSummaryDelta != nil && n.TurnID == turnID && n.Delta != "" {
				return t.onSummaryDelta(n.ItemID, n.SummaryIndex, n.Delta)
			}
		case "item/started":
			if n.Item.Type == "webSearch" && t.onWebSearch != nil && n.TurnID == turnID {
				return t.onWebSearch(n.Item.ID, false, n.Item.Action, n.Item.Query, nil)
			}
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
			case "webSearch":
				if t.onWebSearch != nil && n.TurnID == turnID {
					return t.onWebSearch(n.Item.ID, true, n.Item.Action, n.Item.Query, n.Item.Results)
				}
			case "reasoning":
				if t.onSummary != nil && n.TurnID == turnID {
					return t.onSummary(n.Item.ID, n.Item.Summary)
				}
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
			if n.Turn.ID != turnID {
				return nil
			}
			done = true
			if n.Turn.Status != "completed" {
				return cliTurnFailure(n.Turn.Status, n.Turn.Error)
			}
		}
		return nil
	}
	if len(t.history) != 0 {
		if _, e = p.call(ctx, "thread/inject_items", M{"threadId": thread, "items": t.history}); e != nil {
			return res, e
		}
	}
	params := M{"threadId": thread, "environments": []any{}, "input": t.input}
	if t.effort != "" {
		params["effort"] = t.effort
	}
	if t.summary != "" {
		params["summary"] = t.summary
	}
	if t.outputSchema != nil {
		params["outputSchema"] = t.outputSchema
	}
	b, e = p.call(ctx, "turn/start", params)
	if e != nil {
		return res, e
	}
	var started struct{ Turn struct{ ID string } }
	if e = json.Unmarshal(b, &started); e != nil || started.Turn.ID == "" {
		return res, errUnconfirmedRPC
	}
	turnID = started.Turn.ID
	for !done {
		v, e := inbox.next(ctx, p.ctx)
		if e != nil {
			return res, e
		}
		if v.Method == "item/tool/call" && t.onTool != nil {
			var call struct {
				ThreadID, TurnID, CallID, Namespace, Tool string
				Arguments                                 json.RawMessage
			}
			if json.Unmarshal(v.Params, &call) != nil || call.ThreadID != thread || call.TurnID != turnID || call.CallID == "" || call.Tool == "" {
				return res, errors.New("invalid CLI tool request")
			}
			// The HTTP client executes its function. finishThread interrupts the pending
			// CLI turn, and the next request replays the call and result together.
			return res, t.onTool(call.CallID, call.Namespace, call.Tool, call.Arguments)
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

type cliTurnError struct {
	Info    json.RawMessage `json:"codexErrorInfo"`
	Message string
}

type providerErrorDetail struct{ Code, Type, Param, Message string }

// Some upstream HTTP and WebSocket errors arrive as "other", with the
// provider's JSON error envelope in TurnError.message. Never expose its body.
func (e cliTurnError) providerError() providerErrorDetail {
	var envelope struct{ Error providerErrorDetail }
	if json.Unmarshal([]byte(e.Message), &envelope) != nil {
		return providerErrorDetail{}
	}
	return envelope.Error
}

func (e cliTurnError) isCyberPolicy() bool {
	return string(e.Info) == `"cyberPolicy"` || e.providerError().Code == "cyber_policy"
}

func cliTurnFailure(status string, detail cliTurnError) error {
	if detail.isCyberPolicy() {
		return errCyberPolicy
	}
	provider := detail.providerError()
	if (string(detail.Info) == `"other"` || len(detail.Info) == 0 || string(detail.Info) == "null") && provider.Type == "invalid_request_error" {
		if provider.Code == "unsupported_parameter" && provider.Param == "input" && provider.Message == "This request does not support user-supplied input item IDs." {
			return invalidResponse("The provider rejected a supplied input item ID in the replayed history")
		}
		return invalidResponse("The CLI provider rejected the request parameters")
	}
	var transport map[string]struct {
		Status int `json:"httpStatusCode"`
	}
	_ = json.Unmarshal(detail.Info, &transport)
	for _, kind := range []string{"httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts"} {
		if transport[kind].Status == 400 {
			return invalidResponse("The CLI provider rejected the request (HTTP 400)")
		}
	}
	var code string
	_ = json.Unmarshal(detail.Info, &code)
	switch code {
	case "usageLimitExceeded", "rateLimitExceeded", "sessionBudgetExceeded":
		return &responsesError{429, "rate_limit_exceeded", "The CLI reported an upstream usage limit"}
	case "contextWindowExceeded":
		return &responsesError{400, "context_length_exceeded", "The conversation exceeds the model context window"}
	case "badRequest", "invalidPrompt":
		return &responsesError{400, "invalid_request", "The CLI provider rejected the request"}
	case "modelNotFound":
		return &responsesError{404, "model_not_found", "The CLI provider could not find the requested model"}
	case "serverOverloaded", "flexUnavailable", "policyCapacityUnavailable":
		return &responsesError{503, "upstream_unavailable", "The CLI provider is temporarily unavailable"}
	}
	return errors.New("upstream turn " + status)
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
