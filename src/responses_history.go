package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const responseRetention = 30 * 24 * time.Hour

func (a *app) responseHead(ctx context.Context, sid string) (string, error) {
	if sid == "" {
		return "", nil
	}
	var head string
	err := a.db.QueryRowContext(ctx, "SELECT id FROM response_turns WHERE session=? ORDER BY rowid DESC LIMIT 1", sid).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return head, err
}

// A turn stores only its new input and output. Shared ancestors can serve multiple
// stateless branches. A fixed session has one FIFO-serialized current head.
func (a *app) responseContext(ctx context.Context, b *responsesRequest, owner, sid, head string) (responseItems, error) {
	parent := b.previous
	if parent != "" {
		var exists bool
		err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM response_turns WHERE id=? AND stored=1 AND
   ((session IS NULL AND ?='' AND owner=? AND created>=?) OR session=?))`, parent, sid, owner, time.Now().Add(-responseRetention).Unix(), sid).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, &responsesError{404, "previous_response_not_found", "Previous response not found"}
		}
		if sid != "" && parent != head {
			return nil, &responsesError{409, "session_conflict", "Continue from the latest response in this session"}
		}
	} else {
		parent = head
	}
	b.parent = parent
	var reverse []responseItems
	size := 0
	for parent != "" {
		var input, output []byte
		err := a.db.QueryRowContext(ctx, `SELECT parent,input,output FROM response_turns WHERE id=? AND ((session IS NULL AND ?='' AND owner=?) OR session=?)`, parent, sid, owner, sid).Scan(&parent, &input, &output)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &responsesError{404, "previous_response_not_found", "Previous response history has expired"}
		}
		if err != nil {
			return nil, err
		}
		size += len(input) + len(output)
		if size > chatBodyLimit || len(reverse) >= 4096 {
			return nil, invalidResponse("Responses history is too large; start a new conversation")
		}
		var in, out responseItems
		if json.Unmarshal(input, &in) != nil || json.Unmarshal(output, &out) != nil {
			return nil, errors.New("invalid saved Responses history")
		}
		reverse = append(reverse, append(in, out...))
	}
	var history responseItems
	for i := len(reverse) - 1; i >= 0; i-- {
		history = append(history, reverse[i]...)
	}
	if b.previous == "" && len(history) > 0 {
		b.input = unreplayedInput(history, b.input)
	}
	return append(history, b.input...), nil
}

// Some clients send the full transcript on every HTTP request, others send only
// a new message or function output. Match the latest assistant/tool item by ID
// or by content (CLI clients can strip output item IDs during replay).
func unreplayedInput(history, input responseItems) responseItems {
	if len(input) < 2 {
		return input
	}
	type anchor struct{ Type, Role, CallID, Content string }
	key := func(raw json.RawMessage) anchor {
		var i struct {
			Type, Role string
			Call       string `json:"call_id"`
			Content    json.RawMessage
		}
		if json.Unmarshal(raw, &i) != nil {
			return anchor{}
		}
		if (i.Type == "function_call" || i.Type == "custom_tool_call") && i.Call != "" {
			return anchor{Type: i.Type, CallID: i.Call}
		}
		if i.Role == "assistant" {
			m := responseMessages(responseItems{raw})
			if len(m) > 0 && m[0].Content != "" {
				return anchor{Role: "assistant", Content: m[0].Content}
			}
		}
		return anchor{}
	}
	for j := len(history) - 1; j >= 0; j-- {
		k := key(history[j])
		if k == (anchor{}) {
			continue
		}
		for n := len(input) - 1; n >= 0; n-- {
			if key(input[n]) == k {
				return input[n+1:]
			}
		}
		break
	}
	return input
}

func (b *responsesRequest) save(tx *sql.Tx, sid, owner, id string, out *responseStream) error {
	if sid == "" && !b.store {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM response_turns WHERE session IS NULL AND created<?", time.Now().Add(-responseRetention).Unix()); err != nil {
		return err
	}
	var status string
	_ = json.Unmarshal(out.final["status"], &status)
	_, err := tx.Exec("INSERT INTO response_turns(id,owner,session,parent,stored,input,output,status,created) VALUES(?,?,NULLIF(?,''),?,?,?,?,?,?)", id, owner, sid, b.parent, b.store, []byte(rawJSON(b.input)), []byte(rawJSON(out.items)), status, now())
	if err == nil && sid != "" {
		_, err = tx.Exec("UPDATE sessions SET model=CASE WHEN model='' THEN ? ELSE model END,updated=? WHERE id=?", b.model, now(), sid)
	}
	return err
}

func chatResponseRequest(msgs []chatMessage, model string) *responsesRequest {
	b := &responsesRequest{model: model, fields: responseFields{"model": rawJSON(model)}}
	for _, m := range msgs {
		parts, _ := m.parts()
		if len(parts) == 0 {
			continue
		}
		content := []M{}
		for _, p := range parts {
			if p.Type == "image" {
				item := M{"type": "input_image", "image_url": p.URL}
				if p.Detail != "" {
					item["detail"] = p.Detail
				}
				content = append(content, item)
			} else {
				kind := "input_text"
				if m.Role == "assistant" {
					kind = "output_text"
				}
				content = append(content, M{"type": kind, "text": p.Text})
			}
		}
		b.input = append(b.input, rawJSON(M{"type": "message", "role": m.Role, "content": content}))
	}
	return b
}

// Existing CLI-backed turns remain in the CLI. When a client switches protocol
// the model still receives their original text and inline vision inputs.
func (a *app) nativeResponseHistory(ctx context.Context, run runner, thread string) (responseItems, error) {
	p, err := a.servers.acquire(ctx, run, false)
	if err != nil {
		return nil, err
	}
	defer a.servers.release(p, false)
	var items responseItems
	cursor := ""
	size := 0
	for page := 0; page < 4096; page++ {
		args := M{"threadId": thread, "itemsView": "full", "limit": 1, "sortDirection": "asc"}
		if cursor != "" {
			args["cursor"] = cursor
		}
		raw, err := p.call(ctx, "thread/turns/list", args)
		if err != nil {
			return nil, err
		}
		size += len(raw)
		if size > chatBodyLimit {
			return nil, invalidResponse("Session context exceeds 24 MiB")
		}
		var data struct {
			NextCursor string
			Data       []struct {
				Items []struct {
					Type, Text, Phase string
					Content           []struct{ Type, Text, URL, Detail string }
				}
			}
		}
		if err = json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		for _, t := range data.Data {
			for _, i := range t.Items {
				switch i.Type {
				case "userMessage":
					var parts []M
					for _, c := range i.Content {
						switch c.Type {
						case "text":
							parts = append(parts, M{"type": "input_text", "text": c.Text})
						case "image":
							parts = append(parts, M{"type": "input_image", "image_url": c.URL})
						case "localImage":
							return nil, invalidResponse("Cannot replay local files in a Responses session")
						}
					}
					if len(parts) > 0 {
						items = append(items, rawJSON(M{"type": "message", "role": "user", "content": parts}))
					}
				case "agentMessage":
					if i.Phase != "commentary" {
						items = append(items, rawJSON(M{"type": "message", "role": "assistant", "content": []M{{"type": "output_text", "text": i.Text}}}))
					}
				}
			}
		}
		if data.NextCursor == "" {
			return items, nil
		}
		if cursor == data.NextCursor {
			return nil, errors.New("invalid CLI history cursor")
		}
		cursor = data.NextCursor
	}
	return nil, invalidResponse("Session history is too large")
}

// Responses turns are newer than the immutable CLI prefix. Switch cursors at
// that boundary so the existing console can page through both.
func (a *app) responseHistoryPage(w http.ResponseWriter, r *http.Request, sid, thread string) bool {
	cursor := r.URL.Query().Get("cursor")
	if strings.HasPrefix(cursor, "cli:") {
		q := r.URL.Query()
		q.Set("cursor", strings.TrimPrefix(cursor, "cli:"))
		r.URL.RawQuery = q.Encode()
		return false
	}
	var before int64 = 1<<63 - 1
	if cursor != "" {
		v, ok := strings.CutPrefix(cursor, "resp:")
		if !ok {
			return false
		}
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n <= 0 {
			fail(w, 400, "invalid_cursor", "Invalid Responses history cursor")
			return true
		}
		before = n
	}
	rows, err := a.db.QueryContext(r.Context(), "SELECT rowid,id,input,output,status FROM response_turns WHERE session=? AND rowid<? ORDER BY rowid DESC LIMIT 11", sid, before)
	if err != nil {
		internal(w, err)
		return true
	}
	defer rows.Close()
	data := []M{}
	next := ""
	var last int64
	for rows.Next() {
		if len(data) == 10 {
			next = fmt.Sprintf("resp:%d", last)
			break
		}
		var id, status string
		var in, out []byte
		if err = rows.Scan(&last, &id, &in, &out, &status); err != nil {
			internal(w, err)
			return true
		}
		var input, output responseItems
		if json.Unmarshal(in, &input) != nil || json.Unmarshal(out, &output) != nil {
			internal(w, errors.New("invalid Responses history"))
			return true
		}
		data = append(data, M{"id": id, "status": status, "messages": responseMessages(append(input, output...))})
	}
	if err = rows.Err(); err != nil {
		internal(w, err)
		return true
	}
	if len(data) == 0 {
		return false
	}
	if next == "" && thread != "" {
		next = "cli:"
	}
	jsonOut(w, 200, M{"data": data, "next_cursor": next})
	return true
}
