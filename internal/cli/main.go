// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command kae is the single binary for the local multi-agent context store:
// ingesting agent sessions, indexing them, and serving them back over MCP
// and the CLI.
package cli

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/danPoku/kaectx/internal/datadir"
	"github.com/danPoku/kaectx/internal/store"
)

// Main runs the command named by os.Args and exits the process.
func Main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "init":
		if err := runInit(); err != nil {
			fmt.Fprintln(os.Stderr, "kae init:", err)
			os.Exit(1)
		}
	case "ingest":
		if err := runIngest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae ingest:", err)
			os.Exit(1)
		}
	case "search":
		if err := runSearch(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae search:", err)
			os.Exit(1)
		}
	case "explain":
		if err := runExplain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae explain:", err)
			os.Exit(1)
		}
	case "repair":
		if err := runRepair(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae repair:", err)
			os.Exit(1)
		}
	case "project":
		if err := runProject(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae project:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := runMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae mcp:", err)
			os.Exit(1)
		}
	case "embed":
		if err := runEmbed(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae embed:", err)
			os.Exit(1)
		}
	case "audit-secrets":
		if err := runAuditSecrets(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae audit-secrets:", err)
			os.Exit(1)
		}
	case "feedback":
		if err := runFeedback(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae feedback:", err)
			os.Exit(1)
		}
	case "doctor":
		if err := runDoctor(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae doctor:", err)
			os.Exit(1)
		}
	case "daemon":
		if err := runDaemon(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae daemon:", err)
			os.Exit(1)
		}
	case "brief":
		if err := runBrief(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae brief:", err)
			os.Exit(1)
		}
	case "handoff":
		if err := runHandoff(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae handoff:", err)
			os.Exit(1)
		}
	case "resume":
		if err := runResume(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae resume:", err)
			os.Exit(1)
		}
	case "forget":
		if err := runForget(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae forget:", err)
			os.Exit(1)
		}
	case "ui":
		if err := runUI(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae ui:", err)
			os.Exit(1)
		}
	case "report":
		if err := runReport(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae report:", err)
			os.Exit(1)
		}
	case "setup":
		if err := runSetup(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae setup:", err)
			os.Exit(1)
		}
	case "migrate-agents":
		if err := runMigrateAgents(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kae migrate-agents:", err)
			os.Exit(1)
		}
	case "version", "--version":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "kae: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kae <command> [flags]

