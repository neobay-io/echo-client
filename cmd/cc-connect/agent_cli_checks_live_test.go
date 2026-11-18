//go:build live

package main

import (
	"context"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// The smoke checks upgrades rely on must pass on the CLIs installed here,
// or every upgrade would be rolled back. Run with: go test -tags live -run
// TestAgentCLIChecksPassOnInstalledCLIs ./cmd/cc-connect
func TestAgentCLIChecksPassOnInstalledCLIs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, agentType := range core.DoctorAgentTypes {
		path, err := core.ResolveAgentCLI(agentType)
		if err != nil {
			t.Logf("%s: not installed", agentType)
			continue
		}
		if problems := core.CheckAgentCLIOptions(ctx, agentType, path); len(problems) > 0 {
			t.Errorf("%s (%s): %v", agentType, path, problems)
		} else {
			t.Logf("%s (%s): ok", agentType, path)
		}
	}
}
