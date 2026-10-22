package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Free-tier gate facts carried over from the original server:
//   - the upstream requires the built-in read tool by name and schema shape;
//     renaming it or dropping its properties yields HTTP 403;
//   - only the read tool passes the gate: shell/edit/write/glob equivalents are
//     rejected, so it is the only reserved tool injected;
//   - explicit tool_choice "none" is rejected too, so a disabled-tools request
//     is expressed by removing client tools, keeping only the reserved tool,
//     and enforcing the no-tool-call contract in the response layer.
const gateToolName = "read"

// The upstream does not validate the description text (an empty description is
// accepted too), so the most explicit wording is used to stop model-side tool
// selection; response-layer enforcement is the actual guarantee.
const gateToolDescription = "Reserved for the host runtime; do not call or select this function. Use tools explicitly supplied by the user instead."

func gateTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        gateToolName,
			"description": gateToolDescription,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "description": "File or directory to read"},
					"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Starting line or directory entry (1-based)"},
					"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "Maximum lines or directory entries to read"},
				},
				"required":             []any{"path"},
				"additionalProperties": false,
			},
			"strict": false,
		},
	}
}

// Keys that have special meaning in JS runtimes carry no business value; they
// are stripped before forwarding to avoid polluting the upstream payload.
var dangerousKeys = []string{"__proto__", "constructor", "prototype"}

// upstreamRequest is the fully adapted outbound chat request. It also carries
// the reverse tool-name map and the reserved/client tool name lists used by the
// response layer to restore names and reject reserved tool calls.
type upstreamRequest struct {
	Body              []byte
	Aliases           map[string]string // oc2cpa_read → read (alias → original)
	ReservedToolNames []string
	ClientToolNames   []string
	ToolsDisabled     bool
	ThinkingEnabled   bool
	StreamRequested   bool
	ChoiceCount       int
	Model             string
	RequestID         string
	SessionID         string
	ProxyURL          string
}

// buildUpstreamRequest adapts the (already OpenAI chat-completions shaped)
// payload into the upstream body. It is the faithful port of zen.js
// buildUpstreamRequest + handler.chat request-side validation.
func buildUpstreamRequest(ctx context.Context, payload []byte, requestedModel string, auth ocAuthView) (*upstreamRequest, error) {
	if !isAllowedModelID(requestedModel) {
		return nil, &ocError{Code: "invalid_request_error", Message: "Only big-pickle and models ending in -free are allowed", HTTPStatus: 400}
	}
	decoded := decodeJSONUseNumber(payload)
	if decoded == nil {
		return nil, &ocError{Code: "invalid_request_error", Message: "Invalid JSON body", HTTPStatus: 400}
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		return nil, &ocError{Code: "invalid_request_error", Message: "Invalid JSON body", HTTPStatus: 400}
	}
	// The routed model wins over the id in the body: the host may hand over a
	// model alias or a prefixed id, and the upstream only accepts its own names.
	if requestedModel != "" {
		root["model"] = requestedModel
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, &ocError{Code: "invalid_request_error", Message: "messages must be a non-empty array", HTTPStatus: 400}
	}

	streamRequested := root["stream"] == true
	choiceCount := 0
	if number, ok := asInt(root["n"]); ok && number > 0 {
		choiceCount = number
	}
	// reasoning_effort defaults to enabled; only an explicit "none" drops thinking content.
	reasoningEffort := pickReasoningEffort(root)
	toolsDisabled := areToolsDisabled(root)

	toolPhase := withGateTools(toolsDisabled, toolsOf(root))
	root["tools"] = toolPhase.tools
	if toolsDisabled || root["tool_choice"] == nil {
		delete(root, "tool_choice")
	} else {
		root["tool_choice"] = renameToolChoice(root["tool_choice"], toolPhase.names)
	}
	// The message rewrite must run even when there are no client tools: a
	// name field on role:"tool" makes GLM reject the whole request.
	for index, message := range messages {
		messages[index] = renameMessageTools(message, toolPhase.names)
	}
	root["stream"] = true
	root["stream_options"] = map[string]any{"include_usage": true}
	root["reasoning_effort"] = primaryKeyReasoningEffort(root, reasoningEffort)
	if root["max_tokens"] == nil && root["max_completion_tokens"] == nil {
		root["max_tokens"] = 32000
	}
	for _, key := range dangerousKeys {
		delete(root, key)
	}

	aliases := make(map[string]string, len(toolPhase.names))
	for original, alias := range toolPhase.names {
		aliases[alias] = original
	}
	clientToolNames := make([]string, 0, len(toolPhase.tools))
	for alias := range aliases {
		if alias == gateToolName {
			continue
		}
		clientToolNames = append(clientToolNames, alias)
	}
	body, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream body: %w", err)
	}
	return &upstreamRequest{
		Body:              body,
		Aliases:           aliases,
		ReservedToolNames: []string{gateToolName},
		ClientToolNames:   clientToolNames,
		ToolsDisabled:     toolsDisabled,
		ThinkingEnabled:   reasoningEffort != "none",
		StreamRequested:   streamRequested,
		ChoiceCount:       choiceCount,
		Model:             requestedModel,
		RequestID:         ocRequestID(),
		SessionID:         sessionRegistryDefault.get(auth.Holder),
		ProxyURL:          auth.ProxyURL,
	}, nil
}

