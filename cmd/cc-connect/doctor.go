package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// runDoctor implements `echo-client doctor [agent-type...]`: show, for each
// agent CLI, the version and the path this process (and therefore a service
// started with the same PATH) would run, plus other copies on PATH.
func runDoctor(args []string) {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			fmt.Fprintf(os.Stderr, "Usage: %s doctor [agent-type...]\n\nDefault agent types: %s\n", appName, strings.Join(core.DoctorAgentTypes, " "))
			return
		}
	}
	agents := args
	if len(agents) == 0 {
		agents = core.DoctorAgentTypes
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if writeDoctorReport(ctx, os.Stdout, agents) {
		os.Exit(1)
	}
}

// writeDoctorReport prints one block per agent type and reports whether any
// CLI that is installed looked unhealthy (broken install, failed --version,
// or help no longer listing an option the adapter passes).
func writeDoctorReport(ctx context.Context, w io.Writer, agents []string) (unhealthy bool) {
	fmt.Fprintf(w, "PATH=%s\n\n", os.Getenv("PATH"))
	for _, agentType := range agents {
		info := core.InspectAgentCLI(ctx, agentType)
		result := core.AgentCLIDoctorResult(info)
		fmt.Fprintf(w, "%s %s (%s)\n", result.Status.Icon(), agentType, info.Binary)
		if info.Selected == nil {
			fmt.Fprintln(w, "   not installed")
		} else {
			fmt.Fprintf(w, "   path:    %s\n", info.Selected.Path)
			if info.Selected.OffPath {
				fmt.Fprintln(w, "   note:    not on PATH; found in the vendor's install location")
			}
			if info.Selected.Resolved != "" && info.Selected.Resolved != info.Selected.Path {
				fmt.Fprintf(w, "   target:  %s\n", info.Selected.Resolved)
			}
			if info.Version != "" {
				fmt.Fprintf(w, "   version: %s\n", info.Version)
			}
			if info.VersionErr != "" {
				fmt.Fprintf(w, "   version check failed: %s\n", info.VersionErr)
				unhealthy = true
			} else if problems := core.CheckAgentCLIOptions(ctx, agentType, info.Selected.Path); len(problems) > 0 {
				// The same check upgrades run before keeping a new version.
				fmt.Fprintf(w, "   options: %s\n", strings.Join(problems, "; "))
				unhealthy = true
			}
		}
		for _, other := range info.Others {
			state := "other copy, not used"
			if other.Broken {
				state = "broken"
				unhealthy = true
			}
			target := other.Resolved
			if target == "" {
				target = "?"
			}
			fmt.Fprintf(w, "   %s: %s → %s\n", state, other.Path, target)
		}
		fmt.Fprintln(w)
	}
	return unhealthy
}
