package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Hold the terminal event until usage and any continuation are committed, so
// an immediate follow-up can resolve the response ID returned to the client.
type responseStream struct {
	id, model, previous string
	created             int64
	store               bool
	stream              bool
	reply               *generationResponse
	items               responseItems
	size                int
	final               responseFields
	terminal            responseFields
	result              result
	chatDelta           func(string) error
}

func responseProtocolError(message string) error {
	return &responsesError{http.StatusBadGateway, "responses_protocol_error", message}
}

func (s *responseStream) read(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return responseRejection(resp)
	}
	// The workspace can send real SSE over HTTP/2 without a Content-Type.
	// Inspect the first field incrementally when the media type is unspecified.
	media, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	sniff := false
	switch strings.ToLower(strings.TrimSpace(media)) {
	case "text/event-stream":
	case "", "text/plain", "application/octet-stream":
		sniff = true
	default:
		return responseProtocolError("Responses upstream returned HTTP 200 with a non-SSE Content-Type")
	}
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 4096), chatBodyLimit)
	var data bytes.Buffer
	firstLine := true
	for scan.Scan() {
		line := scan.Bytes()
		if firstLine {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			firstLine = false
		}
		if sniff && len(line) > 0 {
			field, _, _ := bytes.Cut(line, []byte(":"))
			switch string(field) {
			case "", "event", "data", "id", "retry":
				sniff = false
			default:
				return responseProtocolError("Responses upstream returned HTTP 200 without recognizable SSE framing")
			}
		}
		if len(line) == 0 {
			if data.Len() == 0 {
				continue
			}
			end, err := s.event(data.Bytes())
			data.Reset()
			if err != nil || end {
				return err
			}
		} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len()+len(value)+1 > chatBodyLimit {
				return responseProtocolError("oversized Responses event")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	if err := scan.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return responseProtocolError("Responses upstream stream could not be read within the event size limit or the connection failed")
	}
	return &responsesError{http.StatusBadGateway, "responses_stream_incomplete", "Responses upstream closed the stream before a terminal event"}
}
func (s *responseStream) event(data []byte) (bool, error) {
	var e responseFields
	var typ string
	if json.Unmarshal(data, &e) != nil || json.Unmarshal(e["type"], &typ) != nil || !imageErrorCode.MatchString(typ) {
		return false, responseProtocolError("invalid Responses event")
	}
	// Gateways own response IDs; use the same ID in every envelope.
	var obj responseFields
	if raw := e["response"]; len(raw) > 0 {
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return false, responseProtocolError("invalid response object")
		}
		obj["id"] = rawJSON(s.id)
		obj["object"] = rawJSON("response")
		obj["store"] = rawJSON(s.store)
		if obj["model"] == nil {
			obj["model"] = rawJSON(s.model)
		}
		if obj["created_at"] == nil {
			obj["created_at"] = rawJSON(s.created)
		}
		if s.previous != "" {
			obj["previous_response_id"] = rawJSON(s.previous)
		}
		if obj["usage"] == nil {
			obj["usage"] = rawJSON(nil)
		}
		e["response"] = rawJSON(obj)
	}
	e["sequence_number"] = rawJSON(s.reply.sequence)
	switch typ {
	case "error", "response.failed":
		code := ""
		if obj != nil {
			var detail struct{ Code string }
			_ = json.Unmarshal(obj["error"], &detail)
			code = detail.Code
		} else {
			_ = json.Unmarshal(e["code"], &code)
		}
		if !imageErrorCode.MatchString(code) {
			code = "responses_provider_error"
		}
		status := 502
		switch code {
		case "rate_limit_exceeded", "insufficient_quota":
			status = 429
		case "invalid_request_error", "context_length_exceeded", "invalid_prompt":
			status = 400
		}
		msg := "Responses upstream failed (" + code + ")"
		if typ == "response.failed" && obj != nil {
			obj["error"] = rawJSON(M{"code": code, "message": msg})
			obj["status"] = rawJSON("failed")
			e["response"] = rawJSON(obj)
			s.reply.failedResponse = e
		}
		return false, &responsesError{status, code, msg}
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.reasoning_summary_text.delta":
		var d string
		if json.Unmarshal(e["delta"], &d) == nil && d != "" {
			if s.result.firstToken.IsZero() {
				s.result.firstToken = time.Now()
			}
			if typ == "response.output_text.delta" && s.chatDelta != nil {
				if err := s.chatDelta(d); err != nil {
					return false, err
				}
			}
		}
	case "response.output_item.done":
		raw := e["item"]
		idx := len(s.items)
		if v, ok := e["output_index"]; ok && json.Unmarshal(v, &idx) != nil {
			return false, responseProtocolError("invalid output index")
		}
		if idx < 0 || idx >= 4096 || len(raw) == 0 || raw[0] != '{' || s.size+len(raw) > chatBodyLimit {
			return false, responseProtocolError("invalid or oversized Responses output")
		}
		for len(s.items) <= idx {
			s.items = append(s.items, nil)
		}
		if s.items[idx] != nil {
			return false, responseProtocolError("duplicate Responses output index")
		}
		s.items[idx] = raw
		s.size += len(raw)
	case "response.completed", "response.incomplete":
		if obj == nil {
			return false, responseProtocolError("missing terminal response")
		}
		var output responseItems
		if v := obj["output"]; len(v) > 0 && json.Unmarshal(v, &output) != nil {
			return false, responseProtocolError("invalid terminal output")
		}
		if len(output) == 0 {
			output = s.items
		}
		if output == nil {
			output = responseItems{}
		}
		for _, i := range output {
			if len(i) == 0 {
				return false, responseProtocolError("missing completed output item")
			}
		}
		s.items = output
		obj["output"] = rawJSON(output)
		obj["status"] = rawJSON(strings.TrimPrefix(typ, "response."))
		_ = json.Unmarshal(obj["model"], &s.result.model)
		var usage struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
			In     struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			Out struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		}
		if json.Unmarshal(obj["usage"], &usage) == nil && usage.Input != nil && usage.Output != nil && *usage.Input >= 0 && *usage.Output >= 0 {
			s.result.metered = true
			s.result.total = counts{*usage.Input, *usage.Output, usage.In.Cached, usage.Out.Reasoning}
		}
		var visible strings.Builder
		for _, i := range responseMessages(output) {
			if i.Role == "assistant" {
				visible.WriteString(i.Content)
			}
		}
		s.result.text = visible.String()
		s.final = obj
		e["response"] = rawJSON(obj)
		s.terminal = e
		return true, nil
	}
	if s.stream {
		if err := s.reply.write(typ, e); err != nil {
			return false, err
		}
		s.reply.sequence++
	}
	return false, nil
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
