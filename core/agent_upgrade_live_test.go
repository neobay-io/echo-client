//go:build live

package core

import (
	"context"
	"testing"
)

// Read-only: shows how each target resolves on this machine. Runs version
// probes and install-route probes only, never an update command.
func TestLiveUpgradeStatusesOnThisMachine(t *testing.T) {
	cfg := DefaultAgentUpgradeConfig()
	cfg.Enabled = true
	mgr := NewAgentUpgradeManager(cfg)
	mgr.SetCLIInspector(InspectAgentCLI)
	mgr.SetLockDir("")
	for _, st := range mgr.Statuses(context.Background()) {
		t.Logf("%s: route=%q binary=%q -> %q version=%q verr=%q\n    update=%q\n    warning=%q blocked=%q",
			st.Name, st.InstallRoute, st.BinaryPath, st.ResolvedPath, st.Version, st.VersionErr, st.UpdateCommand, st.Warning, st.BlockedReason)
	}
}
