package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

//go:embed default_config.yaml
var embeddedDefaultConfigYAML []byte

// applyBuiltinPreUnmarshalDefaults sets Config fields that must be non-zero before
// yaml.Unmarshal so omitted keys keep the intended default.
//
// Port is intentionally left at 0: cloud-deploy standby uses Port==0 as "no config yet".
func applyBuiltinPreUnmarshalDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.Host = ""
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.DisableCooling = false
	cfg.SaveCooldownStatus = false
	cfg.TransientErrorCooldownSeconds = 0
	cfg.DisableImageGeneration = DisableImageGenerationOff
	cfg.XAI = DefaultXAIConfig()
	cfg.Qoder = DefaultQoderConfig()
	cfg.Desensitization = DefaultDesensitizationConfig()
	cfg.Codex = NormalizeCodexConfig(cfg.Codex)
	cfg.WebsocketAuth = true
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.Discovery.Enabled = false
	cfg.Discovery.ServiceType = DefaultDiscoveryServiceType
	cfg.Discovery.Subtypes = []string{"_chat-completions", "_responses", "_messages", "_generate-content", "_interactions"}
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	cfg.RequestRetry = 3
	cfg.MaxRetryCredentials = 0
	cfg.MaxRetryInterval = 30
	cfg.AuthDir = DefaultAuthDir
	cfg.QuotaExceeded.SwitchProject = true
	cfg.QuotaExceeded.SwitchPreviewModel = true
	cfg.QuotaExceeded.AntigravityCredits = true
	cfg.Routing.Strategy = "round-robin"
	cfg.Routing.SessionAffinity = false
	cfg.Routing.SessionAffinityTTL = "1h"
	cfg.VideoResultAuthCacheTTL = "3h"
	cfg.CommercialMode = false
	cfg.ForceModelPrefix = false
	cfg.PassthroughHeaders = false
	cfg.DisableClaudeCloakMode = false
	cfg.NonStreamKeepAliveInterval = 0
}

// ensureMissingConfigKeysOnDisk adds missing default keys to an existing config file
// while preserving comments, ordering, and all existing values.
// Returns true when the file was modified.
func ensureMissingConfigKeysOnDisk(configFile string, data []byte) (bool, error) {
	if strings.TrimSpace(configFile) == "" || len(data) == 0 {
		return false, nil
	}

	var original yaml.Node
	if err := yaml.Unmarshal(data, &original); err != nil {
		return false, fmt.Errorf("parse existing config for backfill: %w", err)
	}
	if original.Kind != yaml.DocumentNode || len(original.Content) == 0 || original.Content[0] == nil {
		return false, fmt.Errorf("invalid existing config yaml structure")
	}
	if original.Content[0].Kind != yaml.MappingNode {
		return false, fmt.Errorf("expected root mapping node in config file")
	}

	var defaultsDoc yaml.Node
	if err := yaml.Unmarshal(embeddedDefaultConfigYAML, &defaultsDoc); err != nil {
		return false, fmt.Errorf("parse embedded default config: %w", err)
	}
	if defaultsDoc.Kind != yaml.DocumentNode || len(defaultsDoc.Content) == 0 || defaultsDoc.Content[0] == nil {
		return false, fmt.Errorf("invalid embedded default config structure")
	}
	if defaultsDoc.Content[0].Kind != yaml.MappingNode {
		return false, fmt.Errorf("expected root mapping node in embedded default config")
	}

	changed := backfillMissingMappingKeys(original.Content[0], defaultsDoc.Content[0], nil)
	if pruneObsoleteTopLevelConfigKeys(original.Content[0]) {
		changed = true
	}
	if !changed {
		return false, nil
	}

	f, err := os.Create(configFile)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(&original); err != nil {
		_ = enc.Close()
		return false, err
	}
	if err = enc.Close(); err != nil {
		return false, err
	}
	out := NormalizeCommentIndentation(buf.Bytes())
	if _, err = f.Write(out); err != nil {
		return false, err
	}
	log.Info("backfilled missing config keys from built-in defaults")
	return true, nil
}

// backfillMissingMappingKeys copies keys present in src but missing in dst.
// Existing dst keys/values are preserved; nested maps are filled recursively.
func backfillMissingMappingKeys(dst, src *yaml.Node, path []string) bool {
	if dst == nil || src == nil {
		return false
	}
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return false
	}

	changed := false
	for i := 0; i+1 < len(src.Content); i += 2 {
		sk := src.Content[i]
		sv := src.Content[i+1]
		if sk == nil || sv == nil {
			continue
		}
		key := sk.Value
		childPath := appendPath(path, key)
		if shouldSkipDefaultBackfillPath(childPath) {
			continue
		}

		idx := findMapKeyIndex(dst, key)
		if idx < 0 {
			dst.Content = append(dst.Content, deepCopyNode(sk), deepCopyNode(sv))
			changed = true
			continue
		}

		dv := dst.Content[idx+1]
		if dv == nil {
			dst.Content[idx+1] = deepCopyNode(sv)
			changed = true
			continue
		}

		// Recurse into mappings only. Never rewrite existing sequences/scalars.
		if dv.Kind == yaml.MappingNode && sv.Kind == yaml.MappingNode {
			if backfillMissingMappingKeys(dv, sv, childPath) {
				changed = true
			}
		}
	}
	return changed
}

// shouldSkipDefaultBackfillPath returns true for paths that must never be auto-injected
// as sample/demo content under an already-present parent.
func shouldSkipDefaultBackfillPath(path []string) bool {
	if len(path) == 0 {
		return false
	}
	// Never inject sample plugin config entries under plugins.configs.<id>.
	if len(path) >= 3 && path[0] == "plugins" && path[1] == "configs" {
		return true
	}
	return false
}

// obsoleteTopLevelConfigKeys are keys that used to exist in config.yaml but are no longer
// part of the product. On load/backfill we remove them so the on-disk file stays relevant.
var obsoleteTopLevelConfigKeys = map[string]struct{}{
	"save-cooldown-status": {},
}

// pruneObsoleteTopLevelConfigKeys removes deprecated root-level keys from a YAML mapping node.
// Returns true when the document was modified.
func pruneObsoleteTopLevelConfigKeys(root *yaml.Node) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for i := 0; i+1 < len(root.Content); {
		keyNode := root.Content[i]
		if keyNode == nil {
			i += 2
			continue
		}
		key := strings.TrimSpace(keyNode.Value)
		if _, obsolete := obsoleteTopLevelConfigKeys[key]; !obsolete {
			i += 2
			continue
		}
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
		changed = true
	}
	return changed
}
