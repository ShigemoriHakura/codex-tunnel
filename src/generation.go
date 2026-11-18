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
	Model         string    `json:"model"`
	Messages      []message `json:"messages"`
	Stream        bool      `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}
type imageRequest struct {
	Model, Prompt string
	N             *int
	Format        string `json:"response_format"`
}

func (a *app) generate(w http.ResponseWriter, r *http.Request, owner string) {
	start := time.Now()
	image := r.URL.Path == "/v1/images/generations"
	sid := r.Header.Get("X-Session-ID")
	t := turn{image: image, persist: sid != ""}
	stream, includeUsage := false, false
	kind := "chat"
	rid := id("chatcmpl-")
	if image {
		kind = "image"
		rid = id("img_")
		var b imageRequest
		if !body(w, r, &b) {
			return
		}
		if strings.TrimSpace(b.Prompt) == "" || (b.N != nil && *b.N != 1) || (b.Format != "" && b.Format != "b64_json") || sid != "" {
			fail(w, 400, "invalid_request", "Use a prompt, n=1, b64_json and no session for image generation")
			return
		}
		t.model = b.Model
		t.prompt = b.Prompt
	} else {
		var b chatRequest
		if !body(w, r, &b) {
			return
		}
		if len(b.Messages) < 1 || len(b.Messages) > 256 {
			fail(w, 400, "invalid_messages", "Provide 1–256 text messages")
			return
		}
		for _, m := range b.Messages {
			if (m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "developer") || strings.TrimSpace(m.Content) == "" {
				fail(w, 400, "invalid_messages", "Unsupported role or empty content")
				return
			}
		}
		if b.Messages[len(b.Messages)-1].Role != "user" || (sid != "" && len(b.Messages) != 1) {
			fail(w, 400, "invalid_messages", "End with a user message; send only the new user message in session mode")
			return
		}
		t.model = b.Model
		stream = b.Stream
		includeUsage = b.StreamOptions != nil && b.StreamOptions.IncludeUsage
		if len(b.Messages) == 1 {
			t.prompt = b.Messages[0].Content
		} else {
			bts, _ := json.Marshal(b.Messages)
			t.prompt = "Continue this conversation, answering the final user message:\n" + string(bts)
		}
	}
	if len(t.model) > 200 {
		fail(w, 400, "invalid_model", "Model name too long")
		return
	}
	var prior counts
	if sid != "" {
		if !a.lock(w, sid) {
			return
		}
		defer a.busy.Delete(sid)
		var model string
		e := a.db.QueryRow(`SELECT thread,model,last_in,last_out FROM sessions WHERE id=? AND owner=?`, sid, owner).Scan(&t.thread, &model, &prior.Input, &prior.Output)
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, 404, "not_found", "Session not found")
			return
		}
		if e != nil {
			internal(w, e)
			return
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
	if !a.acquire(w) {
		return
	}
	defer a.release()
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
	if stream {
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
	res, runErr := run.run(ctx, t)
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
	status := 200
	if runErr != nil {
		status = 502
		if ctx.Err() != nil {
			status = 504
		}
	}
	tx, e := a.db.Begin()
	if e == nil {
		defer tx.Rollback()
		_, e = tx.Exec(`INSERT INTO requests VALUES(?,?,?,?,?,?,?,?,?,?,?)`, rid, owner, sid, kind, model, status, used.Input, used.Output, res.metered, time.Since(start).Milliseconds(), start.Unix())
		if e == nil && sid != "" && res.metered {
			_, e = tx.Exec(`UPDATE sessions SET last_in=?,last_out=?,updated=? WHERE id=?`, res.total.Input, res.total.Output, now(), sid)
		}
		if e == nil {
			e = tx.Commit()
		}
	}
	if e != nil {
		if !opened {
			internal(w, e)
		} else {
			send(M{"error": M{"code": "storage_error", "message": "Could not save request usage"}})
		}
		return
	}
	if runErr != nil {
		if !opened {
			upstream(w, ctx, runErr)
		} else {
			log.Printf("stream failed: %v", runErr)
			send(M{"error": M{"code": "upstream_error", "message": "Codex turn did not complete"}})
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
		return
	}
	if image {
		jsonOut(w, 200, M{"created": start.Unix(), "data": []M{{"b64_json": res.image}}})
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
