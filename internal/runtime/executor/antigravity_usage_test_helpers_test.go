package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type antigravityUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *antigravityUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.Provider != antigravityAuthType || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type antigravityUsageNoop struct{}

func (antigravityUsageNoop) HandleUsage(context.Context, usage.Record) {}
