// Package excel implements the ChatGPT Excel (Basispoints) reverse-proxy
// protocol used to serve Codex Responses clients from the OpenAI backend that
// the official Excel add-in talks to.
//
// Why this exists: the Excel backend accepts the same ChatGPT/Codex OAuth token
// family as the public Codex endpoint, but it does not apply the public
// endpoint's automatic model routing. Requests must therefore be shaped like
// the add-in's own wire format, including an explicit "model_selection".
//
// The backend injects a large spreadsheet-agent system prompt plus ~20 native
// workbook tools that cannot be removed from the request (a non-empty "tools"
// field is rejected with HTTP 422). Client tools are therefore relayed through
// a text-marker protocol embedded in the assistant text, which the proxy
// extracts before the response reaches the client. See relay.go.
package excel

import "strings"

// Model describes one Excel-backed model alias.
type Model struct {
	// Slug is the upstream model identifier, e.g. "gpt-5.6-sol".
	Slug string
	// Alias is the client-facing model identifier, e.g. "gpt-5.6-sol-excel".
	Alias string
	// DisplayName is shown in the Codex client model picker.
	DisplayName string
	// ContextLength is the advertised context window.
	ContextLength int
	// Efforts lists the reasoning efforts the backend accepts for this model.
	Efforts []string
}

// DefaultModels mirrors the alias set published by Nonary/ghcp_proxy.
//
// Descriptions are intentionally absent: the Codex client builds its own
// prompt text and the upstream slug is what selects the actual backend model.
var DefaultModels = []Model{
	{
		Slug:          "gpt-6-astra",
		Alias:         "gpt-6-astra-excel",
		DisplayName:   "6-Astra Excel",
		ContextLength: 272_000,
		Efforts:       []string{"medium", "high", "xhigh"},
	},
	{
		Slug:          "gpt-5.6-luna",
		Alias:         "gpt-5.6-luna-excel",
		DisplayName:   "5.6-Luna Excel",
		ContextLength: 200_000,
		Efforts:       []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:          "gpt-5.6-terra",
		Alias:         "gpt-5.6-terra-excel",
		DisplayName:   "5.6-Terra Excel",
		ContextLength: 272_000,
		Efforts:       []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:          "gpt-5.6-sol",
		Alias:         "gpt-5.6-sol-excel",
		DisplayName:   "5.6-Sol Excel",
		ContextLength: 272_000,
		Efforts:       []string{"low", "medium", "high", "xhigh"},
	},
}

// DefaultEfforts is used when a model declares no effort list.
var DefaultEfforts = []string{"low", "medium", "high", "xhigh"}

// EffortAliases maps tolerated spellings onto the wire values the backend wants.
var EffortAliases = map[string]string{
	"x-high":     "xhigh",
	"extra-high": "xhigh",
	"extra_high": "xhigh",
}

// LookupAlias resolves a client-facing alias to its upstream model.
func LookupAlias(model string) (Model, bool) {
	needle := strings.ToLower(strings.TrimSpace(model))
	if needle == "" {
		return Model{}, false
	}
	for _, m := range DefaultModels {
		if strings.ToLower(m.Alias) == needle || strings.ToLower(m.Slug) == needle {
			return m, true
		}
	}
	return Model{}, false
}

// IsAlias reports whether the model name is served by this package.
func IsAlias(model string) bool {
	_, ok := LookupAlias(model)
	return ok
}

// SlugFor resolves an alias (or a bare slug) to the upstream model identifier.
func SlugFor(model string) (string, bool) {
	m, ok := LookupAlias(model)
	if !ok {
		return "", false
	}
	return m.Slug, true
}

// Aliases returns the client-facing aliases in declaration order.
func Aliases() []string {
	out := make([]string, 0, len(DefaultModels))
	for _, m := range DefaultModels {
		out = append(out, m.Alias)
	}
	return out
}

// NormalizeEffort lower-cases and de-aliases a reasoning effort, returning
// "medium" when the value is missing or not accepted for the model.
//
// The backend answers HTTP 422 to unknown effort values, so an unrecognized
// client value must degrade to a known-good default rather than pass through.
func NormalizeEffort(model, effort string) string {
	value := strings.ToLower(strings.TrimSpace(effort))
	if mapped, ok := EffortAliases[value]; ok {
		value = mapped
	}
	allowed := DefaultEfforts
	if m, ok := LookupAlias(model); ok && len(m.Efforts) > 0 {
		allowed = m.Efforts
	}
	for _, candidate := range allowed {
		if candidate == value {
			return value
		}
	}
	for _, candidate := range allowed {
		if candidate == "medium" {
			return "medium"
		}
	}
	if len(allowed) > 0 {
		return allowed[0]
	}
	return "medium"
}
