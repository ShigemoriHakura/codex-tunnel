package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const responsesPath = "/backend-api/codex/responses"

type responsesError struct {
	status        int
	code, message string
}

func (e *responsesError) Error() string { return e.message }
func invalidResponse(msg string) error  { return &responsesError{400, "invalid_request", msg} }

type responseFields map[string]json.RawMessage
type responseItems []json.RawMessage

type responsesRequest struct {
	fields                  responseFields
	input                   responseItems
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
	b := &responsesRequest{fields: f, store: true}
	for k, v := range f {
		switch k {
		case "model":
			if json.Unmarshal(v, &b.model) != nil || len(b.model) > 200 {
				return nil, invalidResponse("Invalid model")
			}
		case "input", "instructions", "reasoning", "text", "tools", "tool_choice", "parallel_tool_calls", "include", "prompt_cache_key", "client_metadata", "metadata", "max_output_tokens", "temperature", "top_p", "service_tier", "truncation", "safety_identifier", "max_tool_calls", "top_logprobs", "prompt_cache_retention", "verbosity":
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
			if !imageErrorCode.MatchString(k) {
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
			Type, Role string
			Content    json.RawMessage
			Output     json.RawMessage
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
		case "function_call_output", "custom_tool_call_output":
			if err := validateResponseParts(i.Output); err != nil {
				return nil, err
			}
		case "function_call", "custom_tool_call", "reasoning", "web_search_call", "compaction", "tool_search_call", "tool_search_output":
		default:
			return nil, invalidResponse("Unsupported input item type")
		}
	}
	var tools []struct {
		Type  string
		Tools []struct{ Type string }
	}
	if raw := f["tools"]; len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return nil, invalidResponse("Invalid tools array")
	}
	for _, t := range tools {
		switch t.Type {
		case "function", "custom", "web_search", "web_search_preview", "tool_search":
		case "namespace":
			for _, n := range t.Tools {
				if n.Type != "function" && n.Type != "custom" {
					return nil, invalidResponse("Unsupported namespaced tool")
				}
			}
		default:
			return nil, invalidResponse("Unsupported Responses tool type")
		}
	}
	return b, nil
}

func validateResponseParts(content json.RawMessage) error {
	if len(content) > 0 && content[0] == '"' {
		return nil
	}
	var parts []struct {
		Type     string
		ImageURL string `json:"image_url"`
		FileID   string `json:"file_id"`
	}
	if json.Unmarshal(content, &parts) != nil || parts == nil {
		return invalidResponse("Invalid Responses content")
	}
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "refusal":
		case "input_image":
			if p.FileID != "" || !validInlineImage(p.ImageURL) {
				return invalidResponse("Use an inline base64 image in input_image.image_url")
			}
		default:
			return invalidResponse("Unsupported Responses content type")
		}
	}
	return nil
}

// The client defines and executes its own functions. Send only Responses protocol
// data to a fixed first-party endpoint selected from the active CLI account.
func (a *app) runResponses(ctx context.Context, run runner, b *responsesRequest, owner string, input responseItems, out *responseStream) (err error) {
	p, err := a.servers.acquire(ctx, run, false)
	if err != nil {
		return err
	}
	defer func() { a.servers.release(p, errors.Is(err, errUnconfirmedRPC)) }()
	f := b.fields
	delete(f, "previous_response_id")
	delete(f, "background")
	f["input"] = rawJSON(input)
	f["stream"] = rawJSON(true)
	f["store"] = rawJSON(false)
	if _, ok := f["instructions"]; !ok {
		f["instructions"] = rawJSON("")
	}
	if cache := f["prompt_cache_key"]; len(cache) > 0 {
		f["prompt_cache_key"] = rawJSON(digest(owner + string(cache)))
	}
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(body) > chatBodyLimit {
		return invalidResponse("Combined Responses context exceeds 24 MiB")
	}
	client := http.Client{Transport: a.workspaceTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var identity string
	for attempt := 0; attempt < 2; attempt++ {
		account, e := p.call(ctx, "account/read", M{"refreshToken": attempt != 0})
		if e != nil {
			return e
		}
		var detail struct{ Account struct{ Type string } }
		if e = json.Unmarshal(account, &detail); e != nil {
			return e
		}
		var endpoint, token, accountID, region string
		if detail.Account.Type == "apiKey" {
			endpoint = "https://api.openai.com/v1/responses"
			token = run.responsesAPIKey()
			if token == "" {
				return errors.New("Codex API key is unavailable")
			}
		} else {
			ws, key, e := run.workspaceCredential(account)
			if e != nil {
				return e
			}
			endpoint, e = ws.endpointFor(responsesPath)
			if e != nil {
				return e
			}
			token, accountID, region = key, ws.ChatGPTAccountID, ws.AccountRoutingOverride
		}
		if attempt != 0 && identity != endpoint+accountID {
			return errors.New("Codex account changed during refresh")
		}
		identity = endpoint + accountID
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", "Codex-Tunnel")
		if accountID != "" {
			req.Header.Set("ChatGPT-Account-Id", accountID)
			req.Header.Set("originator", "desk_tunnel")
			if region != "NO_CONSTRAINT" {
				req.Header.Set("x-openai-account-routing-override", region)
			}
		}
		start := time.Now()
		resp, e := client.Do(req)
		if e == nil {
			e = out.read(resp)
			resp.Body.Close()
		}
		recordHop(ctx, "POST", req.URL.Path, start, e)
		if attempt == 0 && accountID != "" && resp != nil && resp.StatusCode == 401 {
			continue
		}
		return e
	}
	return errors.New("Codex account refresh failed")
}

func (r runner) responsesAPIKey() string {
	f, e := os.Open(filepath.Join(r.home, "auth.json"))
	if e == nil {
		defer f.Close()
		var auth struct {
			Mode string `json:"auth_mode"`
			Key  string `json:"OPENAI_API_KEY"`
		}
		if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&auth) == nil && (auth.Mode == "apikey" || auth.Mode == "") && auth.Key != "" {
			return auth.Key
		}
	}
	for _, k := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func responseRejection(resp *http.Response) error {
	var envelope struct {
		Error  struct{ Code, Type, Param string }
		Detail string
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&envelope)
	code := envelope.Error.Code
	if !imageErrorCode.MatchString(code) {
		code = envelope.Error.Type
	}
	if !imageErrorCode.MatchString(code) {
		code = "responses_provider_error"
	}
	status := resp.StatusCode
	switch status {
	case 400, 403, 404, 408, 409, 413, 422, 429, 500, 502, 503, 504:
	default:
		status = 502
	}
	msg := fmt.Sprintf("Responses upstream returned HTTP %d", resp.StatusCode)
	if imageErrorCode.MatchString(envelope.Error.Param) {
		msg += " for " + envelope.Error.Param
	}
	return &responsesError{status, code, msg}
}
