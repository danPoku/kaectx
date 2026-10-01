// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// searchFlagsWithValue lists this command's flags that take a value, so
// reorderFlagsFirst knows to keep each one paired with its following token
// when it hoists them ahead of the query words.
var searchFlagsWithValue = map[string]bool{"agent": true, "limit": true}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	agent := fs.String("agent", "", "restrict to one agent (default: all agents)")
	limit := fs.Int("limit", 10, "max results")
	if err := fs.Parse(reorderFlagsFirst(args, searchFlagsWithValue)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: ctx search <query> [--agent NAME] [--limit N]")
	}
	query := strings.Join(fs.Args(), " ")

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
	got, err := search.BestTracked(context.Background(), db, client, query, projectID, *agent, *limit, "cli")
	if err != nil {
		return err
	}
	if got.FallbackReason != nil {
		fmt.Fprintf(os.Stderr, "ctx search: semantic search unavailable, falling back to keyword-only: %v\n", got.FallbackReason)
	}
	if got.TelemetryErr != nil {
		fmt.Fprintf(os.Stderr, "ctx search: warning: retrieval telemetry was not recorded: %v\n", got.TelemetryErr)
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
