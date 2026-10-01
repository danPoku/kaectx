// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command ctx is the single binary for the local multi-agent context store:
// ingesting agent sessions, indexing them, and serving them back over MCP
// and the CLI.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danPoku/kaectx/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "init":
		if err := runInit(); err != nil {
			fmt.Fprintln(os.Stderr, "ctx init:", err)
			os.Exit(1)
		}
	case "ingest":
		if err := runIngest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx ingest:", err)
			os.Exit(1)
		}
	case "search":
		if err := runSearch(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx search:", err)
			os.Exit(1)
		}
	case "explain":
		if err := runExplain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx explain:", err)
			os.Exit(1)
		}
	case "repair":
		if err := runRepair(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx repair:", err)
			os.Exit(1)
		}
	case "project":
		if err := runProject(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx project:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := runMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx mcp:", err)
			os.Exit(1)
		}
	case "embed":
		if err := runEmbed(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx embed:", err)
			os.Exit(1)
		}
	case "audit-secrets":
		if err := runAuditSecrets(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx audit-secrets:", err)
			os.Exit(1)
		}
	case "daemon":
		if err := runDaemon(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "ctx daemon:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ctx: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: ctx <command> [flags]

commands:
  init                                create ~/.ctx and its database
  ingest [--path FILE] [--agent A]    ingest sessions (default: scan Claude Code + Codex session dirs)
  embed [--batch N]                   embed pending chunks via a local Ollama for semantic search
  search <query> [--agent A] [--limit N]   keyword+semantic search over ingested sessions in this project
  explain <path> [--limit N]          show sessions, actions and git span for a project-relative file
  repair git [--dry-run]              re-derive git commit spans and repo-root file paths for sessions ingested by older versions
  repair touches [--dry-run]          backfill file records for Codex apply_patch calls ingested before ctx read them
  project list | merge <from> <into>  list projects, or fold a duplicate (e.g. from a deleted checkout) into another
  audit-secrets                       re-scan stored text with the redaction rules and report residual secrets
  mcp                                 serve ctx's tools over MCP (stdio) for an agent to call
  daemon [--fallback-interval D]      watch session dirs continuously and ingest live (Ctrl+C to stop)`)
}

// reorderFlagsFirst hoists any token in args matching a name in
// flagsWithValue (plus the value token right after it) to the front, in
// place, leaving the rest as a trailing run of positional args.
//
// Go's flag package stops parsing at the first non-flag token, so
// "ctx search some query --agent codex" would otherwise swallow "--agent
// codex" into the query text itself instead of parsing it as a flag — a
// query-first-then-flags order is the natural way to type a search command,
// so it needs to actually work.
func reorderFlagsFirst(args []string, flagsWithValue map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		name, _, hasEquals := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if !flagsWithValue[name] {
			positional = append(positional, args[i])
			continue
		}
		flags = append(flags, args[i])
		if !hasEquals && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// openStore resolves ~/.ctx, creates and secures it if it doesn't already
// exist, and returns an open, migrated database. Every subcommand that
// touches the store goes through this — not just `ctx init` — so a fresh
// machine doesn't require remembering to run `ctx init` before anything
// else works. This is also what a locally-registered MCP server relies on:
// an agent's config points straight at `ctx mcp` with no setup step of its
// own, so `ctx mcp` on a machine that never ran `ctx init` needs to just
// work, not fail with "no such file or directory".
func openStore() (*sql.DB, store.MigrateResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, store.MigrateResult{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".ctx")

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, store.MigrateResult{}, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := secureDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "ctx: warning: %v\n", err)
	}

	db, err := store.Open(filepath.Join(dir, "ctx.db"))
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

	fmt.Println("initialized ~/.ctx/ctx.db")
	if !result.VectorsLoaded {
		fmt.Fprintf(os.Stderr, "ctx init: warning: semantic search unavailable, sqlite-vec did not load: %v\n", result.VectorsErr)
	}
	return nil
}

// secureDir makes sure dir is readable only by its owner. ~/.ctx holds
// redacted transcripts pulled from every coding agent on the machine —
// silently accepting a pre-existing world- or group-readable directory is
// the wrong default. os.MkdirAll(dir, 0700) only sets that mode when it
// creates the directory; if ~/.ctx already existed with looser permissions
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
