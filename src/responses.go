package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

var responseFieldName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

type responsesError struct {
	status        int
	code, message string
}

func (e *responsesError) Error() string { return e.message }
func invalidResponse(msg string) error  { return &responsesError{400, "invalid_request", msg} }

type responseFields map[string]json.RawMessage
type responseItems []json.RawMessage

type responsesRequest struct {
	options                 turn
	input                   responseItems
	omittedIncludes         []string
	model, previous, parent string
	stream, store           bool
}

func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func parseResponses(f responseFields) (*responsesRequest, error) {
	b := &responsesRequest{store: true}
	for k, v := range f {
		switch k {
		case "model":
			if json.Unmarshal(v, &b.model) != nil || len(b.model) > 200 {
				return nil, invalidResponse("Invalid model")
			}
		case "input":
		case "client_metadata":
			// This belongs to the client-to-gateway hop. The CLI builds its own
			// metadata for the authenticated upstream thread and turn.
			if !bytes.Equal(v, []byte("null")) && (len(v) == 0 || v[0] != '{') {
				return nil, invalidResponse("client_metadata must be an object or null")
			}
		case "prompt_cache_key":
			// The CLI also chooses its upstream cache key for that thread.
			if !bytes.Equal(v, []byte("null")) && (len(v) == 0 || v[0] != '"') {
				return nil, invalidResponse("prompt_cache_key must be a string or null")
			}
		case "instructions":
			if json.Unmarshal(v, &b.options.instructions) != nil {
				return nil, invalidResponse("instructions must be a string")
			}
		case "reasoning":
			var reasoning struct{ Effort string }
			if err := strictOption(v, &reasoning); err != nil {
				return nil, invalidResponse("The CLI adapter supports reasoning.effort only")
			}
			b.options.effort = reasoning.Effort
		case "text":
			var text struct {
				Format *struct {
					Type, Name, Description string
					Strict                  *bool
					Schema                  json.RawMessage
				}
			}
			if strictOption(v, &text) != nil {
				return nil, invalidResponse("Invalid text.format")
			}
			if f := text.Format; f != nil {
				switch f.Type {
				case "text":
				case "json_schema":
					if len(f.Schema) == 0 || f.Schema[0] != '{' {
						return nil, invalidResponse("text.format.schema must be a JSON Schema object")
					}
					b.options.outputSchema = f.Schema
				default:
					return nil, invalidResponse("The CLI adapter supports text and json_schema formats")
				}
			}
		case "tools", "tool_choice":
		case "parallel_tool_calls":
			// A client may allow parallel calls. This adapter always hands off one.
			var allowed *bool
			if json.Unmarshal(v, &allowed) != nil {
				return nil, invalidResponse("parallel_tool_calls must be a boolean or null")
			}
		case "include":
			var selectors []string
			if json.Unmarshal(v, &selectors) != nil {
				return nil, invalidResponse("include must be an array of selector strings or null")
			}
			// The public app-server events used by this adapter expose none of
			// these optional fields. Keep requests usable and report the omissions.
			for _, selector := range selectors {
				switch selector {
				case "reasoning.encrypted_content", "message.output_text.logprobs", "message.input_image.image_url",
					"web_search_call.action.sources", "web_search_call.results", "file_search_call.results",
					"computer_call_output.output.image_url", "code_interpreter_call.outputs":
					if !slices.Contains(b.omittedIncludes, selector) {
						b.omittedIncludes = append(b.omittedIncludes, selector)
					}
				default:
					if !responseFieldName.MatchString(selector) {
						return nil, invalidResponse("include contains an invalid selector name")
					}
					return nil, invalidResponse("Unknown include selector " + selector)
				}
			}
		case "stream":
			if json.Unmarshal(v, &b.stream) != nil {
				return nil, invalidResponse("stream must be a boolean")
			}
		case "store":
			if json.Unmarshal(v, &b.store) != nil {
				return nil, invalidResponse("store must be a boolean")
			}
		case "previous_response_id":
			if json.Unmarshal(v, &b.previous) != nil || len(b.previous) > 200 {
				return nil, invalidResponse("Invalid previous_response_id")
			}
		case "background":
			if string(v) != "false" && string(v) != "null" {
				return nil, invalidResponse("Background responses are unavailable")
			}
		default:
			if !responseFieldName.MatchString(k) {
				k = "(unknown)"
			}
			return nil, invalidResponse("Unsupported Responses field " + k)
		}
	}
	if strings.TrimSpace(b.model) == "" {
		return nil, invalidResponse("Provide a model")
	}
	input := f["input"]
	if len(input) > 0 && input[0] == '"' {
		var s string
		if json.Unmarshal(input, &s) != nil || strings.TrimSpace(s) == "" {
			return nil, invalidResponse("Provide input text or an array of input items")
		}
		b.input = responseItems{rawJSON(M{"type": "message", "role": "user", "content": []M{{"type": "input_text", "text": s}}})}
	} else if json.Unmarshal(input, &b.input) != nil || b.input == nil {
		return nil, invalidResponse("Provide input text or an array of input items")
	}
	if len(b.input) == 0 && b.previous == "" {
		return nil, invalidResponse("Provide at least one input item")
	}
	for index, v := range b.input {
		var i struct {
			Type, Role, Name string
			CallID           string `json:"call_id"`
			Content          json.RawMessage
			Output           json.RawMessage
		}
		if json.Unmarshal(v, &i) != nil {
			return nil, invalidResponse("Invalid input item")
		}
		switch i.Type {
		case "message", "":
			switch i.Role {
			case "user", "developer", "system", "assistant":
			default:
				return nil, invalidResponse("Invalid message role")
			}
			if err := validateResponseParts(i.Content); err != nil {
				return nil, err
			}
			if i.Type == "" || i.Content[0] == '"' {
				var item responseFields
				if err := json.Unmarshal(v, &item); err != nil {
					return nil, err
				}
				item["type"] = rawJSON("message")
				if i.Content[0] == '"' {
					partType := "input_text"
					if i.Role == "assistant" {
						partType = "output_text"
					}
					item["content"] = rawJSON([]responseFields{{"type": rawJSON(partType), "text": i.Content}})
				}
				b.input[index] = rawJSON(item)
			}
		case "function_call_output":
			if i.CallID == "" {
				return nil, invalidResponse("function_call_output requires a call_id")
			}
			if err := validateResponseParts(i.Output); err != nil {
				return nil, err
			}
		case "function_call":
			if i.CallID == "" || i.Name == "" {
				return nil, invalidResponse("function_call requires a call_id and name")
			}
		default:
			return nil, invalidResponse("Unsupported input item type")
		}
	}
	if err := b.parseTools(f); err != nil {
		return nil, err
	}
	return b, nil
}

