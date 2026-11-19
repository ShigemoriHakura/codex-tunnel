package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const imagesPath = "/backend-api/codex/images/generations"

type imageProviderError struct {
	status            int
	code              string
	param             string
	streamUnsupported bool
}

func (e *imageProviderError) Error() string {
	msg := fmt.Sprintf("Codex Images returned HTTP %d", e.status)
	if e.code != "" {
		msg += " (" + e.code + ")"
	}
	if e.param != "" {
		msg += " for " + e.param
	}
	return msg
}

func (b imageRequest) providerRequest() imageRequest {
	b.ResponseFormat = "" // GPT image models always return base64.
	if b.Format == "jpg" {
		b.Format = "jpeg"
	}
	return b
}

type imageWorkspace struct {
	BackendOrigin, ChatGPTAccountID, AccountRoutingOverride string
}

func (ws imageWorkspace) endpoint() (string, error) {
	u, err := url.Parse(ws.BackendOrigin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Hostname() != "chatgpt.com" && !strings.HasSuffix(u.Hostname(), ".chatgpt.com")) || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || ws.ChatGPTAccountID == "" {
		return "", errors.New("unrecognized Codex Images workspace origin")
	}
	switch ws.AccountRoutingOverride {
	case "NO_CONSTRAINT", "us", "us_cr":
	default:
		return "", errors.New("unsupported Codex Images workspace routing")
	}
	u.Path = imagesPath
	return u.String(), nil
}

// CLI account discovery owns refresh and workspace selection. Read only the active
// access token, and require its account to match that discovery before sending it.
func (r runner) imageWorkspace(p *rpc, refresh bool) (imageWorkspace, string, error) {
	var account struct {
		Account          struct{ Type string }
		WorkspaceRouting imageWorkspace
	}
	raw, err := p.call("account/read", M{"refreshToken": refresh}, nil)
	if err != nil {
		return imageWorkspace{}, "", err
	}
	if err = json.Unmarshal(raw, &account); err != nil {
		return imageWorkspace{}, "", err
	}
	ws := account.WorkspaceRouting
	if account.Account.Type != "chatgpt" || ws.ChatGPTAccountID == "" {
		return ws, "", errors.New("named image models require a signed-in Codex ChatGPT workspace")
	}
	f, err := os.Open(filepath.Join(r.home, "auth.json"))
	if err != nil {
		return ws, "", errors.New("Codex login unavailable")
	}
	defer f.Close()
	var auth struct {
		Mode   string `json:"auth_mode"`
		Tokens struct {
			Access  string `json:"access_token"`
			Account string `json:"account_id"`
		}
	}
	if err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&auth); err != nil || auth.Mode != "chatgpt" || auth.Tokens.Access == "" || auth.Tokens.Account != ws.ChatGPTAccountID {
		return ws, "", errors.New("Codex credential does not match the selected image workspace")
	}
	return ws, auth.Tokens.Access, nil
}

