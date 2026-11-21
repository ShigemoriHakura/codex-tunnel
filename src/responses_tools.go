package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

type clientToolName struct{ Namespace, Name string }

type clientTool struct {
	clientToolName
	custom bool
}

var clientToolIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Dynamic tools share the CLI's registry with built-ins. Give client tools
// their own stable identifiers, including during replay of older responses.
func nativeToolIdentifier(name string, limit int) string {
	if name == "" {
		return ""
	}
	if len(name)+3 <= limit && clientToolIdentifier.MatchString(name) && !strings.Contains(name, ".") {
		return "ct_" + name
	}
	digest := sha256.Sum256([]byte(name))
	return "cth_" + hex.EncodeToString(digest[:24])
}

func (n clientToolName) native() clientToolName {
	return clientToolName{nativeToolIdentifier(n.Namespace, 64), nativeToolIdentifier(n.Name, 128)}
}

func toolOption(raw json.RawMessage, path string, dst any) error {
	if err := strictOption(raw, dst); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), `json: unknown field "`); ok {
			field = strings.TrimSuffix(field, `"`)
			if responseFieldName.MatchString(field) {
				return invalidResponse("Unsupported tool field " + path + "." + field)
			}
		}
		return invalidResponse("Invalid tool declaration at " + path)
	}
	return nil
}

type clientFunctionSpec struct {
	Type, Name, Description string
	Parameters              json.RawMessage
	Strict                  bool
	Deferred                bool `json:"defer_loading"`
}

func (t *turn) clientCallable(raw json.RawMessage, ns, path string) (M, error) {
	var kind struct{ Type string }
	if json.Unmarshal(raw, &kind) != nil {
		return nil, invalidResponse("Invalid tool declaration at " + path)
	}
	var f clientFunctionSpec
	switch kind.Type {
	case "function":
		if err := toolOption(raw, path, &f); err != nil {
			return nil, err
		}
	case "custom":
		var c struct {
			Type, Name, Description string
			Deferred                bool `json:"defer_loading"`
			Format                  *struct{ Type, Syntax, Definition string }
		}
		if err := toolOption(raw, path, &c); err != nil {
			return nil, err
		}
		input := M{"type": "string"}
		if g := c.Format; g != nil {
			switch g.Type {
			case "text":
			case "grammar":
				if (g.Syntax != "lark" && g.Syntax != "regex") || g.Definition == "" {
					return nil, invalidResponse(path + ".format requires a lark or regex grammar definition")
				}
				input["description"] = "The raw tool input must match this " + g.Syntax + " grammar:\n" + g.Definition
			default:
				return nil, invalidResponse(path + ".format must use text or grammar")
			}
		}
		f = clientFunctionSpec{Name: c.Name, Description: c.Description, Deferred: c.Deferred,
			Parameters: rawJSON(M{"type": "object", "properties": M{"input": input}, "required": []string{"input"}, "additionalProperties": false})}
	default:
		return nil, invalidResponse(path + " must be a function or custom tool")
	}
	if len(f.Name) > 128 || !clientToolIdentifier.MatchString(f.Name) {
		return nil, invalidResponse(path + " requires a valid tool name")
	}
	if f.Strict {
		return nil, invalidResponse(path + ".strict=true is unavailable for CLI dynamic tools")
	}
	if f.Deferred && ns == "" {
		return nil, invalidResponse(path + ".defer_loading requires a namespace")
	}
	if f.Parameters == nil {
		f.Parameters = rawJSON(M{"type": "object", "properties": M{}})
	}
	if f.Parameters[0] != '{' {
		return nil, invalidResponse(path + ".parameters must be a JSON Schema object")
	}
	name := clientToolName{ns, f.Name}
	native := name.native()
	if _, exists := t.clientTools[native]; exists {
		return nil, invalidResponse("Duplicate function at " + path)
	}
	t.clientTools[native] = clientTool{name, kind.Type == "custom"}
	spec := M{"type": "function", "name": native.Name, "description": f.Description, "inputSchema": f.Parameters}
	if f.Deferred {
		spec["deferLoading"] = true
	}
	return spec, nil
}