func pickReasoningEffort(root map[string]any) any {
	if value, ok := root["reasoning_effort"]; ok {
		return value
	}
	return root["reasoningEffort"]
}

func primaryKeyReasoningEffort(root map[string]any, effort any) any {
	delete(root, "reasoningEffort")
	if effort == nil {
		delete(root, "reasoning_effort")
		return nil
	}
	if text, ok := effort.(string); ok && text == "" {
		delete(root, "reasoning_effort")
		return nil
	}
	return effort
}

// toolsOf narrows the payload tools list (nil when absent or malformed).
func toolsOf(root map[string]any) []any {
	tools, ok := root["tools"].([]any)
	if !ok {
		return nil
	}
	return tools
}

func areToolsDisabled(root map[string]any) bool {
	choice, hasChoice := root["tool_choice"]
	if hasChoice && choice == "none" {
		return true
	}
	tools := toolsOf(root)
	return len(tools) == 0
}

// gatePhase carries the adapted tool list and the alias map.
type gatePhase struct {
	tools []any
	names map[string]string // original → alias
}

// withGateTools renames client tool objects into oc2cpa_* aliases and appends
// the reserved gate tool. Duplicate names are skipped; alias collisions are
// resolved with _1, _2 suffixes (the same algorithm as the original).
func withGateTools(toolsDisabled bool, clientTools []any) gatePhase {
	live := make([]any, 0, len(clientTools))
	if !toolsDisabled {
		for _, raw := range clientTools {
			if asObject(raw) != nil {
				live = append(live, raw)
			}
		}
	}
	// JS parity: present starts with the clients' actual function names, so
	// two names that sanitize to the same alias base (read / re.ad) get _1,
	// _2 suffixes.
	names := make(map[string]string, len(live))
	present := make(map[string]bool, len(live))
	for _, raw := range live {
		if name := functionName(asObject(raw)); name != "" {
			present[name] = true
		}
	}
	for _, raw := range live {
		tool := asObject(raw)
		name := functionName(tool)
		if name == "" {
			continue
		}
		if _, seen := names[name]; seen {
			continue
		}
		base := ("oc2cpa_" + sanitizeToolName(name))[:clampIndex("oc2cpa_"+sanitizeToolName(name), 60)]
		alias := base
		suffix := 1
		for present[alias] {
			alias = base[:clampIndex(base, 64-len(fmt.Sprintf("_%d", suffix)))] + fmt.Sprintf("_%d", suffix)
			suffix++
		}
		present[alias] = true
		names[name] = alias
	}
	result := make([]any, 0, len(live))
	for _, raw := range live {
		tool := asObject(raw)
		renamed := renameToolCall(tool, names)
		object := renamed
		if originalName, ok := names[functionName(tool)]; ok {
			description := ""
			if fn := asObject(asObject(object)["function"]); fn != nil {
				if raw, isStr := fn["description"].(string); isStr {
					description = raw
				}
			}
			object = withToolDescription(object, originalName, functionName(asObject(renamed)), description)
		}
		if object == nil {
			continue
		}
		result = append(result, object)
	}
	// The gate tool is always appended last, even for disabled requests where
	// the response layer rejects any tool call.
	result = append(result, gateTool())
	return gatePhase{tools: result, names: names}
}

func withToolDescription(tool any, originalName, aliasName, description string) any {
	object := copyObject(asObject(tool))
	function := copyObject(asObject(object["function"]))
	function["description"] = fmt.Sprintf("Client tool %q; use %q for the client's %q operation. %s", originalName, aliasName, originalName, description)
	object["function"] = function
	return object
}

// functionName returns the tool's function.name field when present.
func functionName(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

func sanitizeToolName(name string) string {
	keep := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}
	return strings.Map(keep, name)
}

