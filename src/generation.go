package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model            string        `json:"model"`
	Messages         []chatMessage `json:"messages"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"` // Client metadata, excluded from the Codex turn and diagnostics.
	Stream           bool          `json:"stream"`
	StreamOptions    *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}
type imageRequest struct {
	imageOutput
	Model            string `json:"model,omitempty"`
	Prompt           string `json:"prompt"`
	N                *int   `json:"n,omitempty"`
	ResponseFormat   string `json:"response_format,omitempty"`
	Size             string `json:"size,omitempty"`
	Quality          string `json:"quality,omitempty"`
	Background       string `json:"background,omitempty"`
	Moderation       string `json:"moderation,omitempty"`
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	User             string `json:"user,omitempty"`
	Stream           bool   `json:"stream,omitempty"`
	PartialImages    *int   `json:"partial_images,omitempty"`
}

func (a *app) generate(w http.ResponseWriter, r *http.Request, owner string) {
	start := time.Now()
	image := r.URL.Path == "/v1/images/generations"
	responses := r.URL.Path == "/v1/responses"
	var native *responsesRequest
	var chat *responsesRequest
	sid := r.Header.Get("X-Session-ID")
	binding := boundSession(r)
	if !image && binding != "" {
		if sid != "" && sid != binding {
			fail(w, 403, "session_fixed", "Session ID differs from this key’s fixed session")
			return
		}
		sid = binding
	}
	t := turn{image: image, persist: sid != ""}
	var output imageOutput
	var imageParams imageRequest
	reply := generationResponse{w: w, image: image, responses: responses}
	stream, includeUsage := false, false
	kind := "chat"
	rid := id("chatcmpl-")
	if responses {
		rid = id("resp_")
	}
	if image {
		rid = id("img_")
	}
	trace := tracing(r.Context())
	if trace != nil {
		trace.id = rid
		if len(sid) <= 200 {
			trace.session = sid
		}
	}
	w.Header().Set("X-Request-ID", rid)
	select {
	case a.admitted <- struct{}{}:
		defer func() { <-a.admitted }()
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, 429, "busy", "The generation queue is full")
		return
	}
	if image {
		kind = "image"
		b := &imageParams
		if !body(w, r, b) {
			return
		}
		if trace != nil && trace.capture {
			trace.reqBody = preview(b)
		}
		if strings.TrimSpace(b.Prompt) == "" || (b.N != nil && *b.N != 1) || (b.ResponseFormat != "" && b.ResponseFormat != "b64_json") || sid != "" {
			fail(w, 400, "invalid_request", "Use a prompt, n=1, b64_json and no session for image generation")
			return
		}
		stream = b.Stream
		if err := b.cliOptions(); err != nil {
			fail(w, 400, "invalid_image_option", err.Error())
			return
		}
		output = b.imageOutput
		if err := output.validate(); err != nil {
			fail(w, 400, "invalid_image_output", err.Error())
			return
		}
		b.imageOutput = output
		t.model = b.Model
		if b.Model == pinnedImageModel {
			t.imageModel, t.model = b.Model, ""
		}
		t.input = []turnInput{{Type: "text", Text: b.Prompt}}

	} else if responses {
		var fields responseFields
		if !boundedBody(w, r, &fields, chatBodyLimit) {
			return
		}
		var err error
		native, err = parseResponses(fields)
		if err != nil {
			fail(w, 400, "invalid_request", err.Error())
			return
		}
		t.model, stream = native.model, native.stream
		if len(native.omittedIncludes) > 0 {
			w.Header().Set("X-Codex-Tunnel-Omitted-Include", strings.Join(native.omittedIncludes, ", "))
		}
		if trace != nil && trace.capture {
			trace.reqBody = preview(M{"model": t.model, "input_items": len(native.input), "stream": stream})
		}
	} else {
		var b chatRequest
		if !boundedBody(w, r, &b, chatBodyLimit) {
			return
		}
		if len(b.Messages) < 1 || len(b.Messages) > 256 || b.Messages[len(b.Messages)-1].Role != "user" {
			fail(w, 400, "invalid_messages", "Provide 1–256 messages ending with a user message")
			return
		}
		if sid != "" && len(b.Messages) != 1 && boundSession(r) == "" {
			fail(w, 400, "invalid_messages", "Send only the new user message in session mode")
			return
		}
		var err error
		chat, err = chatResponseRequest(b.Messages, b.Model)
		if err == nil {
			err = t.responseInput(chat.input)
		}
		if err != nil {
			fail(w, 400, "invalid_messages", err.Error())
			return
		}
		t.model = b.Model
		stream = b.Stream
		includeUsage = b.StreamOptions != nil && b.StreamOptions.IncludeUsage
		if trace != nil && trace.capture {
			trace.reqBody = preview(M{"model": b.Model, "messages": responseMessages(chat.input), "stream": b.Stream})
		}

	}
	if !image && isImageEngine(t.model) {
		fail(w, 400, "invalid_model", "Use /v1/images/generations with a supported CLI image engine")
		return
	}
	if len(t.model) > 200 {
		fail(w, 400, "invalid_model", "Model name too long")
		return
	}
	if trace != nil {
		trace.model = t.model
		trace.stream = &stream
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	reply.jsonBody = !stream
	stopHeartbeat := func() {}
	reject := func(status int, code, message string) {
		stopHeartbeat()
		reply.reject(status, code, message)
	}
	var prior counts
	var pinnedModel string
	readSession := func() bool {
		err := a.db.QueryRowContext(ctx, `SELECT s.thread,s.model,s.last_in,s.last_out,m.cached,m.reasoning FROM sessions s LEFT JOIN session_metrics m ON s.id=m.session WHERE s.id=? AND (s.owner=? OR EXISTS(SELECT 1 FROM key_bindings b WHERE b.key_id=? AND b.session=s.id))`, sid, owner, owner).Scan(&t.thread, &pinnedModel, &prior.Input, &prior.Output, &prior.Cached, &prior.Reasoning)
		switch {
		case ctx.Err() != nil:
			reject(errorStatus(ctx, ctx.Err()), "request_interrupted", "Request interrupted while loading the session")
		case errors.Is(err, sql.ErrNoRows):
			reject(404, "not_found", "Session not found")
		case err != nil:
			log.Printf("load session: %v", err)
			reject(500, "storage_error", "Gateway storage error")
		case pinnedModel != "" && t.model != "" && pinnedModel != t.model:
			reject(400, "model_mismatch", "Create a new session to switch models")
		default:
			return true
		}
		return false
	}
	var ready <-chan struct{}
	var refreshSession bool
	if sid != "" {
		var leave func()
		ready, leave = a.sessionTurns.join(sid, cap(a.admitted)-cap(a.slots))
		if leave == nil {
			w.Header().Set("Retry-After", "2")
			fail(w, 429, "busy", "The session queue is full")
			return
		}
		defer leave()
		select {
		case <-ready:
		default:
			refreshSession = true
		}
		if !readSession() {
			return
		}
	}
	if stream || a.jsonKeepalive {
		stopHeartbeat = reply.startHeartbeats(ctx, cancel, 3*time.Second)
		defer stopHeartbeat()
	}
	if err := a.waitForWorker(ctx, ready); err != nil {
		if errors.Is(err, context.Canceled) {
			reject(499, "request_canceled", "Request canceled while waiting for a worker")
		} else {
			reject(504, "queue_timeout", "Request timed out in the generation queue")
		}
		return
	}
	releaseWorker := sync.OnceFunc(a.release)
	defer releaseWorker()
	run, e := a.current()
	if e != nil {
		reject(500, "storage_error", "Could not load the CLI configuration")
		return
	}
	if sid != "" {
		// The preceding turn may have created the thread or changed its cumulative usage.
		if refreshSession && !readSession() {
			return
		}
		if t.thread != "" && binding != "" {
			t.history = nil // A bound client may replay the full conversation on each request.
		}
		if pinnedModel != "" {
			t.model = pinnedModel
		}
		t.onThread = func(thread, model string) error {
			_, e := a.db.Exec(`UPDATE sessions SET thread=?,model=?,updated=? WHERE id=?`, thread, model, now(), sid)
			return e
		}
	}
	var head string
	if sid != "" {
		head, e = a.responseHead(ctx, sid)
		if e != nil {
			reject(500, "storage_error", "Could not load session history")
			return
		}
		if head != "" && !responses {
			native = chat
			native.model = t.model
			if binding != "" && len(native.input) > 1 {
				native.input = native.input[len(native.input)-1:]
			}
		}
	}
	var contextItems responseItems
	if native != nil {
		contextItems, e = a.responseContext(ctx, native, owner, sid, head)
		if e == nil && t.thread != "" {
			var prefix responseItems
			prefix, e = a.nativeResponseHistory(ctx, run, t.thread)
			if e == nil {
				if head == "" && native.previous == "" {
					native.input = unreplayedInput(prefix, contextItems)
					contextItems = native.input
				}
				contextItems = append(prefix, contextItems...)
			}
		}
		if e != nil {
			var bad *responsesError
			if errors.As(e, &bad) {
				reject(bad.status, bad.code, bad.message)
			} else {
				log.Printf("load Responses history: %v", e)
				reject(errorStatus(ctx, e), "history_error", "Could not load Responses history")
			}
			return
		}
	}
	opened := false
	model := t.model
	chunk := func(delta M, finish any) M {
		return M{"id": rid, "object": "chat.completion.chunk", "created": start.Unix(), "model": model, "choices": []M{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	send := func(v any) error { return reply.write("", v) }
	if stream && !image && !responses {
		bind := t.onThread
		t.onThread = func(thread, m string) error {
			model = m
			if bind != nil {
				return bind(thread, m)
			}
			return nil
		}
		t.delta = func(d string) error {
			if !opened {
				opened = true
				if e := send(chunk(M{"role": "assistant"}, nil)); e != nil {
					return e
				}
			}
			return send(chunk(M{"content": d}, nil))
		}
	}
	upstreamStart := time.Now()
	var res result
	var runErr error
	outputStream := responseStream{id: rid, stream: responses && stream, reply: &reply, model: t.model, created: start.Unix()}
	if native != nil {
		outputStream.store = native.store
		outputStream.previous = native.previous
		res, runErr = a.runResponses(ctx, run, native, contextItems, &outputStream, t.delta)
		prior = counts{} // Each Responses request replays its owned context in an ephemeral CLI thread.
	} else {
		res, runErr = run.run(ctx, a.servers, t)
		if runErr == nil && image {
			res.image, runErr = output.encode(res.image)
		}
	}
	releaseWorker()
	stopHeartbeat()
	ended := time.Now()
	if res.model != "" {
		model = res.model
	}
	used := counts{}
	if res.metered {
		used.Input = res.total.Input - prior.Input
		used.Output = res.total.Output - prior.Output
		if used.Input < 0 || used.Output < 0 {
			used = res.total
		}
	}
	// Upstream can finish at the same moment the downstream proxy disconnects.
	hopErr := runErr
	if runErr == nil {
		runErr = ctx.Err()
	}
	status := errorStatus(ctx, runErr)
	if trace != nil {
		trace.model = model
		recordHop(ctx, "stdio", "codex/app-server/turn", upstreamStart, hopErr)
		if trace.capture {
			safe := make([]turnInput, len(t.input))
			copy(safe, t.input)
			for i := range safe {
				if safe[i].Type == "image" {
					safe[i].URL = "[image omitted]"
				}
			}
			if native != nil {
				trace.outBody = preview(M{"model": t.model, "input_items": len(contextItems), "stream": true})
			} else {
				trace.outBody = preview(M{"model": t.model, "thread": t.thread, "input": safe})
			}
			if image {
				trace.resBody = preview(M{"generated": len(res.image) > 0})
			} else {
				trace.resBody = preview(M{"text": res.text})
			}
		}
		if runErr != nil {
			trace.status = status
			trace.code = "upstream_error"
			trace.message = "Codex turn failed"
			if status == 499 {
				trace.code, trace.message = "request_canceled", "Request canceled before completion"
			}
		}
	}
	used.Cached = counterDelta(res.total.Cached, prior.Cached, prior.Input == 0 || res.total.Input < prior.Input)
	used.Reasoning = counterDelta(res.total.Reasoning, prior.Reasoning, prior.Output == 0 || res.total.Output < prior.Output)
	var ttft *int64
	if !res.firstToken.IsZero() {
		v := res.firstToken.Sub(start).Milliseconds()
		ttft = &v
	}
	var speed *float64
	if res.metered && used.Output >= 100 {
		elapsed := ended.Sub(start)
		if !res.firstToken.IsZero() && ended.Sub(res.firstToken) >= 20*time.Millisecond {
			elapsed = ended.Sub(res.firstToken)
		}
		if elapsed > 0 {
			v := float64(used.Output) / elapsed.Seconds()
			speed = &v
		}
	}
	account := accountLabel(run.home)
	tx, e := a.db.Begin()
	if e == nil {
		defer tx.Rollback()
		_, e = tx.Exec(`INSERT INTO requests VALUES(?,?,?,?,?,?,?,?,?,?,?)`, rid, owner, sid, kind, model, status, used.Input, used.Output, res.metered, ended.Sub(start).Milliseconds(), start.Unix())
		if e == nil {
			_, e = tx.Exec("INSERT INTO request_metrics VALUES(?,?,?,?,?,?,?)", rid, used.Cached, used.Reasoning, ttft, speed, estimateCost(model, kind, used), account)
		}
		if e == nil && native != nil && runErr == nil {
			e = native.save(tx, sid, owner, rid, &outputStream)
		}
		if e == nil && native == nil && sid != "" && res.metered {
			_, e = tx.Exec(`UPDATE sessions SET last_in=?,last_out=?,updated=? WHERE id=?`, res.total.Input, res.total.Output, now(), sid)
			if e == nil {
				_, e = tx.Exec("INSERT INTO session_metrics VALUES(?,?,?) ON CONFLICT(session) DO UPDATE SET cached=excluded.cached,reasoning=excluded.reasoning", sid, res.total.Cached, res.total.Reasoning)
			}
		}
		if e == nil {
			e = tx.Commit()
		}
	}
	if e != nil {
		log.Printf("save generation usage: %v", e)
		reply.reject(500, "storage_error", "Could not save request usage")
		return
	}
	if runErr != nil {
		status, code, message := upstreamFailure(ctx, runErr)
		reply.reject(status, code, message)
		return
	}
	// Preserve upstream usage while recording delivery failures separately.
	var deliveryErr error
	defer func() {
		if deliveryErr == nil {
			return
		}
		if trace != nil {
			trace.status, trace.code, trace.message = 499, "delivery_failed", "Connection closed while delivering the response"
		}
		if _, err := a.db.Exec("UPDATE requests SET status=499 WHERE id=?", rid); err != nil {
			log.Printf("record delivery failure: %v", err)
		}
	}()
	if responses {
		if stream {
			deliveryErr = outputStream.send("response.completed", M{"response": outputStream.final})
		} else {
			deliveryErr = reply.finishJSON(outputStream.final)
		}
		return
	}
	if image {
		if stream {
			deliveryErr = reply.write(imageCompleted, imageParams.completedEvent(res))
		} else {
			payload := M{"created": start.Unix(), "output_format": output.Format, "data": []M{{"b64_json": res.image}}}
			if res.imageModel != "" {
				payload["model"] = res.imageModel
			}
			deliveryErr = reply.finishJSON(payload)
		}
		return
	}
	var usage any
	if res.metered {
		usage = M{"prompt_tokens": used.Input, "completion_tokens": used.Output, "total_tokens": used.Input + used.Output}
	}
	if stream {
		if !opened {
			if deliveryErr = t.delta(res.text); deliveryErr != nil {
				return
			}
		}
		if deliveryErr = send(chunk(M{}, "stop")); deliveryErr != nil {
			return
		}
		if includeUsage {
			c := chunk(nil, nil)
			c["choices"] = []M{}
			c["usage"] = usage
			if deliveryErr = send(c); deliveryErr != nil {
				return
			}
		}
		if _, deliveryErr = fmt.Fprint(w, "data: [DONE]\n\n"); deliveryErr == nil {
			deliveryErr = http.NewResponseController(w).Flush()
		}
		return
	}
	deliveryErr = reply.finishJSON(M{"id": rid, "object": "chat.completion", "created": start.Unix(), "model": model, "choices": []M{{"index": 0, "message": message{"assistant", res.text}, "finish_reason": "stop"}}, "usage": usage})
}
