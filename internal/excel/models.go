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
	// BaseModel is the public Codex model this alias shares its model with. It
	// supplies the advertised context window, token limits and capabilities, so
	// the Excel route looks like the ordinary model instead of an anonymous
	// catalog entry. Empty means the alias has no public equivalent.
	BaseModel string
	// ContextLength overrides the advertised context window. Zero means unknown,
	// in which case DefaultContextLength is advertised.
	ContextLength int
	// Efforts lists the reasoning efforts the backend accepts for this model.
	Efforts []string
}

// DefaultModels mirrors the alias set published by Nonary/ghcp_proxy, extended
// with the GPT-6 aliases the backend serves.
//
// Descriptions are intentionally absent: the Codex client builds its own
// prompt text and the upstream slug is what selects the actual backend model.
//
// Each entry names its BaseModel so the alias inherits the public model's
// advertised context window, token limits and capabilities. The Excel route is
// the same model on a different backend endpoint, not a separate model, so it
// must not look narrower than the ordinary route; only the reasoning efforts
// differ (see the package comment in protocol.go).
var DefaultModels = []Model{
	{
		Slug:        "gpt-6-astra",
		Alias:       "gpt-6-astra-excel",
		DisplayName: "6-Astra Excel",
		BaseModel:   "gpt-6-astra",
		Efforts:     []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:        "gpt-6-sol",
		Alias:       "gpt-6-sol-excel",
		DisplayName: "6-Sol Excel",
		BaseModel:   "gpt-6-sol",
		Efforts:     []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:        "gpt-6-luna",
		Alias:       "gpt-6-luna-excel",
		DisplayName: "6-Luna Excel",
		BaseModel:   "gpt-6-luna",
		Efforts:     []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:        "gpt-5.6-luna",
		Alias:       "gpt-5.6-luna-excel",
		DisplayName: "5.6-Luna Excel",
		BaseModel:   "gpt-5.6-luna",
		Efforts:     []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:        "gpt-5.6-terra",
		Alias:       "gpt-5.6-terra-excel",
		DisplayName: "5.6-Terra Excel",
		BaseModel:   "gpt-5.6-terra",
		Efforts:     []string{"low", "medium", "high", "xhigh"},
	},
	{
		Slug:        "gpt-5.6-sol",
		Alias:       "gpt-5.6-sol-excel",
		DisplayName: "5.6-Sol Excel",
		BaseModel:   "gpt-5.6-sol",
		Efforts:     []string{"low", "medium", "high", "xhigh"},
	},
}

// ContextLength returns the advertised window for this model, falling back to
// DefaultContextLength when the real value is unknown.
func (m Model) ContextLengthValue() int {
	if m.ContextLength > 0 {
		return m.ContextLength
	}
	return DefaultContextLength
}

// DefaultEfforts is used when a model declares no effort list.
var DefaultEfforts = []string{"low", "medium", "high", "xhigh"}

// DefaultContextLength is advertised for an Excel-backed model whose real window
// is not known.
//
// The backend publishes no model metadata, so the exact per-model window is a
// guess. A larger value is the safer default here: advertising less than the
// backend allows would truncate working conversations, and prompt compaction is
// driven by the backend's own thresholds regardless.
const DefaultContextLength = 350_000

// EffortAliases maps tolerated spellings onto the wire values the backend wants.
var EffortAliases = map[string]string{
	"x-high":     "xhigh",
	"extra-high": "xhigh",
	"extra_high": "xhigh",
}

// BaseModelFor maps an Excel alias onto the public Codex model that shares its
// underlying model.
//
// The Excel backend is an alternative route to the same model, so the alias must
// advertise the same context window and token limits as the public route; only
// the reasoning efforts differ, because this backend rejects "max" and "ultra".
// The mapping is declared on the alias entry (Model.BaseModel) rather than
// guessed from the name.
//
// Provider prefixes and reasoning suffixes are tolerated, matching IsRoute. A
// bare upstream slug is not an alias and never resolves here. A false result
// means the alias has no public Codex equivalent, and it falls back to the
// generic Excel defaults.
func BaseModelFor(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "("); i >= 0 && strings.HasSuffix(model, ")") {
		model = model[:i]
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if !IsAlias(model) {
		return "", false
	}
	m, ok := LookupAlias(model)
	if !ok {
		return "", false
	}
	base := strings.TrimSpace(m.BaseModel)
	if base == "" {
		return "", false
	}
	return base, true
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

// IsAlias reports whether the model name is one of the Excel aliases.
//
// Only the "-excel" aliases are served by this protocol. A bare upstream slug
// such as "gpt-5.6-luna" must keep its normal Codex path: treating it as an
// Excel model would silently reroute an existing model.
func IsAlias(model string) bool {
	needle := strings.ToLower(strings.TrimSpace(model))
	if needle == "" {
		return false
	}
	for _, m := range DefaultModels {
		if strings.ToLower(m.Alias) == needle {
			return true
		}
	}
	return false
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

// IsRoute recognizes reserved Excel aliases through provider prefixes and reasoning
// suffixes. Unlike LookupAlias, it never treats a bare upstream slug as Excel.
func IsRoute(model string) bool {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "("); i >= 0 && strings.HasSuffix(model, ")") {
		model = model[:i]
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return IsAlias(model)
}
