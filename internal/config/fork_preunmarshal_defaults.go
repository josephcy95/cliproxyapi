package config

// DefaultForkAuthDir keeps auth files under the single data root when auth-dir is
// omitted. The relative path resolves to CLIPROXY_DATA_DIR/auths via util.ResolveAuthDir.
const DefaultForkAuthDir = "auths"

// applyBuiltinPreUnmarshalDefaults sets Config fields that must be non-zero before
// yaml.Unmarshal so omitted keys keep the intended default.
//
// Global retry, routing, and quota values intentionally keep their Go zero value:
// presence in the document, not an implicit default, decides them. This matches the
// v8 configuration API contract (GET and failed writes must not mutate the stored
// file, and DELETE restores the zero default). The shipped config.example.yaml sets
// the operator-facing retry/routing/quota defaults explicitly, so fresh deployments
// are unchanged.
//
// Fork-owned product defaults that upstream does not model (xAI/Qoder/Codex failure
// policy, desensitization, the fork panel repository, single data root) still live
// here so an omitted block keeps fork behavior.
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
	// Fork single data root: a relative auth directory resolves under CLIPROXY_DATA_DIR.
	cfg.AuthDir = DefaultForkAuthDir
	cfg.CommercialMode = false
	cfg.ForceModelPrefix = false
	cfg.PassthroughHeaders = false
	cfg.DisableClaudeCloakMode = false
	cfg.NonStreamKeepAliveInterval = 0
}