func (t *turn) clientNamespace(raw json.RawMessage, path string) (M, error) {
	var ns struct {
		Type, Name, Description string
		Tools                   []json.RawMessage
	}
	if err := toolOption(raw, path, &ns); err != nil {
		return nil, err
	}
	if !clientToolIdentifier.MatchString(ns.Name) || len(ns.Name) > 64 || len(ns.Tools) == 0 {
		return nil, invalidResponse(path + " requires a namespace name and a nonempty array of tools")
	}
	children := make([]M, 0, len(ns.Tools))
	for i, child := range ns.Tools {
		f, err := t.clientCallable(child, ns.Name, fmt.Sprintf("%s.tools[%d]", path, i))
		if err != nil {
			return nil, err
		}
		children = append(children, f)
	}
	description := fitNamespaceDescription(ns.Description, children)
	return M{"type": "namespace", "name": nativeToolIdentifier(ns.Name, 64), "description": description, "tools": children}, nil
}

// app-server limits the namespace field to 1024 Unicode code points. Keep
// longer documentation in tool metadata, with one copy on a visible member
// when possible. Function descriptions have no corresponding protocol limit.
func fitNamespaceDescription(description string, children []M) string {
	const limit = 1024
	count := 0
	for end := range description {
		if count == limit {
			member := children[0]
			for _, child := range children {
				if child["deferLoading"] != true {
					member = child
					break
				}
			}
			member["description"] = member["description"].(string) + "\n\nShared namespace documentation:\n" + description
			return description[:end]
		}
		count++
	}
	return description
}

func (b *responsesRequest) parseTools(f responseFields, additional []json.RawMessage) error {
	var tools []json.RawMessage
	if raw := f["tools"]; len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return invalidResponse("tools must be an array of tool declarations")
	}
	t := &b.options
	t.clientTools = make(map[clientToolName]clientTool)
	names := map[string]bool{}
	for i, raw := range append(tools, additional...) {
		path := fmt.Sprintf("tools[%d]", i)
		var kind struct{ Type, Name string }
		if json.Unmarshal(raw, &kind) != nil || !responseFieldName.MatchString(kind.Type) {
			return invalidResponse(path + " requires a tool type")
		}
		var spec M
		var err error
		switch kind.Type {
		case "function", "custom":
			spec, err = t.clientCallable(raw, "", path)
		case "namespace":
			spec, err = t.clientNamespace(raw, path)
		case "web_search":
			if t.webSearch != "" {
				return invalidResponse("Duplicate web_search tool")
			}
			t.webSearch, err = webSearchMode(raw, path)
		default:
			return invalidResponse("Unsupported tool type " + kind.Type + " at " + path)
		}
		if err != nil {
			return err
		}
		if spec != nil {
			if names[kind.Name] {
				return invalidResponse("Duplicate tool name at " + path)
			}
			names[kind.Name] = true
			t.tools = append(t.tools, spec)
		}
	}
	if raw := f["tool_choice"]; len(raw) != 0 {
		var choice string
		if json.Unmarshal(raw, &choice) != nil || choice != "auto" && choice != "none" {
			return invalidResponse("The CLI adapter supports tool_choice auto and none")
		}
		if choice == "none" {
			t.tools, t.clientTools, t.webSearch = nil, nil, ""
		}
	}
	return nil
}

func webSearchMode(raw json.RawMessage, path string) (string, error) {
	var w struct {
		Type     string
		External *bool `json:"external_web_access"`
		Indexed  bool  `json:"indexed_web_access"`
	}
	if err := toolOption(raw, path, &w); err != nil {
		return "", err
	}
	if w.External != nil && !*w.External {
		if w.Indexed {
			return "", invalidResponse(path + " requests indexed access with external_web_access=false")
		}
		return "cached", nil
	}
	if w.Indexed {
		return "indexed", nil
	}
	return "live", nil
}

