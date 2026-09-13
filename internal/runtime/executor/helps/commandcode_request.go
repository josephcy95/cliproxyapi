package helps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Command Code CLI-mimic envelope constants. They mirror the values the real
// `command-code` CLI sends (verified against the 1.53.1 bundle) rather than the
// values third-party proxies guessed.
const (
	CommandCodeDefaultVersion    = "1.53.1"
	CommandCodeDefaultMaxTokens  = 64000
	CommandCodeDefaultWorkingDir = "/tmp"
	CommandCodeDefaultBaseURL    = "https://api.commandcode.ai"
	CommandCodeEmptySystemPrompt = " "
	CommandCodeCLIEnvironment    = "production"
	CommandCodeUserAgent         = "cli"

	// CommandCodeDefaultMode is the only mode valid for an ordinary
	// chat-completion request. Upstream validates this field against the enum
	// agent | learning | custom-agent | custom-agent-create | title-gen |
	// tool-desc | compact | vision, and rejects anything else with
	// 'Invalid option: expected one of "agent"|... at "mode"'. Do not change this
	// to a plausible-looking value such as "default".
	CommandCodeDefaultMode = "agent"

	// CommandCodeDefaultPromptCache is the only promptCache value the vendor CLI
	// ever sends. Upstream validates it with
	// 'Invalid input: expected "off" at "promptCache"', so "on" is rejected.
	CommandCodeDefaultPromptCache = "off"

	// CommandCodeDefaultPermissionMode is sent as permissionMode. The vendor CLI
	// always includes the key; the value itself has not been confirmed against a
	// live response.
	CommandCodeDefaultPermissionMode = "standard"
)

// CommandCodeEnvelopeInput carries the per-request values the envelope needs.
type CommandCodeEnvelopeInput struct {
	// Model is the upstream model name placed in params.model.
	Model string
	// ThreadID must be a real UUID and is shared with the x-session-id header.
	ThreadID string
	// WorkingDir fills config.workingDir; the project slug derives from it.
	WorkingDir string
	// Date fills config.date as YYYY-MM-DD. Defaults to today when empty.
	Date string
	// Platform fills config.environment with a platform word (win32, linux, darwin).
	Platform string
	// MaxTokens overrides the 64000 default when positive.
	MaxTokens int64
}

