package helps

import (
	"bytes"

	"github.com/MoeForge/moego/ojson"
)

// IsResponsesTokenEvent reports whether the given OpenAI/xAI Responses API WebSocket or SSE event
// payload carries an actual output token, reasoning trace, tool call argument, or multimodal delta,
// according to the official Responses API streaming / websocket specification plus Codex-compatible private events.
func IsResponsesTokenEvent(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}

	// Handle SSE line format (e.g., "data: {...}")
	if bytes.HasPrefix(payload, []byte("data:")) {
		prefixLen := len("data:")
		if bytes.HasPrefix(payload, []byte("data: ")) {
			prefixLen = len("data: ")
		}
		payload = bytes.TrimSpace(payload[prefixLen:])
		if len(payload) == 0 {
			return false
		}
	}

	eventType := ojson.GetBytesNoCopy(payload, "type").String()
	switch eventType {
	// Substantive text, reasoning, and tool argument streaming deltas
	case "response.reasoning_summary_text.delta",
		"response.reasoning.delta",
		"response.reasoning_text.delta",
		"response.output_text.delta",
		"response.text.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta",
		"response.code_interpreter_call_code.delta",
		"response.mcp_call_arguments.delta",
		"response.shell_call_command.delta",
		"response.refusal.delta",
		"response.audio.transcript.delta":
		return len(ojson.GetBytesNoCopy(payload, "delta").String()) > 0

	// Multimodal streaming deltas (audio and image generation preview)
	case "response.audio.delta":
		return len(ojson.GetBytesNoCopy(payload, "delta").String()) > 0 || len(ojson.GetBytesNoCopy(payload, "data").String()) > 0
	case "response.image_generation_call.partial_image":
		return len(ojson.GetBytesNoCopy(payload, "partial_image_b64").String()) > 0

	// Shell call command added event when command is populated upfront
	case "response.shell_call_command.added":
		return len(ojson.GetBytesNoCopy(payload, "command").String()) > 0

	// Single-chunk / non-stream completed items
	case "response.reasoning_summary_text.done",
		"response.reasoning_text.done",
		"response.output_text.done":
		return len(ojson.GetBytesNoCopy(payload, "text").String()) > 0
	case "response.refusal.done":
		return len(ojson.GetBytesNoCopy(payload, "refusal").String()) > 0
	case "response.function_call_arguments.done",
		"response.mcp_call_arguments.done":
		return len(ojson.GetBytesNoCopy(payload, "arguments").String()) > 0
	case "response.custom_tool_call_input.done":
		return len(ojson.GetBytesNoCopy(payload, "input").String()) > 0
	case "response.code_interpreter_call_code.done":
		return len(ojson.GetBytesNoCopy(payload, "code").String()) > 0
	case "response.shell_call_command.done":
		return len(ojson.GetBytesNoCopy(payload, "command").String()) > 0
	case "response.reasoning_summary_part.done":
		return len(ojson.GetBytesNoCopy(payload, "part.text").String()) > 0
	case "response.content_part.done":
		return len(ojson.GetBytesNoCopy(payload, "part.text").String()) > 0 || len(ojson.GetBytesNoCopy(payload, "part.refusal").String()) > 0
	case "response.output_item.done":
		itemType := ojson.GetBytesNoCopy(payload, "item.type").String()
		switch itemType {
		case "function_call":
			return len(ojson.GetBytesNoCopy(payload, "item.arguments").String()) > 0
		case "custom_tool_call":
			return len(ojson.GetBytesNoCopy(payload, "item.input").String()) > 0
		case "reasoning":
			for _, field := range []string{"item.summary", "item.content"} {
				for _, part := range ojson.GetBytesNoCopy(payload, field).Array() {
					text := part.Get("text")
					if text.Type == ojson.String && text.String() != "" {
						return true
					}
				}
			}
		case "message":
			for _, content := range ojson.GetBytesNoCopy(payload, "item.content").Array() {
				if len(content.Get("text").String()) > 0 || len(content.Get("refusal").String()) > 0 {
					return true
				}
			}
		}
		return false

	// Terminal events fallback to guarantee TTFT is never missed on truncated, failed, or fast turns
	case "response.completed",
		"response.done",
		"response.incomplete",
		"response.failed",
		"error":
		return true

	// All other container lifecycles (added, created, in_progress, search state machines, metadata)
	default:
		return false
	}
}

