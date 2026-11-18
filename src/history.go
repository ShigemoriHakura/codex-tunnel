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

// Fetch on demand from the CLI, never duplicate conversation bodies in the gateway database.
func (a *app) history(w http.ResponseWriter, r *http.Request, owner string) {
	var thread string
	e := a.db.QueryRow(`SELECT thread FROM sessions WHERE id=? AND (?='' OR owner=?)`, r.PathValue("id"), owner, owner).Scan(&thread)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "not_found", "Session not found")
		return
	}
	if e != nil {
		internal(w, e)
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
	args := M{"threadId": thread, "itemsView": "full", "limit": 10, "sortDirection": "desc"}
	if c := r.URL.Query().Get("cursor"); c != "" {
		args["cursor"] = c
	}
	b, e := p.call("thread/turns/list", args, nil)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	var page struct {
		Data []struct {
			ID, Status string
			Items      []struct {
				Type, Text, Phase string
				Content           []struct{ Type, Text string }
			}
		}
		NextCursor string
	}
	if e = json.Unmarshal(b, &page); e != nil {
		upstream(w, ctx, e)
		return
	}
	data := []M{}
	for _, t := range page.Data {
		messages := []message{}
		for _, i := range t.Items {
			switch i.Type {
			case "userMessage":
				var text strings.Builder
				for _, part := range i.Content {
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
	jsonOut(w, 200, M{"data": data, "next_cursor": page.NextCursor})
}
