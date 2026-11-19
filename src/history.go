package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The native history prefix remains in the CLI after a session adopts Responses.
func (a *app) history(w http.ResponseWriter, r *http.Request, owner string) {
	var thread string
	sid := r.PathValue("id")
	bound := boundSession(r)
	if bound != "" && sid != bound {
		fail(w, 404, "not_found", "Session not found")
		return
	}
	e := a.db.QueryRow(`SELECT thread FROM sessions s WHERE id=? AND (?='' OR owner=? OR EXISTS(SELECT 1 FROM key_bindings b WHERE b.key_id=? AND b.session=s.id))`, sid, owner, owner, owner).Scan(&thread)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "not_found", "Session not found")
		return
	}
	if e != nil {
		internal(w, e)
		return
	}
	if a.responseHistoryPage(w, r, sid, thread) {
		return
	}
	if thread == "" {
		jsonOut(w, 200, M{"data": []M{}})
		return
	}
	if !a.acquire(w) {
		return
	}
	defer a.release()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	run, e := a.current()
	if e != nil {
		internal(w, e)
		return
	}
	p, e := run.open(ctx, false)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	defer p.close()
	// Keep each CLI reply within the frame limit even when consecutive turns contain large images.
	data := []M{}
	cursor := r.URL.Query().Get("cursor")
	for len(data) < 10 {
		args := M{"threadId": thread, "itemsView": "full", "limit": 1, "sortDirection": "desc"}
		if cursor != "" {
			args["cursor"] = cursor
		}
		raw, err := p.call("thread/turns/list", args, nil)
		if err != nil {
			upstream(w, ctx, err)
			return
		}
		var page historyPage
		if err = json.Unmarshal(raw, &page); err != nil {
			upstream(w, ctx, err)
			return
		}
		data = append(data, page.messages()...)
		if page.NextCursor == "" {
			cursor = ""
			break
		}
		if len(page.Data) == 0 || page.NextCursor == cursor {
			upstream(w, ctx, errors.New("invalid turn history cursor"))
			return
		}
		cursor = page.NextCursor
	}
	jsonOut(w, 200, M{"data": data, "next_cursor": cursor})
}

type historyPage struct {
	Data []struct {
		ID, Status string
		Items      []struct {
			Type, Text, Phase string
			Content           []struct{ Type, Text string }
		}
	}
	NextCursor string
}

func (page historyPage) messages() []M {
	data := []M{}
	for _, t := range page.Data {
		messages := []message{}
		for _, i := range t.Items {
			switch i.Type {
			case "userMessage":
				var text strings.Builder
				for _, part := range i.Content {
					if part.Type == "image" || part.Type == "localImage" {
						text.WriteString("[Image]")
					}
					if part.Type == "text" {
						text.WriteString(part.Text)
					}
				}
				messages = append(messages, message{"user", text.String()})
			case "agentMessage":
				if i.Phase != "commentary" {
					messages = append(messages, message{"assistant", i.Text})
				}
			}
		}
		data = append(data, M{"id": t.ID, "status": t.Status, "messages": messages})
	}
	return data
}
