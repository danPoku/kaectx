// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/danPoku/kaectx/internal/agentcfg"
	"github.com/danPoku/kaectx/internal/firstrun"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/service"
)

// runSetup handles `kae setup`: everything between "the binary is installed"
// and "my agents can search their history", in one command.
//
// Done by hand it is a different edit per agent, a service file per
// operating system, and two commands to fill the database. Each of those is
// a place where a new user gives up. Setup is like the engineer who comes to
// connect a new appliance: it finds what is in the house, connects what is
// there, asks before doing anything optional, and can be called back without
// doing the work twice.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print every change without making it")
	yes := fs.Bool("yes", false, "answer yes to every question")
	noDaemon := fs.Bool("no-daemon", false, "do not offer to run the daemon as a background service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	return setup(setupOptions{
		Home: home, Exe: selfCommand(), GOOS: runtime.GOOS, UID: os.Getuid(),
		DryRun: *dryRun, Yes: *yes, NoDaemon: *noDaemon,
		Run: agentcfg.ExecRunner{}, In: os.Stdin, Out: os.Stdout,
		Ingest: firstIngest,
	})
}

type setupOptions struct {
	Home string
	// Exe is the command agents and the service are told to run.
	Exe  string
	GOOS string
	UID  int

	DryRun, Yes, NoDaemon bool

	Run agentcfg.Runner
	In  io.Reader
	Out io.Writer

	// Ingest creates the database if needed and reads the agents' logs,
	// returning a line that says what it did.
	Ingest func() (string, error)
}

func setup(o setupOptions) error {
	var changes []agentcfg.Change
	asked := bufio.NewReader(o.In)
	say := func(format string, a ...any) { fmt.Fprintf(o.Out, format+"\n", a...) }

	// confirm asks a yes/no question. No answer (stdin closed, as when setup
	// is run from a script) counts as no: an optional change is never made
	// on silence. --yes answers yes; --dry-run does not ask at all.
	confirm := func(question string) bool {
		if o.DryRun {
			say("would ask: %s", question)
			return false
		}
		if o.Yes {
			return true
		}
		fmt.Fprintf(o.Out, "%s [y/N] ", question)
		line, err := asked.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if err != nil && answer == "" {
			say("(no answer; skipped. Run with --yes to accept)")
			return false
		}
		return answer == "y" || answer == "yes"
	}

	hasClaude, hasCodex := agentcfg.HasClaude(o.Home), agentcfg.HasCodex(o.Home)
	if !hasClaude && !hasCodex {
		say("no supported agent found: looked for Claude Code (~/.claude) and Codex (~/.codex)")
	}

	if hasClaude {
		// An old `ctx` registration is moved over first, so the agent ends
		// with one server, not two that answer the same questions.
		legacy := agentcfg.LegacyClaudeRegistered(o.Home)
		migrated, err := agentcfg.MigrateClaude(o.Home, o.Run, o.DryRun)
		if err != nil {
			return err
		}
		// When the migration only described its work (a dry run, or no
		// `claude` command to do it with), the kae entry it describes does
		// not exist yet, and asking for it again here would list the same
		// registration twice.
		_, cliErr := o.Run.LookPath("claude")
		described := legacy && (o.DryRun || cliErr != nil)
		var added []agentcfg.Change
		if !described {
			added, err = agentcfg.EnsureClaude(o.Home, o.Run, o.Exe, o.DryRun)
			if err != nil {
				return err
			}
		}
		got := append(migrated, added...)
		if len(got) == 0 {
			say("Claude Code: already connected")
		}
		changes = append(changes, got...)
	}

	if hasCodex {
		migrated, err := agentcfg.MigrateCodex(o.Home, o.DryRun)
		if err != nil {
			return err
		}
		state, err := agentcfg.ReadCodex(o.Home)
		if err != nil {
			return err
		}
		approve := false
		if !state.ApprovalSet {
			approve = confirm("Codex: let `codex exec` call kae's tools without asking? (Interactive Codex asks each time either way; without this, `codex exec` refuses them.)")
		}
		// In a dry run the migration above did not rename the old table, so
		// adding a new one as well would describe a change that a real run
		// would not make.
		var added []agentcfg.Change
		if !(o.DryRun && len(migrated) > 0 && !migrated[0].Manual) {
			added, err = agentcfg.EnsureCodex(o.Home, o.Exe, approve, o.DryRun)
			if err != nil {
				return err
			}
		}
		got := append(migrated, added...)
		if len(got) == 0 {
			say("Codex: already connected")
		}
		changes = append(changes, got...)
	}

	if o.DryRun {
		say("would create the database if needed and ingest existing session logs")
	} else {
		summary, err := o.Ingest()
		if err != nil {
			return err
		}
		say("%s", summary)
	}

	if !o.NoDaemon && service.Supported(o.GOOS) {
		svc := service.Options{GOOS: o.GOOS, Home: o.Home, Exe: o.Exe, UID: o.UID, Run: o.Run, DryRun: o.DryRun}
		switch {
		case service.Installed(svc):
			say("daemon: already installed as a background service")
		case confirm("Run the daemon in the background from now on, so new sessions are indexed as they happen?"):
			got, err := service.Install(svc)
			if err != nil {
				return err
			}
			changes = append(changes, got...)
		case !o.DryRun:
			say("daemon: not installed. Without it, kae catches up when an agent starts its MCP server; run `kae daemon` or `kae setup` again to change that.")
		}
	}

	verb := ""
	if o.DryRun {
		verb = "would have "
	}
	manual := 0
	for _, c := range changes {
		if c.Manual {
			manual++
			say("%s: to do by hand: %s", c.Agent, c.Detail)
			continue
		}
		say("%s: %s%s", c.Agent, verb, c.Detail)
	}
	switch {
	case len(changes) == 0:
		say("nothing to change: kae is already set up")
	case o.DryRun:
		say("dry run: nothing was changed")
	case len(changes) > manual:
		say("done. Agents already running pick kae up when you start a new chat.")
	}
	return nil
}

