package registry

import (
	"sort"
	"strings"
)

var antigravityEffortOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// AntigravityEffortFamily recognizes wire variants, not distinct routes such as tiered.
func AntigravityEffortFamily(id string) (string, string) {
	for _, effort := range antigravityEffortOrder {
		if base, ok := strings.CutSuffix(id, "-"+effort); ok {
			return base, effort
		}
	}
	return id, ""
}

// ExpandAntigravityEffortModels adds entitled variants of known catalog families.
// Token limits and modalities remain catalog-owned; discovery controls entitlement.
func ExpandAntigravityEffortModels(filtered, catalog []*ModelInfo, entitled map[string]struct{}) []*ModelInfo {
	templates := map[string]*ModelInfo{}
	seen := map[string]bool{}
	for _, m := range filtered {
		if m != nil {
			seen[m.ID] = true
		}
	}
	for _, m := range catalog {
		if m == nil || m.Thinking == nil {
			continue
		}
		base, effort := AntigravityEffortFamily(m.ID)
		if effort != "" {
			templates[base] = m
		}
	}
	ids := make([]string, 0, len(entitled))
	for id := range entitled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		base, effort := AntigravityEffortFamily(id)
		template := templates[base]
		if effort == "" || template == nil {
			continue
		}
		m := cloneModelInfo(template)
		m.ID = id
		m.Name = id
		if i := strings.LastIndex(m.DisplayName, " ("); i >= 0 {
			m.DisplayName = m.DisplayName[:i]
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// GroupAntigravityEffortModels publishes one family and preserves hidden legacy routes.
// Only variants present after account filtering contribute supported efforts.
func GroupAntigravityEffortModels(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models))
	families := map[string]*ModelInfo{}
	for _, source := range models {
		if source == nil {
			continue
		}
		m := cloneModelInfo(source)
		base, effort := AntigravityEffortFamily(m.ID)
		if effort == "" || m.Thinking == nil {
			result = append(result, m)
			continue
		}
		family := families[base]
		if family == nil {
			family = cloneModelInfo(m)
			family.HiddenFromList = false
			family.ID = base
			family.Name = base
			// Display labels may already omit the effort in the catalog.
			if i := strings.LastIndex(family.DisplayName, " ("); i >= 0 {
				family.DisplayName = family.DisplayName[:i]
			}
			family.Description = family.DisplayName
			family.Thinking.Levels = nil
			family.AntigravityEffortModels = map[string]string{}
			family.AntigravityDefaultEffort = effort
			families[base] = family
			result = append(result, family)
		}
		family.AntigravityEffortModels[effort] = m.ID
		m.HiddenFromList = true
		result = append(result, m)
	}
	for _, family := range families {
		for _, effort := range antigravityEffortOrder {
			if _, ok := family.AntigravityEffortModels[effort]; ok {
				family.Thinking.Levels = append(family.Thinking.Levels, effort)
			}
		}
		// Preserve the catalog's historic high/xhigh default where available.
		for _, effort := range []string{"xhigh", "high", "medium", "low", "minimal", "max"} {
			if _, ok := family.AntigravityEffortModels[effort]; ok {
				family.AntigravityDefaultEffort = effort
				break
			}
		}
	}
	return result
}
