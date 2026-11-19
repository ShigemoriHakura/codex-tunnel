package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
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
	var latest chatMessage
	var output imageOutput
	var direct *imageRequest
	var imageParams imageRequest
	images := imageStream{w: w}
	stream, includeUsage := false, false
	kind := "chat"
	rid := id("chatcmpl-")
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
	if !a.acquire(w) {
		return
	}
	defer a.release()
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
		if b.PartialImages != nil && (*b.PartialImages < 0 || *b.PartialImages > 3 || *b.PartialImages > 0 && !stream) {
			fail(w, 400, "invalid_image_option", "partial_images must be 0–3 and previews require stream=true")
			return
		}
		if strings.HasPrefix(b.Model, "gpt-image-") || b.Model == "chatgpt-image-latest" {
			direct = b
		} else if (b.Size != "" && b.Size != "auto") || (b.Quality != "" && b.Quality != "auto") || (b.Background != "" && b.Background != "auto" && b.Background != "opaque") || b.Moderation != "" || b.SafetyIdentifier != "" || b.User != "" {
			fail(w, 400, "invalid_image_option", "Specify an image model such as gpt-image-2.5-sunburst to select image generation parameters")
			return
		}
		output = b.imageOutput
		if err := output.validate(); err != nil {
			fail(w, 400, "invalid_image_output", err.Error())
			return
		}
		b.imageOutput = output
		t.model = b.Model
		t.input = []turnInput{{Type: "text", Text: b.Prompt}}

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
		var transcript []message
		var err error
		t.input, transcript, err = chatInputs(b.Messages)
		if err != nil {
			fail(w, 400, "invalid_messages", err.Error())
			return
		}
		t.model = b.Model
		stream = b.Stream
		includeUsage = b.StreamOptions != nil && b.StreamOptions.IncludeUsage
		if trace != nil && trace.capture {
			trace.reqBody = preview(M{"model": b.Model, "messages": transcript, "stream": b.Stream})
		}
		if boundSession(r) != "" && len(b.Messages) > 1 {
			latest = b.Messages[len(b.Messages)-1]
		}

	}
	if len(t.model) > 200 {
		fail(w, 400, "invalid_model", "Model name too long")
		return
	}
	if trace != nil {
		trace.model = t.model
		trace.stream = &stream
	}
	var prior counts
	if sid != "" {
		if !a.lock(w, sid) {
			return
		}
		defer a.busy.Delete(sid)
		var model string
		e := a.db.QueryRow(`SELECT s.thread,s.model,s.last_in,s.last_out,m.cached,m.reasoning FROM sessions s LEFT JOIN session_metrics m ON s.id=m.session WHERE s.id=? AND (s.owner=? OR EXISTS(SELECT 1 FROM key_bindings b WHERE b.key_id=? AND b.session=s.id))`, sid, owner, owner).Scan(&t.thread, &model, &prior.Input, &prior.Output, &prior.Cached, &prior.Reasoning)
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, 404, "not_found", "Session not found")
			return
		}
		if e != nil {
			internal(w, e)
			return
		}
		if t.thread != "" && latest.Role != "" {
			t.input, e = latest.parts()
			if e != nil {
				fail(w, 400, "invalid_messages", e.Error())
				return
			}
		}
		if model != "" {
			if t.model != "" && t.model != model {
				fail(w, 400, "model_mismatch", "Create a new session to switch models")
				return
			}
			t.model = model
		}
		t.onThread = func(thread, model string) error {
			_, e := a.db.Exec(`UPDATE sessions SET thread=?,model=?,updated=? WHERE id=?`, thread, model, now(), sid)
			return e
		}
	}
	run, e := a.current()
	if e != nil {
		internal(w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	opened := false
	model := t.model
	chunk := func(delta M, finish any) M {
		return M{"id": rid, "object": "chat.completion.chunk", "created": start.Unix(), "model": model, "choices": []M{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	send := func(v any) error {
		data, _ := json.Marshal(v)
		if _, e := fmt.Fprintf(w, "data: %s\n\n", data); e != nil {
			return e
		}
		return http.NewResponseController(w).Flush()
	}
	if stream && !image {
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
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Accel-Buffering", "no")
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
	var stopHeartbeats func()
	if image && stream {
		stopHeartbeats = images.startHeartbeats(ctx, cancel, 3*time.Second)
		defer stopHeartbeats()
	}
	if direct != nil {
		res, runErr = a.generateNamedImage(ctx, run, *direct, func(e imageEvent) error { return images.write(imagePartial, e) })
	} else {
		res, runErr = run.run(ctx, t)
		if runErr == nil && image {
			res.image, runErr = output.encode(res.image)
		}
	}
	if stopHeartbeats != nil {
		stopHeartbeats()
	}
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
	if image && stream && runErr == nil {
		runErr = ctx.Err()
	}
	status := errorStatus(ctx, runErr)
	if trace != nil {
		trace.model = model
		if direct == nil {
			recordHop(ctx, "stdio", "codex/app-server/turn", upstreamStart, hopErr)
		}
		if trace.capture {
			safe := make([]turnInput, len(t.input))
			copy(safe, t.input)
			for i := range safe {
				if safe[i].Type == "image" {
					safe[i].URL = "[image omitted]"
				}
			}
			if direct != nil {
				trace.outBody = preview(direct.providerRequest())
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
		if e == nil && sid != "" && res.metered {
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
		if images.opened {
			images.write("error", M{"type": "error", "error": M{"code": "storage_error", "message": "Could not save request usage"}})
			if trace != nil {
				trace.status, trace.code, trace.message = 500, "storage_error", "Could not save request usage"
			}
		} else if !opened {
			internal(w, e)
		} else {
			if trace != nil {
				trace.status, trace.code, trace.message = 500, "storage_error", "Could not save request usage"
			}
			send(M{"error": M{"code": "storage_error", "message": "Could not save request usage"}})
		}
		return
	}
	if runErr != nil {
		if status == 499 && image && stream {
			return
		}
		if images.opened {
			code, msg := "upstream_error", "Image generation stream ended before completion"
			var provider *imageProviderError
			if errors.As(runErr, &provider) {
				code, msg = "image_provider_error", provider.Error()
			}
			images.write("error", M{"type": "error", "error": M{"code": code, "message": msg}})
			if trace != nil {
				trace.code, trace.message = code, msg
			}
		} else if !opened {
			upstream(w, ctx, runErr)
		} else {
			log.Printf("stream failed: %v", runErr)
			send(M{"error": M{"code": "upstream_error", "message": "Codex turn did not complete"}})
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
		return
	}
	if image {
		if stream {
			if writeErr := images.write(imageCompleted, imageParams.completedEvent(res)); writeErr != nil {
				// Keep reported upstream usage even if the client left during delivery.
				if trace != nil {
					trace.status, trace.code, trace.message = 499, "delivery_failed", "Connection closed while delivering the final image"
				}
				if _, err := a.db.Exec("UPDATE requests SET status=? WHERE id=?", 499, rid); err != nil {
					log.Printf("record image delivery failure: %v", err)
				}
			}
			return
		}
		jsonOut(w, 200, M{"created": start.Unix(), "output_format": output.Format, "data": []M{{"b64_json": res.image}}})
		return
	}
	var usage any
	if res.metered {
		usage = M{"prompt_tokens": used.Input, "completion_tokens": used.Output, "total_tokens": used.Input + used.Output}
	}
	if stream {
		if !opened {
			if e = t.delta(res.text); e != nil {
				return
			}
		}
		send(chunk(M{}, "stop"))
		if includeUsage {
			c := chunk(nil, nil)
			c["choices"] = []M{}
			c["usage"] = usage
			send(c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	jsonOut(w, 200, M{"id": rid, "object": "chat.completion", "created": start.Unix(), "model": model, "choices": []M{{"index": 0, "message": message{"assistant", res.text}, "finish_reason": "stop"}}, "usage": usage})
}