func clampIndex(value string, length int) int {
	if length > len(value) {
		return len(value)
	}
	return length
}

func renameToolCall(call any, names map[string]string) any {
	obj := asObject(call)
	if obj == nil {
		return call
	}
	fn, ok := obj["function"].(map[string]any)
	if !ok {
		return call
	}
	name, _ := fn["name"].(string)
	alias, found := names[name]
	if !found {
		return call
	}
	clone := copyObject(obj)
	clone["function"] = renameFunction(clone["function"], alias)
	return clone
}

func renameFunction(fn any, name string) any {
	obj, ok := fn.(map[string]any)
	if !ok {
		return fn
	}
	clone := copyObject(obj)
	clone["name"] = name
	return clone
}

// renameToolChoice mirrors the original: named tool_choice objects are renamed
// through renameToolCall, and any embedded `tools` array entries are renamed
// through both the function-name path and the legacy direct-name path.
func renameToolChoice(choice any, names map[string]string) any {
	renamed := renameToolCall(choice, names)
	if renamed == nil {
		return choice
	}
	root, ok := renamed.(map[string]any)
	if !ok {
		return renamed
	}
	if rawTools, ok := root["tools"].([]any); ok {
		rewritten := make([]any, 0, len(rawTools))
		for _, tool := range rawTools {
			rewritten = append(rewritten, renameToolObjectWithTools(tool, names))
		}
		root["tools"] = rewritten
	}
	return renamed
}

// renameToolObjectWithTools mirrors the second half of the original renameToolChoice:
// legacy tool entries addressed by a direct `name` property (instead of nested
// function.name) are renamed when the name is a known original name.
func renameToolObjectWithTools(tool any, names map[string]string) any {
	next := renameToolCall(tool, names)
	if name, ok := asObject(next)["name"].(string); ok {
		if alias, found := names[name]; found {
			clone := copyObject(asObject(next))
			clone["name"] = alias
			return clone
		}
	}
	return next
}

// renameMessageTools strips the `name` field from role:"tool" messages (GLM
// 400s on it while DeepSeek tolerates it) and renames legacy role:"function"
// names because that name is required there.
func renameMessageTools(message any, names map[string]string) any {
	obj := asObject(message)
	if obj == nil {
		return message
	}
	role, _ := obj["role"].(string)
	clone := copyObject(obj)
	switch role {
	case "tool":
		// A name on role:"tool" messages is rejected by GLM outright, so it is
		// always dropped regardless of client tools.
		delete(clone, "name")
		return clone
	case "function":
		name, ok := clone["name"].(string)
		if !ok {
			return clone
		}
		alias, found := names[name]
		if !found {
			return clone
		}
		clone["name"] = alias
		return clone
	default:
		if _, has := obj["tool_calls"]; has {
			toolCalls, _ := obj["tool_calls"].([]any)
			rewritten := make([]any, 0, len(toolCalls))
			for _, call := range toolCalls {
				rewritten = append(rewritten, renameToolCall(call, names))
			}
			clone["tool_calls"] = rewritten
			return clone
		}
		if functionCall, ok := obj["function_call"].(map[string]any); ok {
			if name, isStr := functionCall["name"].(string); isStr {
				if alias, found := names[name]; found {
					call := copyObject(functionCall)
					call["name"] = alias
					clone["function_call"] = call
					return clone
				}
			}
		}
		return clone
	}
}

func asObject(value any) map[string]any {
	conv, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return conv
}

func copyObject(obj map[string]any) map[string]any {
	clone := make(map[string]any, len(obj))
	for key, value := range obj {
		clone[key] = value
	}
	return clone
}

func asInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case json.Number:
		if parsed, err := number.Int64(); err == nil {
			return int(parsed), true
		}
	case float64:
		return int(number), true
	}
	return 0, false
}

// decodeJSONUseNumber keeps integer width stable when round-tripping payloads.
func decodeJSONUseNumber(data []byte) any {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return nil
	}
	return out
}

// ocRequestID mirrors the original req_ id layout: req_<hex timestamp><16 char
// base64url-ish tail>. It is only used as the deterministic fallback id.
func ocRequestID() string {
	return "req_" + fmt.Sprintf("%x", time.Now().UnixNano()/1_000_000) + randomBase64URLTail(16)
}

func randomBase64URLTail(length int) string {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i)
		}
	}
	encoded := base64.RawURLEncoding.EncodeToString(buf)
	return encoded[:min(len(encoded), length)]
}