commands:
  setup [--dry-run] [--yes] [--no-daemon]   connect installed agents, ingest their history, and offer a background service
  init                                create ~/.kae and its database
  ingest [--path FILE] [--agent A] [--quiet]   ingest sessions (default: scan Claude Code + Codex session dirs)
  embed [--batch N]                   embed pending chunks via a local Ollama for semantic search
  search <query> [--agent A] [--limit N] [--since D] [--until D] [--risk-aware]   search ingested sessions in this project
  brief [--budget N]                  print the short summary of this project an agent is given at session start
  handoff [--session ID]              write a handoff note from this project's latest session, for any agent to pick up
  resume                              print this project's newest handoff note
  feedback <run_id> <rank> <outcome> [--note T]   label a search hit by its run and rank: helpful, unhelpful, stale, incorrect or superseded
  explain <path> [--limit N]          show sessions, actions and git span for a project-relative file
  repair git [--dry-run]              re-derive git commit spans and repo-root file paths for sessions ingested by older versions
  repair touches [--dry-run]          backfill file records for Codex apply_patch calls ingested before kae read them
  repair messages [--dry-run]         reclassify harness-injected user messages and rebuild affected chunks
  repair projects [--dry-run]         re-file Claude Code sessions under the project their main transcript starts in
  repair notes [--dry-run]            link saved notes to the exchange they were saved in, from messages already stored
  project list | merge <from> <into>  list projects, or fold a duplicate (e.g. from a deleted checkout) into another
  project exclude <id> [--forget] | include <id>   stop (or resume) ingesting a project; --forget also deletes what is stored
  forget <session-id> [--yes]         delete a session and everything stored from it; it is never ingested again
  ui [--port N] [--allow-edits]       browse what kae holds in a local web page (read-only unless --allow-edits)
  audit-secrets                       re-scan stored text with the redaction rules and report residual secrets
  report [--since D] [--all-projects] [--json]   what kae did over a period, in counts, against the period before
  doctor                              report ingestion, embedding, retrieval, and outcome health
  mcp                                 serve kae's tools over MCP (stdio) for an agent to call
  daemon [--fallback-interval D]      watch session dirs continuously and ingest live (Ctrl+C to stop)
  migrate-agents [--dry-run]          point Claude Code and Codex at kae instead of the old ctx MCP server
  version                             print the version of this binary`)
}

// reorderFlagsFirst hoists any actual flag token in args whose name appears
// in flagsWithValue. A true map value means the following token belongs to
// the flag; false represents a boolean flag.
//
// Go's flag package stops parsing at the first non-flag token, so
// "kae search some query --agent codex" would otherwise swallow "--agent
// codex" into the query text itself instead of parsing it as a flag — a
// query-first-then-flags order is the natural way to type a search command,
// so it needs to actually work.
func reorderFlagsFirst(args []string, flagsWithValue map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positional = append(positional, args[i])
			continue
		}
		name, _, hasEquals := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		takesValue, known := flagsWithValue[name]
		if !known {
			positional = append(positional, args[i])
			continue
		}
		flags = append(flags, args[i])
		if takesValue && !hasEquals && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// dataLocation resolves where this process keeps its data, once. The first
// call may move a catalogue left under the old name (see package datadir);
// every later call in the same process must agree with it, or the database
// and its ingest lock could end up in different folders.
func dataLocation() (datadir.Location, error) {
	locMu.Lock()
	defer locMu.Unlock()
	if locDone {
		return cachedLoc, locErr
	}
	locDone = true
	home, err := os.UserHomeDir()
	if err != nil {
		locErr = fmt.Errorf("resolve home directory: %w", err)
		return cachedLoc, locErr
	}
	cachedLoc, locErr = datadir.Resolve(home)
	if locErr == nil && cachedLoc.Notice != "" {
		fmt.Fprintf(os.Stderr, "kae: %s\n", cachedLoc.Notice)
	}
	return cachedLoc, locErr
}

var (
	locMu     sync.Mutex
	locDone   bool
	cachedLoc datadir.Location
	locErr    error
)

// WarnDeprecatedAlias prints the notice the `ctx` alias binary shows before
// running a command. It goes to stderr so a script reading the command's
// output, or an agent speaking MCP over stdout, is not disturbed by it.
func WarnDeprecatedAlias(w io.Writer) {
	fmt.Fprintln(w, "ctx: this command is now called kae. `ctx` still works in this release and will be removed in the next minor release; use `kae` instead, and run `kae migrate-agents` to update Claude Code and Codex.")
}

// openStore resolves the data folder (~/.kae), creates and secures it if it
// doesn't already exist, and returns an open, migrated database. Every
// subcommand that touches the store goes through this — not just `kae init`
// — so a fresh machine doesn't require remembering to run `kae init` before
// anything else works. This is also what a locally-registered MCP server
// relies on: an agent's config points straight at `kae mcp` with no setup
// step of its own, so `kae mcp` on a machine that never ran `kae init` needs
// to just work, not fail with "no such file or directory".
func openStore() (*sql.DB, store.MigrateResult, error) {
	loc, err := dataLocation()
	if err != nil {
		return nil, store.MigrateResult{}, err
	}

	if err := os.MkdirAll(loc.Dir, 0700); err != nil {
		return nil, store.MigrateResult{}, fmt.Errorf("create %s: %w", loc.Dir, err)
	}
	if err := secureDir(loc.Dir); err != nil {
		fmt.Fprintf(os.Stderr, "kae: warning: %v\n", err)
	}

	db, err := store.Open(loc.DB)
	if err != nil {
		return nil, store.MigrateResult{}, err
	}
	result, err := store.Migrate(db)
	if err != nil {
		db.Close()
		return nil, store.MigrateResult{}, err
	}
	return db, result, nil
}

func runInit() error {
	db, result, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	loc, err := dataLocation()
	if err != nil {
		return err
	}
	fmt.Println("initialized", loc.DB)
	if !result.VectorsLoaded {
		fmt.Fprintf(os.Stderr, "kae init: warning: semantic search unavailable, sqlite-vec did not load: %v\n", result.VectorsErr)
	}
	return nil
}

// secureDir makes sure dir is readable only by its owner. ~/.kae holds
// redacted transcripts pulled from every coding agent on the machine —
// silently accepting a pre-existing world- or group-readable directory is
// the wrong default. os.MkdirAll(dir, 0700) only sets that mode when it
// creates the directory; if ~/.kae already existed with looser permissions
// (created by hand, or by a future version with a different default),
// MkdirAll leaves it alone, so this checks and tightens it explicitly.
func secureDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if info.Mode().Perm() != 0700 {
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("%s is mode %#o (not 0700) and could not be tightened: %w", dir, info.Mode().Perm(), err)
		}
	}
	return nil
}
