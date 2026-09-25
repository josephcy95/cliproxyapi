package excel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// identityNamespace mirrors the derivation namespace used by ghcp_proxy so the
// same conversation maps to the same backend task across implementations.
const identityNamespace = "cliproxyapi/excel/"

// DeriveIdentities produces the task_id / turn_id / agent_iteration triple the
// backend requires.
//
// Every value is derived, never random: a retried request must serialise to the
// same bytes so the backend treats it as the same turn rather than new work,
// and the upstream prompt cache can reuse the conversation prefix.
//
// The turn spans all tool iterations of one user message. Advancing the turn on
// every tool result makes the backend discard its plan state and start
// planning again, so only the iteration counter moves within a turn.
func DeriveIdentities(body map[string]any, cleanInput []any) (taskID, turnID, iteration string) {
	conversation := promptCacheKey(body)
	if conversation == "" {
		conversation = fingerprint(firstItem(cleanInput))
	}

	lastUserIndex := -1
	for index, raw := range cleanInput {
		item, okItem := raw.(map[string]any)
		if !okItem {
			continue
		}
		if strings.EqualFold(stringField(item, "role"), "user") {
			lastUserIndex = index
		}
	}

	turnAnchor := firstItem(cleanInput)
	if lastUserIndex >= 0 && lastUserIndex < len(cleanInput) {
		if rendered, okRender := canonicalJSON(cleanInput[lastUserIndex]); okRender {
			turnAnchor = rendered
		}
	}
	turnFingerprint := fingerprint(turnAnchor)

	iterations := 0
	if lastUserIndex >= 0 {
		for _, raw := range cleanInput[lastUserIndex+1:] {
			item, okItem := raw.(map[string]any)
			if !okItem {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(stringField(item, "type"))) {
			case "function_call_output", "custom_tool_call_output":
				iterations++
			}
		}
	}

	taskID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(identityNamespace+conversation)).String()
	turnID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(identityNamespace+conversation+"/turn/"+turnFingerprint)).String()
	iteration = strconv.Itoa(iterations + 1)
	return taskID, turnID, iteration
}

func firstItem(items []any) string {
	for _, raw := range items {
		if rendered, okRender := canonicalJSON(raw); okRender {
			return rendered
		}
	}
	return "empty"
}

// fingerprint hashes a canonical rendering of an input item.
func fingerprint(value any) string {
	rendered, okRender := canonicalJSON(value)
	if !okRender {
		return "unrenderable"
	}
	sum := sha256.Sum256([]byte(rendered))
	return hex.EncodeToString(sum[:])
}

// canonicalJSON renders a value deterministically. map keys are sorted by
// encoding/json, which is what makes the derived identities stable.
func canonicalJSON(value any) (string, bool) {
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return "", false
	}
	return string(encoded), true
}