// Forward explicit image engines to the same first-party Images endpoint the CLI
// uses. Its model-facing image tool hardcodes its own engine and generation settings.
func (a *app) generateNamedImage(ctx context.Context, r runner, b imageRequest, partial func(imageEvent) error) (result, error) {
	p, err := r.open(ctx, false)
	if err != nil {
		return result{}, err
	}
	defer p.close()
	params := b.providerRequest()
	client := http.Client{Transport: a.imageTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var originalAccount string
	turnID := id("")
	turnID = fmt.Sprintf("%s-%s-4%s-8%s-%s", turnID[:8], turnID[8:12], turnID[13:16], turnID[17:20], turnID[20:32])
	refreshed, refresh := false, false
	for {
		ws, token, err := r.imageWorkspace(p, refresh)
		refresh = false
		if err != nil {
			return result{}, err
		}
		if originalAccount != "" && ws.ChatGPTAccountID != originalAccount {
			return result{}, errors.New("Codex account changed while refreshing image credentials")
		}
		originalAccount = ws.ChatGPTAccountID
		endpoint, err := ws.endpoint()
		if err != nil {
			return result{}, err
		}
		payload, err := json.Marshal(params)
		if err != nil {
			return result{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return result{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("ChatGPT-Account-Id", ws.ChatGPTAccountID)
		req.Header.Set("Content-Type", "application/json")
		if params.Stream {
			req.Header.Set("Accept", "text/event-stream, application/json")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		req.Header.Set("User-Agent", "Codex-Tunnel")
		req.Header.Set("originator", "desk_tunnel")
		req.Header.Set("x-codex-image-turn-id", turnID)
		if ws.AccountRoutingOverride != "NO_CONSTRAINT" {
			req.Header.Set("x-openai-account-routing-override", ws.AccountRoutingOverride)
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			recordHop(ctx, "POST", imagesPath, start, err)
			return result{}, err
		}
		var res result
		if resp.StatusCode == http.StatusOK && b.Stream && isEventStream(resp.Header.Get("Content-Type")) {
			res, err = readImageEvents(resp.Body, b, partial)
		} else {
			res, err = readProviderImage(resp, b)
		}
		resp.Body.Close()
		recordHop(ctx, "POST", imagesPath, start, err)
		if !refreshed && resp.StatusCode == http.StatusUnauthorized {
			refreshed, refresh = true, true
			continue
		}
		// Retry only an explicit HTTP parameter rejection, before any image was accepted.
		var rejection *imageProviderError
		if params.Stream && (resp.StatusCode == 400 || resp.StatusCode == 422) && errors.As(err, &rejection) && rejection.streamUnsupported {
			params.Stream = false
			params.PartialImages = nil
			continue
		}
		return res, err
	}
}

var imageErrorCode = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

func readProviderImage(resp *http.Response, b imageRequest) (result, error) {
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error  struct{ Code, Type, Param, Message string }
			Detail json.RawMessage
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&body)
		code, param, msg := body.Error.Code, body.Error.Param, strings.ToLower(body.Error.Message)
		if msg == "" {
			var detail string
			if json.Unmarshal(body.Detail, &detail) == nil {
				msg = strings.ToLower(detail)
			}
		}
		// Some hosts use a FastAPI validation envelope for fields they do not accept.
		var fields []struct {
			Loc  []string
			Type string
		}
		if json.Unmarshal(body.Detail, &fields) == nil && len(fields) == 1 && len(fields[0].Loc) > 0 {
			param = fields[0].Loc[len(fields[0].Loc)-1]
			code = fields[0].Type
		}
		if len(fields) > 1 {
			onlyStream := true
			for _, field := range fields {
				if field.Type != "extra_forbidden" || len(field.Loc) == 0 {
					onlyStream = false
					break
				}
				key := field.Loc[len(field.Loc)-1]
				if key != "stream" && key != "partial_images" {
					onlyStream = false
					break
				}
			}
			if onlyStream {
				param, code = "stream", "extra_forbidden"
			}
		}
		if param == "" {
			for _, field := range []string{"stream", "partial_images", "model", "size", "quality", "background", "output_format"} {
				if strings.Contains(msg, "'"+field+"'") || strings.Contains(msg, `"`+field+`"`) {
					param = field
					break
				}
			}
		}
		if code == "" {
			code = body.Error.Type
		}
		if !imageErrorCode.MatchString(code) {
			code = ""
		}
		if !imageErrorCode.MatchString(param) {
			param = ""
		}
		unsupported := code == "unknown_parameter" || code == "unsupported_parameter" || code == "extra_forbidden" || strings.Contains(msg, "not supported") || strings.Contains(msg, "unsupported") || strings.Contains(msg, "unknown parameter") || strings.Contains(msg, "unrecognized")
		if code == "" && strings.Contains(msg, "model") && (unsupported || strings.Contains(msg, "not available") || strings.Contains(msg, "not found") || strings.Contains(msg, "not allowed") || strings.Contains(msg, "does not exist")) {
			code, param = "model_unavailable", "model"
		}
		return result{}, &imageProviderError{status: resp.StatusCode, code: code, param: param, streamUnsupported: (param == "stream" || param == "partial_images") && unsupported}
	}
	// Bound the entire encoded JSON, including metadata, before decoding the binary.
	limited := &io.LimitedReader{R: resp.Body, N: maxImageEvent + 1}
	var reply struct {
		imageMetadata
		Created int64 `json:"created"`
		Model   string
		Data    []struct {
			Bytes []byte `json:"b64_json"`
		}
	}
	dec := json.NewDecoder(limited)
	if err := dec.Decode(&reply); err != nil || dec.Decode(new(json.RawMessage)) != io.EOF || limited.N == 0 {
		return result{}, errors.New("invalid or oversized Codex Images response")
	}
	if len(reply.Data) != 1 {
		return result{}, errNoImage
	}
	reply.imageMetadata.Created = reply.Created
	return b.providerResult(reply.Model, reply.Data[0].Bytes, reply.imageMetadata)
}

func (b imageRequest) providerResult(model string, data []byte, meta imageMetadata) (result, error) {
	if model != "" && model != b.Model && !strings.HasPrefix(model, b.Model+"-") {
		return result{}, errors.New("image provider reported a different model")
	}
	if len(data) == 0 || len(data) > maxGeneratedImageBytes {
		return result{}, errNoImage
	}
	format := b.imageOutput
	if err := format.validate(); err != nil {
		return result{}, err
	}
	// Upstream applies requested compression. Only convert if its actual encoding differs.
	if http.DetectContentType(data) != "image/"+format.Format {
		var err error
		data, err = format.encode(data)
		if err != nil {
			return result{}, err
		}
	}
	b.imageOutput = format
	res := result{image: data, model: b.Model, imageMeta: b.metadata(meta)}
	var u struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
	}
	if json.Unmarshal(meta.Usage, &u) == nil && u.Input != nil && u.Output != nil && *u.Input >= 0 && *u.Output >= 0 {
		res.metered = true
		res.total.Input = *u.Input
		res.total.Output = *u.Output
	}
	return res, nil
}
