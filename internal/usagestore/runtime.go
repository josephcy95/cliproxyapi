package usagestore

import (
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var (
	runtimeMu     sync.Mutex
	runtimeStore  *Store
	runtimePlugin *Plugin
)

// Configure opens/replaces the global durable usage store and registers the plugin.
// path may be empty to use the default. enabled controls whether events are written.
func Configure(path string, retentionDays int, enabled bool) (*Store, error) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()

	if runtimePlugin == nil {
		runtimePlugin = NewPlugin(nil)
		coreusage.RegisterNamedPlugin("usagestore", runtimePlugin)
	}
	runtimePlugin.SetEnabled(enabled)

	// Reuse existing store when path matches.
	if runtimeStore != nil && runtimeStore.Path() == ResolveStorePath(path, "") {
		runtimeStore.SetRetentionDays(retentionDays)
		runtimePlugin.SetStore(runtimeStore)
		return runtimeStore, nil
	}

	// Close previous store if path changes.
	if runtimeStore != nil {
		_ = runtimeStore.Close()
		runtimeStore = nil
		runtimePlugin.SetStore(nil)
	}

	store, err := Open(Options{Path: ResolveStorePath(path, ""), RetentionDays: retentionDays})
	if err != nil {
		log.Errorf("usagestore: failed to open %s: %v", path, err)
		return nil, err
	}
	runtimeStore = store
	runtimePlugin.SetStore(store)
	log.Infof("usagestore: opened %s (enabled=%v retention_days=%d)", store.Path(), enabled, retentionDays)
	return store, nil
}

// Current returns the active store, if any.
func Current() *Store {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	return runtimeStore
}

// SetEnabled toggles persistence on the runtime plugin.
func SetEnabled(enabled bool) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if runtimePlugin != nil {
		runtimePlugin.SetEnabled(enabled)
	}
}

// CloseRuntime closes the global store.
func CloseRuntime() {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if runtimeStore != nil {
		_ = runtimeStore.Close()
		runtimeStore = nil
	}
	if runtimePlugin != nil {
		runtimePlugin.SetStore(nil)
	}
}
