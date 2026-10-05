// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// searchFlagsWithValue lists this command's flags and whether they take a
// value, so reorderFlagsFirst can hoist them ahead of the query words.
var searchFlagsWithValue = map[string]bool{"agent": true, "limit": true, "risk-aware": false, "since": true, "until": true}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	agent := fs.String("agent", "", "restrict to one agent (default: all agents)")
	limit := fs.Int("limit", 10, "max results")
	riskAware := fs.Bool("risk-aware", false, "rerank using credible retrieval outcomes")
	since := fs.String("since", "", "only turns at or after this UTC date (2026-09-28) or RFC 3339 time")
	until := fs.String("until", "", "only turns before this time; a date means through the end of that day")
	if err := fs.Parse(reorderFlagsFirst(args, searchFlagsWithValue)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: ctx search <query> [--agent NAME] [--limit N] [--since DATE] [--until DATE] [--risk-aware]")
	}
	query := strings.Join(fs.Args(), " ")
	window, err := search.ParseWindow(*since, *until)
	if err != nil {
		return err
	}

	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	projectID, err := store.ResolveProject(db, cwd)
	if err != nil {
		return fmt.Errorf("resolve project for %s: %w", cwd, err)
	}

	// Best tries semantic+keyword fusion and falls back to keyword-only on
	// its own — cheap to always offer a client here, since Hybrid only
	// actually reaches Ollama if vec0 (and chunk_vectors) are usable at all.
	client := embed.NewOllamaClient(embed.DefaultBaseURL, embed.DefaultModel)
	got, err := search.BestTrackedIn(context.Background(), db, client, query, projectID, *agent, window, *limit, "cli", *riskAware)
	if err != nil {
		return err
	}
	if got.FallbackReason != nil {
		fmt.Fprintf(os.Stderr, "ctx search: semantic search unavailable, falling back to keyword-only: %v\n", got.FallbackReason)
	}
	if got.TelemetryErr != nil {
		fmt.Fprintf(os.Stderr, "ctx search: warning: retrieval telemetry was not recorded: %v\n", got.TelemetryErr)
	}
	if got.GovernanceErr != nil {
		fmt.Fprintf(os.Stderr, "ctx search: warning: risk-aware reranking was not applied: %v\n", got.GovernanceErr)
	}
	// The CLI only warns; it never ingests on its own, because a search
	// that silently turns into a multi-minute ingest would be surprising.
	if home, err := os.UserHomeDir(); err == nil {
		if f, err := ingest.CheckFreshness(db, ingestRoots(home), time.Now()); err == nil && f.PendingFiles > 0 {
			fmt.Fprintf(os.Stderr, "ctx search: warning: %d session log file(s) not ingested yet (oldest from %s); results may be missing recent work. Run `ctx ingest`, or `ctx daemon` to stay current.\n",
				f.PendingFiles, f.OldestPendingAt)
		}
	}
	if len(got.Results) == 0 {
		fmt.Println("no results")
		return nil
	}
	for _, r := range got.Results {
		fmt.Printf("[%s] %s  %s  seq %d-%d\n    %s\n\n", r.Agent, r.At, r.SessionID, r.FirstSeq, r.LastSeq, r.Snippet)
	}
	return nil
}