// BuildCommandCodeEnvelope converts an OpenAI Chat Completions payload into the
// Command Code CLI envelope. The envelope has exactly nine top-level keys
// (config, memory, taste, skills, permissionMode, threadId, mode, promptCache,
// params) because the gateway validates their presence.
func BuildCommandCodeEnvelope(payload []byte, in CommandCodeEnvelopeInput) ([]byte, error) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil, fmt.Errorf("commandcode: request payload is not valid JSON")
	}
	root := gjson.ParseBytes(payload)
	messages := root.Get("messages")
	if !messages.IsArray() {
		return nil, fmt.Errorf("commandcode: request payload has no messages array")
	}

	convertedMessages, systemText, errMessages := commandCodeMessages(messages.Array())
	if errMessages != nil {
		return nil, errMessages
	}

	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = strings.TrimSpace(root.Get("model").String())
	}
	if model == "" {
		return nil, fmt.Errorf("commandcode: request payload has no model")
	}

	workingDir := strings.TrimSpace(in.WorkingDir)
	if workingDir == "" {
		workingDir = CommandCodeDefaultWorkingDir
	}
	date := strings.TrimSpace(in.Date)
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	platform := strings.TrimSpace(in.Platform)
	if platform == "" {
		platform = CommandCodePlatform()
	}
	// The gateway injects roughly 7.5k tokens of its own system prompt when the
	// field is empty, so an absent system prompt becomes a single space.
	if systemText == "" {
		systemText = CommandCodeEmptySystemPrompt
	}

	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = commandCodeMaxTokens(root)
	}

	body := []byte(`{"config":{"workingDir":"","date":"","environment":"","structure":[],"isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[]},"memory":null,"taste":null,"skills":null,"permissionMode":"","threadId":"","mode":"","promptCache":"","params":{}}`)
	var errSet error
	set := func(path string, value any) {
		if errSet != nil {
			return
		}
		body, errSet = sjson.SetBytes(body, path, value)
	}

	set("config.workingDir", workingDir)
	set("config.date", date)
	set("config.environment", platform)
	set("permissionMode", CommandCodeDefaultPermissionMode)
	set("threadId", in.ThreadID)
	set("mode", CommandCodeDefaultMode)
	set("promptCache", CommandCodeDefaultPromptCache)
	set("params.model", model)
	body, errSet = setRaw(body, "params.messages", convertedMessages, errSet)
	set("params.system", systemText)
	set("params.max_tokens", maxTokens)
	set("params.stream", true)
	// Temperature and reasoning_effort are only forwarded when the client sent
	// them, matching the CLI's `...(value !== undefined ? {...} : {})` spread.
	if temperature := root.Get("temperature"); temperature.Exists() && temperature.Type == gjson.Number {
		set("params.temperature", temperature.Float())
	}
	if effort := strings.TrimSpace(root.Get("reasoning_effort").String()); effort != "" {
		set("params.reasoning_effort", effort)
	}
	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		if convertedTools := commandCodeTools(tools.Array()); len(convertedTools) > 0 {
			body, errSet = setRaw(body, "params.tools", convertedTools, errSet)
		}
	}
	if toolChoice := commandCodeToolChoice(root.Get("tool_choice")); len(toolChoice) > 0 {
		body, errSet = setRaw(body, "params.tool_choice", toolChoice, errSet)
	}
	if parallel := root.Get("parallel_tool_calls"); parallel.Type == gjson.True || parallel.Type == gjson.False {
		set("params.parallel_tool_calls", parallel.Bool())
	}
	if errSet != nil {
		return nil, fmt.Errorf("commandcode: failed to build request envelope: %w", errSet)
	}
	// A non-UUID threadId would be dropped by the upstream JSON parser, and the
	// header/body identity must match, so refuse to send an unusable one.
	if !commandCodeIsUUID(strings.TrimSpace(in.ThreadID)) {
		return nil, fmt.Errorf("commandcode: threadId must be a UUID")
	}
	return body, nil
}

func setRaw(body []byte, path string, value []byte, current error) ([]byte, error) {
	if current != nil {
		return body, current
	}
	updated, errSet := sjson.SetRawBytes(body, path, value)
	if errSet != nil {
		return body, errSet
	}
	return updated, nil
}

// commandCodeMaxTokens resolves the envelope's max_tokens from the client
// request, defaulting to the CLI's 64000.
func commandCodeMaxTokens(root gjson.Result) int64 {
	for _, path := range []string{"max_completion_tokens", "max_tokens"} {
		if value := root.Get(path); value.Exists() && value.Type == gjson.Number && value.Int() > 0 {
			return value.Int()
		}
	}
	return CommandCodeDefaultMaxTokens
}

// CommandCodePlatform returns the platform word the CLI reports in
// config.environment.
func CommandCodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	default:
		return runtime.GOOS
	}
}

// commandCodeMessages converts OpenAI messages into the CC block-array shape and
// returns the folded system prompt.
func commandCodeMessages(messages []gjson.Result) ([]byte, string, error) {
	toolNames := commandCodeToolNameMap(messages)
	systemParts := make([]string, 0, 2)
	out := []byte("[]")
	var errBuild error

	appendMessage := func(message []byte) {
		if errBuild != nil || len(message) == 0 {
			return
		}
		out, errBuild = sjson.SetRawBytes(out, "-1", message)
	}

	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		switch role {
		case "system", "developer":
			if text := commandCodeMessageText(message.Get("content")); text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			blocks := commandCodeContentBlocks(message.Get("content"), true)
			if len(blocks) == 0 {
				continue
			}
			appendMessage(commandCodeRoleMessage("user", blocks))
		case "assistant":
			blocks := make([]byte, 0, 3)
			// Reasoning must be replayed before text and tool calls: the gateway
			// rejects DeepSeek thinking-mode tool loops whose assistant turn lost
			// its reasoning block.
			if reasoning := strings.TrimSpace(message.Get("reasoning_content").String()); reasoning != "" {
				blocks = append(blocks, commandCodeReasoningBlock(reasoning)...)
			}
			blocks = append(blocks, commandCodeContentBlocks(message.Get("content"), false)...)
			blocks = append(blocks, commandCodeAssistantToolCallBlocks(message.Get("tool_calls"))...)
			if len(blocks) == 0 {
				continue
			}
			appendMessage(commandCodeRoleMessage("assistant", blocks))
		case "tool":
			block := commandCodeToolResultBlock(message, toolNames)
			if len(block) == 0 {
				continue
			}
			appendMessage(commandCodeRoleMessage("tool", block))
		}
	}
	if errBuild != nil {
		return nil, "", fmt.Errorf("commandcode: failed to convert messages: %w", errBuild)
	}
	return out, strings.Join(systemParts, "\n"), nil
}

