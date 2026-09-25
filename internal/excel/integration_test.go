package excel

import (
	"encoding/json"
	"testing"
)

// TestPrepareBodyProducesBackendWireShape guards the contract the Codex executor
// relies on when it serves an Excel-backed model: the upstream requires these
// fields, and a caller's tool declarations must not be forwarded.
func TestPrepareBodyProducesBackendWireShape(t *testing.T) {
	body := map[string]any{
		"model":  "gpt-5.6-sol-excel",
		"stream": true,
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
			},
		},
	}

	upstream, errPrepare := PrepareBody(body, &Catalog{})
	if errPrepare != nil {
		t.Fatalf("prepare failed: %v", errPrepare)
	}
	encoded, errMarshal := json.Marshal(upstream)
	if errMarshal != nil {
		t.Fatalf("marshal failed: %v", errMarshal)
	}

	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &decoded); errUnmarshal != nil {
		t.Fatalf("round trip failed: %v", errUnmarshal)
	}
	if decoded["model"] != "gpt-5.6-sol" {
		t.Errorf("alias was not resolved to the upstream slug: %v", decoded["model"])
	}
	if decoded["model_selection"] != "explicit" {
		t.Error("model_selection must be explicit so the backend applies no routing of its own")
	}
	// The backend rejects a declared tool list outright.
	for _, forbidden := range []string{"tools", "tool_choice", "include", "max_output_tokens", "previous_response_id"} {
		if _, exists := decoded[forbidden]; exists {
			t.Errorf("field %q must not be forwarded to this backend", forbidden)
		}
	}
	metadata, _ := decoded["metadata"].(map[string]any)
	if metadata["task_id"] == nil || metadata["turn_id"] == nil {
		t.Errorf("backend requires task_id and turn_id: %v", metadata)
	}
}

// TestEveryAliasResolvesToABackendSlug guards the alias table the Codex executor
// matches on before branching to this protocol.
func TestEveryAliasResolvesToABackendSlug(t *testing.T) {
	for _, alias := range Aliases() {
		slug, ok := SlugFor(alias)
		if !ok || slug == "" {
			t.Errorf("alias %q does not resolve to a backend slug", alias)
		}
		if !IsAlias(alias) {
			t.Errorf("IsAlias(%q) = false, so the Codex executor would not route it here", alias)
		}
	}
}

// TestBareSlugIsNotAnExcelAlias guards a real rerouting hazard: the Codex
// executor branches to this protocol on IsAlias. If a bare upstream slug such as
// "gpt-5.6-luna" matched, an existing Codex model would be silently sent to the
// Excel backend instead of the Codex endpoint.
func TestBareSlugIsNotAnExcelAlias(t *testing.T) {
	for _, model := range DefaultModels {
		if IsAlias(model.Slug) {
			t.Errorf("bare slug %q must not be treated as an Excel alias", model.Slug)
		}
		if !IsAlias(model.Alias) {
			t.Errorf("alias %q must be recognised", model.Alias)
		}
	}
	// A normal Codex model must never match.
	for _, model := range []string{"gpt-5.6-luna", "gpt-5.5-codex", "gpt-6-astra", ""} {
		if IsAlias(model) {
			t.Errorf("IsAlias(%q) = true, which would reroute a normal Codex model", model)
		}
	}
}

// TestSlugForStillResolvesBothForms records that SlugFor remains the permissive
// lookup used when the model is already known to be Excel-backed.
func TestSlugForStillResolvesBothForms(t *testing.T) {
	if slug, ok := SlugFor("gpt-5.6-sol-excel"); !ok || slug != "gpt-5.6-sol" {
		t.Errorf("SlugFor(alias) = %q, %v", slug, ok)
	}
	if slug, ok := SlugFor("gpt-5.6-sol"); !ok || slug != "gpt-5.6-sol" {
		t.Errorf("SlugFor(slug) = %q, %v", slug, ok)
	}
}

// TestUnknownContextWindowFallsBackToDefault pins the advertised window. The
// backend publishes no metadata, so an unknown model must not advertise a
// smaller window than it actually serves -- that would truncate conversations
// the backend would have accepted.
func TestUnknownContextWindowFallsBackToDefault(t *testing.T) {
	if DefaultContextLength != 350_000 {
		t.Fatalf("DefaultContextLength = %d, want 350000", DefaultContextLength)
	}
	unknown := Model{Slug: "gpt-unknown", Alias: "gpt-unknown-excel"}
	if got := unknown.ContextLengthValue(); got != DefaultContextLength {
		t.Fatalf("unknown model advertised %d, want the default", got)
	}
	known := Model{ContextLength: 200_000}
	if got := known.ContextLengthValue(); got != 200_000 {
		t.Fatalf("explicit window ignored: %d", got)
	}
	// Every shipped alias must advertise a window.
	for _, model := range DefaultModels {
		if model.ContextLengthValue() < DefaultContextLength {
			t.Errorf("%s advertises %d, below the default", model.Alias, model.ContextLengthValue())
		}
	}
}

// TestAllModelsOfferEveryAcceptedEffort keeps the advertised efforts in step
// with what the backend accepts; the backend answers HTTP 422 to an unsupported
// value, so listing one that is rejected would produce a failing request.
func TestAllModelsOfferEveryAcceptedEffort(t *testing.T) {
	for _, model := range DefaultModels {
		if len(model.Efforts) != len(DefaultEfforts) {
			t.Errorf("%s offers %v, want all of %v", model.Alias, model.Efforts, DefaultEfforts)
		}
		for _, effort := range DefaultEfforts {
			normalized := NormalizeEffort(model.Alias, effort)
			if normalized != effort {
				t.Errorf("%s: effort %q normalized to %q, so it is not actually supported", model.Alias, effort, normalized)
			}
		}
		// A value the backend rejects must not be advertised either.
		for _, rejected := range []string{"max", "minimal", "ultra"} {
			for _, offered := range model.Efforts {
				if offered == rejected {
					t.Errorf("%s advertises %q, which the backend rejects", model.Alias, rejected)
				}
			}
		}
	}
}
