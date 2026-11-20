package main

import (
	"context"
	"encoding/json"
	"strings"
)

// Responses history is keyed by the gateway's response IDs. Replaying the
// selected branch in an ephemeral CLI thread also supports pre-migration rows.
type responseStream struct {
	id, model, previous string
	created             int64
	store, stream       bool
	reply               *generationResponse
	items               responseItems
	final               responseFields
}

func (s *responseStream) send(kind string, fields M) error {
	if !s.stream {
		return nil
	}
	fields["type"] = kind
	fields["sequence_number"] = s.reply.sequence
	s.reply.sequence++
	return s.reply.write(kind, fields)
}

func (s *responseStream) envelope(status string, usage any) responseFields {
	obj := responseFields{"id": rawJSON(s.id), "object": rawJSON("response"), "status": rawJSON(status), "created_at": rawJSON(s.created), "model": rawJSON(s.model), "output": rawJSON(s.items), "usage": rawJSON(usage), "store": rawJSON(s.store), "error": rawJSON(nil)}
	if s.previous != "" {
		obj["previous_response_id"] = rawJSON(s.previous)
	}
	return obj
}

func (a *app) runResponses(ctx context.Context, run runner, b *responsesRequest, items responseItems, s *responseStream, chatDelta func(string) error) (result, error) {
	t := b.options
	t.model = b.model
	if err := t.responseInput(items); err != nil {
		return result{}, err
	}
	s.items = responseItems{}
	t.onThread = func(_, model string) error {
		s.model = model
		obj := s.envelope("in_progress", nil)
		if err := s.send("response.created", M{"response": obj}); err != nil {
			return err
		}
		return s.send("response.in_progress", M{"response": obj})
	}
	msgID := id("msg_")
	opened := false
	part := func(text string) M { return M{"type": "output_text", "text": text, "annotations": []any{}} }
	message := func(status string, contents []M) M {
		return M{"id": msgID, "type": "message", "role": "assistant", "status": status, "content": contents}
	}
	t.delta = func(delta string) error {
		if chatDelta != nil {
			return chatDelta(delta)
		}
		if !opened {
			opened = true
			if err := s.send("response.output_item.added", M{"output_index": 0, "item": message("in_progress", []M{})}); err != nil {
				return err
			}
			if err := s.send("response.content_part.added", M{"output_index": 0, "content_index": 0, "item_id": msgID, "part": part("")}); err != nil {
				return err
			}
		}
		return s.send("response.output_text.delta", M{"output_index": 0, "content_index": 0, "item_id": msgID, "delta": delta})
	}
	var function M
	if len(t.tools) > 0 {
		t.onTool = func(callID, ns, name string, args json.RawMessage) error {
			for _, tool := range t.tools {
				if ns == "" && tool["name"] == name {
					function = M{"type": "function_call", "id": id("fc_"), "call_id": callID, "name": name, "arguments": string(args), "status": "completed"}
					return nil
				}
			}
			return &responsesError{502, "unexpected_tool", "CLI requested a tool outside the client definitions"}
		}
	}
	res, err := run.run(ctx, a.servers, t)
	if err != nil {
		return res, err
	}
	if res.text != "" {
		if !opened && chatDelta == nil {
			if err := t.delta(res.text); err != nil {
				return res, err
			}
		}
		item := message("completed", []M{part(res.text)})
		if err := s.send("response.output_text.done", M{"output_index": 0, "content_index": 0, "item_id": msgID, "text": res.text}); err != nil {
			return res, err
		}
		if err := s.send("response.content_part.done", M{"output_index": 0, "content_index": 0, "item_id": msgID, "part": part(res.text)}); err != nil {
			return res, err
		}
		if err := s.send("response.output_item.done", M{"output_index": 0, "item": item}); err != nil {
			return res, err
		}
		s.items = append(s.items, rawJSON(item))
	}
	if function != nil {
		index := len(s.items)
		start := M{"id": function["id"], "call_id": function["call_id"], "type": "function_call", "name": function["name"], "arguments": "", "status": "in_progress"}
		if err := s.send("response.output_item.added", M{"output_index": index, "item": start}); err != nil {
			return res, err
		}
		if err := s.send("response.function_call_arguments.delta", M{"output_index": index, "item_id": function["id"], "delta": function["arguments"]}); err != nil {
			return res, err
		}
		if err := s.send("response.function_call_arguments.done", M{"output_index": index, "item_id": function["id"], "name": function["name"], "arguments": function["arguments"]}); err != nil {
			return res, err
		}
		if err := s.send("response.output_item.done", M{"output_index": index, "item": function}); err != nil {
			return res, err
		}
		s.items = append(s.items, rawJSON(function))
	}
	var usage M
	if res.metered {
		usage = M{"input_tokens": res.total.Input, "output_tokens": res.total.Output, "total_tokens": res.total.Input + res.total.Output}
		if res.total.Cached != nil {
			usage["input_tokens_details"] = M{"cached_tokens": *res.total.Cached}
		}
		if res.total.Reasoning != nil {
			usage["output_tokens_details"] = M{"reasoning_tokens": *res.total.Reasoning}
		}
	}
	s.final = s.envelope("completed", usage)
	return res, nil
}

// History goes through thread/inject_items with the original roles. Only the new
// user input is translated to turn/start's typed inputs.
func (t *turn) responseInput(items responseItems) error {
	if len(items) == 0 {
		return invalidResponse("Provide a new user message or function_call_output")
	}
	size := 0
	for _, item := range items {
		size += len(item)
		if size > chatBodyLimit {
			return invalidResponse("Combined Responses context exceeds 24 MiB")
		}
	}
	last := len(items) - 1
	var msg struct {
		Type, Role string
		Content    []struct {
			Type, Text, Detail string
			URL                string `json:"image_url"`
		}
	}
	if json.Unmarshal(items[last], &msg) != nil {
		return invalidResponse("Invalid final input item")
	}
	t.history = items[:last]
	if msg.Role != "user" {
		// An empty typed input schedules a turn against the injected context,
		// including a tool result, without inventing an extra user message.
		t.history = items
		t.input = []turnInput{}
		return nil
	}
	for _, p := range msg.Content {
		switch p.Type {
		case "input_text":
			t.input = append(t.input, turnInput{Type: "text", Text: p.Text})
		case "input_image":
			t.input = append(t.input, turnInput{Type: "image", URL: p.URL, Detail: p.Detail})
		default:
			return invalidResponse("New user messages accept input_text and input_image")
		}
	}
	if len(t.input) == 0 {
		return invalidResponse("The final user message is empty")
	}
	return nil
}

func responseMessages(items responseItems) []message {
	messages := []message{}
	for _, raw := range items {
		var i struct {
			Type, Role string
			Content    json.RawMessage
		}
		if json.Unmarshal(raw, &i) != nil || i.Role == "" {
			continue
		}
		var content string
		if json.Unmarshal(i.Content, &content) != nil {
			var parts []struct{ Type, Text, Refusal string }
			_ = json.Unmarshal(i.Content, &parts)
			var text strings.Builder
			for _, p := range parts {
				switch p.Type {
				case "input_text", "output_text":
					text.WriteString(p.Text)
				case "input_image":
					text.WriteString("[Image]")
				case "refusal":
					text.WriteString(p.Refusal)
				}
			}
			content = text.String()
		}
		messages = append(messages, message{i.Role, content})
	}
	return messages
}