// commandCodeJoinBlocks wraps a pre-rendered comma separated block list into a
// JSON array.
func commandCodeJoinBlocks(blocks []byte) []byte {
	return []byte("[" + string(blocks) + "]")
}

// commandCodeRoleMessage renders {role, content:[...]}.
func commandCodeRoleMessage(role string, blocks []byte) []byte {
	message := []byte(`{"role":"","content":[]}`)
	message, _ = sjson.SetBytes(message, "role", role)
	message, _ = sjson.SetRawBytes(message, "content", commandCodeJoinBlocks(blocks))
	return message
}

// commandCodeContentBlocks renders text/image blocks from an OpenAI content
// value, which may be a string or a block array. Images are only accepted for
// user messages.
func commandCodeContentBlocks(content gjson.Result, allowImages bool) []byte {
	blocks := make([]byte, 0, 2)
	appendBlock := func(block []byte) {
		if len(block) == 0 {
			return
		}
		if len(blocks) > 0 {
			blocks = append(blocks, ',')
		}
		blocks = append(blocks, block...)
	}

	switch {
	case content.Type == gjson.String:
		appendBlock(commandCodeTextBlock(content.String()))
	case content.IsArray():
		for _, part := range content.Array() {
			switch strings.ToLower(strings.TrimSpace(part.Get("type").String())) {
			case "text", "input_text", "output_text":
				text := part.Get("text")
				if !text.Exists() {
					text = part.Get("content")
				}
				appendBlock(commandCodeTextBlock(text.String()))
			case "image_url", "input_image", "image":
				if !allowImages {
					continue
				}
				appendBlock(commandCodeImageBlock(commandCodeImageURL(part)))
			}
		}
	}
	return blocks
}

// commandCodeImageURL extracts an image URL from an OpenAI image part, which may
// be the flat CLI shape or the nested image_url object.
func commandCodeImageURL(part gjson.Result) string {
	for _, path := range []string{"image_url.url", "image_url", "image", "url"} {
		value := part.Get(path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return strings.TrimSpace(value.String())
		}
	}
	return ""
}

