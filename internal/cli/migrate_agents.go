// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/danPoku/kaectx/internal/agentcfg"
)

// runMigrateAgents handles `kae migrate-agents`: a one-off for machines that
// registered the MCP server while it was called ctx. Agents keep the name
// they were given, so without this they would go on starting the deprecated
// `ctx` binary and showing its tools as mcp__ctx__*.
func runMigrateAgents(args []string) error {
	fs := flag.NewFlagSet("migrate-agents", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show what would change without changing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	return migrateAgents(os.Stdout, home, agentcfg.ExecRunner{}, *dryRun)
}

func migrateAgents(w io.Writer, home string, run agentcfg.Runner, dryRun bool) error {
	claude, err := agentcfg.MigrateClaude(home, run, dryRun)
	if err != nil {
		return err
	}
	codex, err := agentcfg.MigrateCodex(home, dryRun)
	if err != nil {
		return err
	}
	changes := append(claude, codex...)

	verb := ""
	if dryRun {
		verb = "would have "
	}
	for _, c := range changes {
		switch {
		case c.Manual:
			fmt.Fprintf(w, "%s: to do by hand: %s\n", c.Agent, c.Detail)
		default:
			fmt.Fprintf(w, "%s: %s%s\n", c.Agent, verb, c.Detail)
		}
	}
	if len(changes) == 0 {
		fmt.Fprintln(w, "nothing to change: no agent has a ctx registration")
	}

	// Tool names follow the server name, so allowlists and instructions
	// written for mcp__ctx__* stop matching. Say where they are; changing a
	// permission list is the user's call.
	for _, path := range agentcfg.LegacyToolMentions(home) {
		fmt.Fprintf(w, "note: %s still names mcp__ctx__ tools; they are now mcp__kae__\n", path)
	}
	if len(changes) > 0 && !dryRun {
		fmt.Fprintln(w, "agents already running keep the old server until you start a new chat")
	}
	return nil
}