func nativeToolHistory(items responseItems) (responseItems, error) {
	var history responseItems
	for i, raw := range items {
		var call struct{ Type, Name, Namespace string }
		if err := json.Unmarshal(raw, &call); err != nil {
			return nil, err
		}
		if call.Type != "function_call" && call.Type != "custom_tool_call" && call.Type != "custom_tool_call_output" {
			continue
		}
		if history == nil {
			history = append(responseItems(nil), items...)
		}
		var item responseFields
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		switch call.Type {
		case "custom_tool_call_output":
			history[i] = rawJSON(responseFields{"type": rawJSON("function_call_output"), "call_id": item["call_id"], "output": item["output"]})
			continue
		case "custom_tool_call":
			item["type"] = rawJSON("function_call")
			item["arguments"] = rawJSON(string(rawJSON(responseFields{"input": item["input"]})))
			delete(item, "input")
		}
		native := (clientToolName{call.Namespace, call.Name}).native()
		item["name"] = rawJSON(native.Name)
		if native.Namespace != "" {
			item["namespace"] = rawJSON(native.Namespace)
		}
		history[i] = rawJSON(item)
	}
	if history == nil {
		return items, nil
	}
	return history, nil
}

// app-server reports camelCase actions on public webSearch thread items.
func (s *responseStream) webSearchItem(key string, done bool, raw json.RawMessage, query string, results json.RawMessage) error {
	if key == "" {
		return fmt.Errorf("CLI web search item has no ID")
	}
	if s.searchItems == nil {
		s.searchItems = make(map[string]int)
	}
	index, exists := s.searchItems[key]
	if !exists {
		index = len(s.items)
		s.searchItems[key] = index
		start := M{"type": "web_search_call", "id": key, "status": "in_progress"}
		s.items = append(s.items, rawJSON(start))
		if err := s.send("response.output_item.added", M{"output_index": index, "item": start}); err != nil {
			return err
		}
		if err := s.send("response.web_search_call.in_progress", M{"output_index": index, "item_id": key}); err != nil {
			return err
		}
	}
	if !done {
		return nil
	}
	item := M{"type": "web_search_call", "id": key, "status": "completed"}
	var action responseFields
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &action); err != nil {
			return err
		}
	}
	var kind string
	_ = json.Unmarshal(action["type"], &kind)
	switch kind {
	case "openPage":
		action["type"] = rawJSON("open_page")
	case "findInPage":
		action["type"] = rawJSON("find_in_page")
	case "search":
	default:
		action = nil
		if query != "" {
			action = responseFields{"type": rawJSON("search"), "query": rawJSON(query)}
		}
	}
	if action != nil {
		item["action"] = action
	}
	if len(results) > 0 && string(results) != "null" {
		item["results"] = results
	}
	s.items[index] = rawJSON(item)
	delete(s.searchItems, key)
	if err := s.send("response.web_search_call.completed", M{"output_index": index, "item_id": key}); err != nil {
		return err
	}
	return s.send("response.output_item.done", M{"output_index": index, "item": item})
}

// A dynamic function can interrupt the native turn while a search is in progress.
func (s *responseStream) finishPendingSearches() error {
	for _, key := range slices.SortedFunc(maps.Keys(s.searchItems), func(a, b string) int {
		return s.searchItems[a] - s.searchItems[b]
	}) {
		index := s.searchItems[key]
		item := M{"type": "web_search_call", "id": key, "status": "incomplete"}
		s.items[index] = rawJSON(item)
		if err := s.send("response.output_item.done", M{"output_index": index, "item": item}); err != nil {
			return err
		}
		delete(s.searchItems, key)
	}
	return nil
}