// commandCodeMessageText flattens a message content value into plain text. It is
// used for system/developer messages, which the CLI folds into one string.
func commandCodeMessageText(content gjson.Result) string {
	switch {
	case content.Type == gjson.String:
		return strings.TrimSpace(content.String())
	case content.IsArray():
		parts := make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			text := part.Get("text")
			if !text.Exists() {
				text = part.Get("content")
			}
			if trimmed := strings.TrimSpace(text.String()); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func commandCodeTextBlock(text string) []byte {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	block := []byte(`{"type":"text","text":""}`)
	block, _ = sjson.SetBytes(block, "text", text)
	return block
}

func commandCodeReasoningBlock(text string) []byte {
	block := []byte(`{"type":"reasoning","text":""}`)
	block, _ = sjson.SetBytes(block, "text", text)
	return block
}

// commandCodeImageBlock renders the CLI's {type:"image", image} shape.
func commandCodeImageBlock(url string) []byte {
	if url == "" {
		return nil
	}
	block := []byte(`{"type":"image","image":""}`)
	block, _ = sjson.SetBytes(block, "image", url)
	return block
}

// commandCodeAssistantToolCallBlocks renders assistant tool calls. The upstream
// wire format expects the parsed arguments object, not the JSON string OpenAI
// clients send.
func commandCodeAssistantToolCallBlocks(toolCalls gjson.Result) []byte {
	if !toolCalls.IsArray() {
		return nil
	}
	blocks := make([]byte, 0, 2)
	for _, call := range toolCalls.Array() {
		input := commandCodeToolCallInput(call)
		block := []byte(`{"type":"tool-call","toolCallId":"","toolName":"","input":{}}`)
		var errSet error
		block, errSet = sjson.SetBytes(block, "toolCallId", strings.TrimSpace(call.Get("id").String()))
		if errSet != nil {
			continue
		}
		block, errSet = sjson.SetBytes(block, "toolName", strings.TrimSpace(call.Get("function.name").String()))
		if errSet != nil {
			continue
		}
		block, errSet = sjson.SetRawBytes(block, "input", input)
		if errSet != nil {
			continue
		}
		if len(blocks) > 0 {
			blocks = append(blocks, ',')
		}
		blocks = append(blocks, block...)
	}
	return blocks
}

// commandCodeToolCallInput returns the tool call arguments as a JSON object. An
// absent or empty argument string becomes {}, and arguments that are not valid
// JSON are preserved as a JSON string rather than dropped.
func commandCodeToolCallInput(call gjson.Result) []byte {
	raw := strings.TrimSpace(call.Get("function.arguments").String())
	if raw == "" {
		return []byte(`{}`)
	}
	if json.Valid([]byte(raw)) {
		return []byte(raw)
	}
	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return []byte(`{}`)
	}
	return encoded
}

// commandCodeToolResultBlock renders {type:"tool-result", ...} for a tool
// message. toolName is taken from the message when present, otherwise from the
// reverse map of prior assistant tool calls.
func commandCodeToolResultBlock(message gjson.Result, toolNames map[string]string) []byte {
	callID := strings.TrimSpace(message.Get("tool_call_id").String())
	toolName := strings.TrimSpace(message.Get("name").String())
	if toolName == "" && callID != "" {
		toolName = toolNames[callID]
	}
	value := commandCodeToolResultValue(message.Get("content"))
	outputType := "text"
	if message.Get("is_error").Bool() {
		outputType = "error-text"
	}

	block := []byte(`{"type":"tool-result","toolCallId":"","toolName":"","output":{"type":"","value":""}}`)
	var errSet error
	block, errSet = sjson.SetBytes(block, "toolCallId", callID)
	if errSet != nil {
		return nil
	}
	block, errSet = sjson.SetBytes(block, "toolName", toolName)
	if errSet != nil {
		return nil
	}
	block, errSet = sjson.SetBytes(block, "output.type", outputType)
	if errSet != nil {
		return nil
	}
	block, errSet = sjson.SetBytes(block, "output.value", value)
	if errSet != nil {
		return nil
	}
	return block
}

// commandCodeToolResultValue flattens tool output content into a string.
func commandCodeToolResultValue(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			text := part.Get("text")
			if !text.Exists() {
				text = part.Get("content")
			}
			parts = append(parts, text.String())
		}
		return strings.Join(parts, "")
	}
	if content.Exists() && content.Raw != "" && content.Raw != "null" {
		return content.Raw
	}
	return ""
}

// commandCodeToolNameMap reverses assistant tool calls into call-id -> tool-name.
func commandCodeToolNameMap(messages []gjson.Result) map[string]string {
	names := make(map[string]string)
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		for _, call := range message.Get("tool_calls").Array() {
			id := strings.TrimSpace(call.Get("id").String())
			name := strings.TrimSpace(call.Get("function.name").String())
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

// commandCodeTools converts OpenAI function tools into the CLI's flat shape.
// Non-function tools have no CLI equivalent and are skipped.
func commandCodeTools(tools []gjson.Result) []byte {
	out := make([]byte, 0, len(tools))
	for _, tool := range tools {
		if toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String())); toolType != "function" {
			continue
		}
		function := tool.Get("function")
		name := strings.TrimSpace(function.Get("name").String())
		if name == "" {
			continue
		}
		block := []byte(`{"type":"function","name":"","description":"","input_schema":{"type":"object"}}`)
		var errSet error
		block, errSet = sjson.SetBytes(block, "name", name)
		if errSet != nil {
			continue
		}
		block, errSet = sjson.SetBytes(block, "description", function.Get("description").String())
		if errSet != nil {
			continue
		}
		parameters := function.Get("parameters")
		if parameters.Exists() && parameters.Raw != "" && parameters.Raw != "null" {
			block, errSet = sjson.SetRawBytes(block, "input_schema", []byte(parameters.Raw))
			if errSet != nil {
				continue
			}
		}
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, block...)
	}
	return commandCodeJoinBlocks(out)
}

