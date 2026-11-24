// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danPoku/kaectx/internal/firstrun"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

// agentSource pairs an agent with where its session logs live and how to
// get a fresh adapter for them (RunAll needs a new instance per file — see
// its doc for why: Codex's adapter is stateful within a file).
type agentSource struct {
	name       string
	root       func(home string) string
	newAdapter func() ingest.Adapter
}

var agentSources = []agentSource{
	{
		name:       "claude-code",
		root:       func(home string) string { return filepath.Join(home, ".claude", "projects") },
		newAdapter: func() ingest.Adapter { return claudecode.New() },
	},
	{
		name:       "codex",
		root:       func(home string) string { return filepath.Join(home, ".codex", "sessions") },
		newAdapter: func() ingest.Adapter { return codex.New() },
	},
}

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	path := fs.String("path", "", "ingest a single JSONL file instead of scanning the default session directories")
	agent := fs.String("agent", "claude-code", "which adapter to use with --path (claude-code or codex)")
	quiet := fs.Bool("quiet", false, "do not print the summary shown after the first ingest into an empty database")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	// One ingester at a time; see ingest.Lock. If the daemon holds the lock
	// it is already keeping the index current, so there is nothing to do.
	release, err := ingest.Lock(ingestLockPath())
	if err != nil {
		return err
	}
	defer release()

	if *path != "" {
		src, err := findAgentSource(*agent)
		if err != nil {
			return err
		}
		if err := ingest.Run(db, src.newAdapter(), *path); err != nil {
			return fmt.Errorf("%s: %w", *path, err)
		}
		fmt.Println("ingested", *path)
		return nil
	}

	// Whether this ingest is the one that fills the database decides what is
	// printed afterwards, so it has to be asked before any row is written.
	before, err := firstrun.CountSessions(db)
	if err != nil {
		return err
	}

	ok, failed := 0, 0
	for _, src := range agentSources {
		results, err := ingest.RunAll(db, src.newAdapter, src.root(home))
		if err != nil {
			return fmt.Errorf("%s: %w", src.name, err)
		}
		for _, r := range results {
			if r.Err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "kae ingest: %s: %v\n", r.Path, r.Err)
				continue
			}
			ok++
		}
	}
	fmt.Printf("ingested %d file(s), %d failed\n", ok, failed)
	if before == 0 && !*quiet {
		summary, err := firstRunSummary(db)
		if err != nil {
			// The ingest itself succeeded; a summary that cannot be built
			// is worth a warning, not a failed command.
			fmt.Fprintf(os.Stderr, "kae ingest: warning: could not summarise the first ingest: %v\n", err)
		} else if summary != "" {
			fmt.Print("\n" + summary)
		}
	}
	return nil
}

// firstRunSummary describes a database that has just been filled for the
// first time: what was found, and a search to try from the project the user
// is standing in. It returns "" when the ingest found no sessions, since
// there is then nothing to show.
func firstRunSummary(db *sql.DB) (string, error) {
	overview, err := firstrun.Load(db)
	if err != nil || overview.Sessions == 0 {
		return "", err
	}
	// LookupProject, not ResolveProject: looking must not create a project
	// row for whatever folder the user happens to run the first ingest in.
	var projectID int64
	if cwd, err := os.Getwd(); err == nil {
		if id, found, err := store.LookupProject(db, cwd); err == nil && found {
			projectID = id
		}
	}
	suggestion, found, err := firstrun.Suggest(context.Background(), db, projectID)
	if err != nil {
		return "", err
	}
	if !found {
		return firstrun.Format(overview, nil), nil
	}
	return firstrun.Format(overview, &suggestion), nil
}

// ingestRoots is agentSources resolved against home, in the shape the
// ingest and mcpserver packages take.
func ingestRoots(home string) []ingest.Root {
	roots := make([]ingest.Root, len(agentSources))
	for i, src := range agentSources {
		roots[i] = ingest.Root{Agent: src.name, Dir: src.root(home), NewAdapter: src.newAdapter}
	}
	return roots
}

// ingestLockPath is the lock ingest.CatchUp uses to keep concurrent kae
// processes from catching up at the same time. It sits beside the database
// because that is the thing it protects.
//
// Call it after openStore: the data folder is resolved once per process and
// openStore is what creates it.
func ingestLockPath() string {
	loc, err := dataLocation()
	if err != nil {
		return ""
	}
	return loc.Lock
}

func findAgentSource(name string) (agentSource, error) {
	for _, src := range agentSources {
		if src.name == name {
			return src, nil
		}
	}
	return agentSource{}, fmt.Errorf("unknown agent %q (want claude-code or codex)", name)
}
