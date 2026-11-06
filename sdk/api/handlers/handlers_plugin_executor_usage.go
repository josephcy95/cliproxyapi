package handlers

import (
	"bytes"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func parsePluginExecutorResponseUsage(protocol string, payload []byte) usage.Detail {
	return helps.ParsePluginExecutorResponseUsage(protocol, payload)
}

func observePluginExecutorStreamUsage(protocol string, payload []byte, buffer *helps.StreamUsageBuffer) {
	helps.ObservePluginExecutorStreamUsage(protocol, payload, buffer)
}

func parseClaudePayloadUsage(payload []byte) usage.Detail {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		usageNode = gjson.GetBytes(payload, "message.usage")
	}
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return helps.ParseClaudeUsage([]byte(`{"usage":` + usageNode.Raw + `}`))
}

func parseClaudeStreamLine(line []byte) (usage.Detail, bool) {
	payload := extractStreamJSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		usageNode = gjson.GetBytes(payload, "message.usage")
	}
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail := helps.ParseClaudeUsage([]byte(`{"usage":` + usageNode.Raw + `}`))
	return detail, true
}

func observeMergedStreamUsage(buffer *helps.StreamUsageBuffer, update usage.Detail) {
	if buffer == nil {
		return
	}
	if existing, ok := buffer.Detail(); ok {
		merged := mergeStreamUsageDetail(existing, update)
		buffer.Observe(merged, true)
		return
	}
	buffer.Observe(update, true)
}

func mergeStreamUsageDetail(existing, update usage.Detail) usage.Detail {
	return helps.MergeStreamUsageDetail(existing, update)
}

func iterateStreamLines(payload []byte, fn func(line []byte)) {
	for _, line := range bytes.Split(payload, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		fn(trimmed)
	}
}

func extractStreamJSONPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	return trimmed
}

// parseResponsesPluginExecutorUsage reads both streaming-event usage (response.usage)
// and a completed Responses object (top-level usage). A service tier without
// response.usage must not hide the completed object's token counts.
func parseResponsesPluginExecutorUsage(payload []byte) usage.Detail {
	detail, ok := helps.ParseCodexUsage(payload)
	if ok && hasPluginTokenUsage(detail) {
		return detail
	}
	openAIDetail := helps.ParseOpenAIUsage(payload)
	if hasPluginTokenUsage(openAIDetail) {
		if openAIDetail.ResponseServiceTier == "" {
			openAIDetail.ResponseServiceTier = detail.ResponseServiceTier
		}
		return openAIDetail
	}
	if ok {
		return detail
	}
	return openAIDetail
}

func hasPluginTokenUsage(d usage.Detail) bool {
	return d.InputTokens != 0 || d.OutputTokens != 0 || d.TotalTokens != 0 || d.CachedTokens != 0 || d.ReasoningTokens != 0 || d.CacheReadTokens != 0 || d.CacheCreationTokens != 0
}

func observePluginExecutorStreamTTFT(protocol string, reporter *helps.UsageReporter, payload []byte) {
	if reporter == nil || len(payload) == 0 {
		return
	}
	reporter.RecordFirstPacket()
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "claude":
		helps.ObserveClaudeTokenEvent(reporter, payload)
	case "gemini", "antigravity", "interactions", "interactions-response":
		helps.ObserveGeminiTokenEvent(reporter, payload)
	case "codex", "openai-response":
		helps.ObserveResponsesTokenEvent(reporter, payload)
	default:
		helps.ObserveChatTokenEvent(reporter, payload)
	}
}