func validateResponseParts(content json.RawMessage) error {
	if len(content) > 0 && content[0] == '"' {
		return nil
	}
	var parts []struct {
		Type     string
		Text     *string
		Detail   string
		ImageURL string `json:"image_url"`
		FileID   string `json:"file_id"`
	}
	if json.Unmarshal(content, &parts) != nil || parts == nil {
		return invalidResponse("Invalid Responses content")
	}
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			if p.Text == nil {
				return invalidResponse("Text content requires a text string")
			}
		case "refusal":
		case "input_image":
			if p.Detail != "" && p.Detail != "auto" && p.Detail != "low" && p.Detail != "high" && p.Detail != "original" {
				return invalidResponse("Use auto, low, high or original for input_image.detail")
			}
			if p.FileID != "" || !validInlineImage(p.ImageURL) {
				return invalidResponse("Use an inline base64 image in input_image.image_url")
			}
		default:
			return invalidResponse("Unsupported Responses content type")
		}
	}
	return nil
}

func strictOption(v json.RawMessage, dest any) error {
	d := json.NewDecoder(bytes.NewReader(v))
	d.DisallowUnknownFields()
	return d.Decode(dest)
}

func (b *responsesRequest) parseTools(f responseFields) error {
	type definition struct {
		Type, Name, Description string
		Parameters              json.RawMessage
		Strict                  *bool
	}
	var tools []definition
	if raw := f["tools"]; len(raw) != 0 && strictOption(raw, &tools) != nil {
		return invalidResponse("tools must be an array of function definitions")
	}
	names := map[string]bool{}
	for _, tool := range tools {
		if tool.Type != "function" || !responseFieldName.MatchString(tool.Name) || names[tool.Name] || tool.Strict != nil && *tool.Strict {
			return invalidResponse("The CLI adapter supports uniquely named, non-strict function tools")
		}
		names[tool.Name] = true
		if tool.Parameters == nil {
			tool.Parameters = rawJSON(M{"type": "object", "properties": M{}})
		}
		if tool.Parameters[0] != '{' {
			return invalidResponse("function parameters must be a JSON Schema object")
		}
		b.options.tools = append(b.options.tools, M{"type": "function", "name": tool.Name, "description": tool.Description, "inputSchema": tool.Parameters})
	}
	if raw := f["tool_choice"]; len(raw) != 0 {
		var choice string
		if json.Unmarshal(raw, &choice) != nil || choice != "auto" && choice != "none" {
			return invalidResponse("The CLI adapter supports tool_choice auto and none")
		}
		if choice == "none" {
			b.options.tools = nil
		}
	}
	return nil
}