// commandCodeToolChoice maps the simple OpenAI tool_choice forms onto the CLI's
// shape. Object forms ({"type":"function","function":{...}}) have no verified
// CLI equivalent and are omitted.
func commandCodeToolChoice(choice gjson.Result) []byte {
	if choice.Type != gjson.String {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(choice.String())) {
	case "auto":
		return []byte(`{"type":"auto"}`)
	case "required", "any":
		return []byte(`{"type":"any"}`)
	default:
		return nil
	}
}

var (
	commandCodeDriveLetterPattern = regexp.MustCompile(`^[a-z]:`)
	commandCodeNonAlnumPattern    = regexp.MustCompile(`[^a-z0-9]+`)
)

// CommandCodeProjectSlug derives x-project-slug from a working directory the way
// the CLI does: lowercase, strip a leading drive letter, collapse runs of
// non-alphanumerics to "-", trim edge dashes, and fall back to "project".
func CommandCodeProjectSlug(workingDir string) string {
	slug := strings.ToLower(strings.TrimSpace(workingDir))
	slug = commandCodeDriveLetterPattern.ReplaceAllString(slug, "")
	slug = commandCodeNonAlnumPattern.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "project"
	}
	return slug
}

// CommandCodeSessionUUID derives the stable per-auth session UUID shared by the
// x-session-id header and the envelope's threadId. Deriving it from the auth ID
// keeps prompt-cache affinity stable across restarts without a cache.
func CommandCodeSessionUUID(authID string) string {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:commandcode:session\x00"+authID)).String()
}

func commandCodeIsUUID(value string) bool {
	if value == "" {
		return false
	}
	_, errParse := uuid.Parse(value)
	return errParse == nil
}

// CommandCodeModelUpstreamModel maps a client-visible alias onto the configured
// upstream model name. Aliases are resolved here because the shared API-key
// alias table only knows the OpenAI-compatible provider shape.
func CommandCodeModelUpstreamModel[T CommandCodeModelEntry](models []T, requested, fallback string) string {
	requested = strings.TrimSpace(requested)
	for _, model := range models {
		name := strings.TrimSpace(model.GetName())
		alias := strings.TrimSpace(model.GetAlias())
		if name == "" {
			continue
		}
		if alias == "" {
			alias = name
		}
		if strings.EqualFold(alias, requested) || strings.EqualFold(name, requested) {
			return name
		}
	}
	if requested != "" {
		return requested
	}
	return strings.TrimSpace(fallback)
}

// CommandCodeModelEntry is the minimal model shape alias resolution needs.
type CommandCodeModelEntry interface {
	GetName() string
	GetAlias() string
}

// ApplyCommandCodeHeaders sets the CLI header set the real `command-code` CLI
// sends. It deliberately omits x-co-flag, which does not exist in the CLI.
//
// Configuration headers are applied by the caller afterwards, so the
// "x-command-code-version" and "x-project-slug" values configured on the entry
// act as overrides of these defaults rather than being overwritten by them.
func ApplyCommandCodeHeaders(req *http.Request, entry *config.CommandCodeKey, apiKey, sessionID string) {
	if req == nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", CommandCodeUserAgent)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	version := CommandCodeHeaderOverride(entry, "x-command-code-version")
	if version == "" {
		version = CommandCodeDefaultVersion
	}
	req.Header.Set("x-command-code-version", version)
	req.Header.Set("x-cli-environment", CommandCodeCLIEnvironment)
	slug := CommandCodeHeaderOverride(entry, "x-project-slug")
	if slug == "" {
		slug = CommandCodeProjectSlug(CommandCodeDefaultWorkingDir)
	}
	req.Header.Set("x-project-slug", slug)
	req.Header.Set("x-taste-learning", "true")
	if sessionID != "" {
		req.Header.Set("x-session-id", sessionID)
	}
}

// CommandCodeHeaderOverride returns a configured header value by name.
func CommandCodeHeaderOverride(entry *config.CommandCodeKey, name string) string {
	if entry == nil {
		return ""
	}
	for key, value := range entry.Headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