// ObserveResponsesTokenEvent inspects a Responses API frame payload and records TTFT if the frame
// represents the first meaningful token event. It records the first packet arrival time as a fallback
// and exits immediately with zero allocations once effective token TTFT is set.
func ObserveResponsesTokenEvent(reporter *UsageReporter, payload []byte) {
	if reporter != nil && len(payload) > 0 {
		reporter.ObserveResponseModel(payload)
	}
	if reporter == nil || len(payload) == 0 {
		return
	}
	if reporter.IsTTFTSet() {
		return
	}
	reporter.ObserveTokenEvent(IsResponsesTokenEvent(payload))
}

// IsChatTokenEvent reports whether the given OpenAI-compatible Chat Completions SSE or JSON chunk
// carries substantive output token content (such as text delta, reasoning content, or tool call arguments).
// It filters out container metadata, role announcements, and empty delta frames.
func IsChatTokenEvent(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}

	// Handle SSE stream terminal marker
	if bytes.Equal(payload, []byte("data: [DONE]")) || bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}

	// Handle SSE line format (e.g., "data: {...}")
	if bytes.HasPrefix(payload, []byte("data:")) {
		prefixLen := len("data:")
		if bytes.HasPrefix(payload, []byte("data: ")) {
			prefixLen = len("data: ")
		}
		payload = bytes.TrimSpace(payload[prefixLen:])
		if len(payload) == 0 {
			return false
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			return true
		}
	}

	// Terminal error envelope fallback
	if ojson.GetBytesNoCopy(payload, "error.message").Exists() || ojson.GetBytesNoCopy(payload, "error").Exists() {
		return true
	}

	choices := ojson.GetBytesNoCopy(payload, "choices").Array()
	if len(choices) == 0 {
		return false
	}

	for _, choice := range choices {
		// Streaming delta checks
		delta := choice.Get("delta")
		if delta.Exists() {
			if len(delta.Get("content").String()) > 0 {
				return true
			}
			if len(delta.Get("reasoning_content").String()) > 0 {
				return true
			}
			if len(delta.Get("reasoning").String()) > 0 {
				return true
			}
			if len(delta.Get("refusal").String()) > 0 {
				return true
			}
			for _, tc := range delta.Get("tool_calls").Array() {
				if len(tc.Get("function.arguments").String()) > 0 || len(tc.Get("function.name").String()) > 0 {
					return true
				}
				if len(tc.Get("custom.input").String()) > 0 {
					return true
				}
			}
		}

		// Non-streaming / completed message fallback
		message := choice.Get("message")
		if message.Exists() {
			if len(message.Get("content").String()) > 0 {
				return true
			}
			if len(message.Get("reasoning_content").String()) > 0 {
				return true
			}
			if len(message.Get("refusal").String()) > 0 {
				return true
			}
			for _, tc := range message.Get("tool_calls").Array() {
				if len(tc.Get("function.arguments").String()) > 0 || len(tc.Get("function.name").String()) > 0 {
					return true
				}
			}
		}

		// Finish reason terminal fallback
		if len(choice.Get("finish_reason").String()) > 0 {
			return true
		}
	}

	return false
}

// ObserveChatTokenEvent inspects an OpenAI Chat Completions chunk and records TTFT if the frame
// represents the first meaningful token event. It records first-packet arrival time as fallback
// and returns immediately with zero allocations once effective token TTFT is set.
func ObserveChatTokenEvent(reporter *UsageReporter, payload []byte) {
	if reporter != nil && len(payload) > 0 {
		reporter.ObserveResponseModel(payload)
	}
	if reporter == nil || len(payload) == 0 {
		return
	}
	if reporter.IsTTFTSet() {
		return
	}
	reporter.ObserveTokenEvent(IsChatTokenEvent(payload))
}