// firstIngest is setup's ingest step: openStore creates ~/.kae and the
// database on a new machine, then every agent's logs are read. If a daemon
// already holds the ingest lock it is doing this very job, which is success,
// not an error.
func firstIngest() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	db, _, err := openStore()
	if err != nil {
		return "", err
	}
	defer db.Close()

	release, err := ingest.Lock(ingestLockPath())
	if errors.Is(err, ingest.ErrIngestBusy) {
		return "ingest: another kae process (the daemon, or an agent's MCP server) is already reading the session logs", nil
	}
	if err != nil {
		return "", err
	}
	defer release()

	before, err := firstrun.CountSessions(db)
	if err != nil {
		return "", err
	}
	ok, failed := 0, 0
	for _, src := range agentSources {
		results, err := ingest.RunAll(db, src.newAdapter, src.root(home))
		if err != nil {
			return "", fmt.Errorf("%s: %w", src.name, err)
		}
		for _, r := range results {
			if r.Err != nil {
				failed++
				continue
			}
			ok++
		}
	}
	line := fmt.Sprintf("ingested %d session log file(s), %d failed", ok, failed)
	// The same report `kae ingest` gives when it fills an empty database.
	// For most people setup is that first ingest, so this is where they
	// learn what kae already knows.
	if before == 0 {
		if summary, err := firstRunSummary(db); err == nil && summary != "" {
			line += "\n\n" + summary
		}
	}
	return line, nil
}

// selfCommand is the path agents and the service should use to start kae.
//
// The obvious answer, this process's own executable, is wrong under a
// package manager: Homebrew runs /opt/homebrew/Cellar/kae/0.3.0/bin/kae, a
// folder that disappears on the next upgrade, through the stable link
// /opt/homebrew/bin/kae. So when the `kae` found on PATH is this same
// binary, the PATH entry is used; it is the address that survives an
// upgrade. Otherwise the executable's own path is the only one known to
// work. An absolute path is used either way, because an agent's PATH is
// often not the shell's.
func selfCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return agentcfg.ServerName
	}
	// Run as the deprecated `ctx` alias, register the real command beside it.
	if agentcfg.IsLegacyCommand(exe) {
		if sibling := agentcfg.RenameCommand(exe); fileExists(sibling) {
			exe = sibling
		}
	}
	onPath, err := exec.LookPath(agentcfg.ServerName)
	if err != nil {
		return exe
	}
	if onPath, err = filepath.Abs(onPath); err != nil {
		return exe
	}
	a, errA := filepath.EvalSymlinks(onPath)
	b, errB := filepath.EvalSymlinks(exe)
	if errA == nil && errB == nil && a == b {
		return onPath
	}
	return exe
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