func IsClaudeTokenEvent(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}

	// Strip "event: ..." line if present in multi-line SSE buffer
	if bytes.HasPrefix(payload, []byte("event:")) {
		if idx := bytes.IndexByte(payload, '\n'); idx != -1 {
			payload = bytes.TrimSpace(payload[idx+1:])
		}
	}

	// Handle SSE data line prefix (e.g., "data: {...}")
	if bytes.HasPrefix(payload, []byte("data:")) {
		prefixLen := len("data:")
		if bytes.HasPrefix(payload, []byte("data: ")) {
			prefixLen = len("data: ")
		}
		payload = bytes.TrimSpace(payload[prefixLen:])
		if len(payload) == 0 {
			return false
		}
	}

	eventType := ojson.GetBytesNoCopy(payload, "type").String()
	switch eventType {
	// Substantive streaming block deltas
	case "content_block_delta":
		delta := ojson.GetBytesNoCopy(payload, "delta")
		if len(delta.Get("text").String()) > 0 {
			return true
		}
		if len(delta.Get("thinking").String()) > 0 {
			return true
		}
		if len(delta.Get("partial_json").String()) > 0 {
			return true
		}
		if len(delta.Get("signature").String()) > 0 {
			return true
		}
		return false

	// Initial block start when populated upfront
	case "content_block_start":
		cb := ojson.GetBytesNoCopy(payload, "content_block")
		if len(cb.Get("text").String()) > 0 || len(cb.Get("thinking").String()) > 0 {
			return true
		}
		return false

	// Terminal completion events fallback
	case "message_delta":
		return len(ojson.GetBytesNoCopy(payload, "delta.stop_reason").String()) > 0
	case "message_stop", "error":
		return true

	// Handshake metadata and container boundaries
	case "message_start", "ping", "content_block_stop":
		return false

	default:
		// Non-streaming / complete message payload fallback
		if ojson.GetBytesNoCopy(payload, "content").IsArray() {
			for _, block := range ojson.GetBytesNoCopy(payload, "content").Array() {
				if len(block.Get("text").String()) > 0 || len(block.Get("thinking").String()) > 0 {
					return true
				}
				if block.Get("type").String() == "tool_use" && len(block.Get("name").String()) > 0 {
					return true
				}
			}
		}
		return false
	}
}

func ObserveClaudeTokenEvent(reporter *UsageReporter, payload []byte) {
	if reporter != nil && len(payload) > 0 {
		reporter.ObserveResponseModel(payload)
	}
	if reporter == nil || len(payload) == 0 {
		return
	}
	if reporter.IsTTFTSet() {
		return
	}
	reporter.ObserveTokenEvent(IsClaudeTokenEvent(payload))
}

func IsGeminiTokenEvent(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}

	// Strip "data:" SSE prefix if present
	if bytes.HasPrefix(payload, []byte("data:")) {
		prefixLen := len("data:")
		if bytes.HasPrefix(payload, []byte("data: ")) {
			prefixLen = len("data: ")
		}
		payload = bytes.TrimSpace(payload[prefixLen:])
		if len(payload) == 0 {
			return false
		}
	}

	// Terminal error fallback
	if ojson.GetBytesNoCopy(payload, "error.message").Exists() || ojson.GetBytesNoCopy(payload, "error").Exists() || ojson.GetBytesNoCopy(payload, "response.error").Exists() {
		return true
	}

	candidatesNode := ojson.GetBytesNoCopy(payload, "candidates")
	if !candidatesNode.Exists() {
		candidatesNode = ojson.GetBytesNoCopy(payload, "response.candidates")
	}
	candidates := candidatesNode.Array()
	if len(candidates) == 0 {
		return false
	}

	for _, candidate := range candidates {
		// Inspect content parts
		parts := candidate.Get("content.parts").Array()
		for _, part := range parts {
			if len(part.Get("text").String()) > 0 {
				return true
			}
			if len(part.Get("thoughtText").String()) > 0 {
				return true
			}
			if thought := part.Get("thought"); thought.Type == ojson.String && len(thought.String()) > 0 {
				return true
			}
			if len(part.Get("functionCall.name").String()) > 0 {
				return true
			}
			if len(part.Get("inlineData.data").String()) > 0 {
				return true
			}
		}

		// Terminal finishReason fallback (e.g., STOP, MAX_TOKENS, SAFETY)
		if len(candidate.Get("finishReason").String()) > 0 {
			return true
		}
	}

	return false
}

func ObserveGeminiTokenEvent(reporter *UsageReporter, payload []byte) {
	if reporter != nil && len(payload) > 0 {
		reporter.ObserveResponseModel(payload)
	}
	if reporter == nil || len(payload) == 0 {
		return
	}
	if reporter.IsTTFTSet() {
		return
	}
	reporter.ObserveTokenEvent(IsGeminiTokenEvent(payload))
}
