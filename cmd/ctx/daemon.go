// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danPoku/kaectx/internal/daemon"
	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/ingest"
)

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fallback := fs.Duration("fallback-interval", 5*time.Minute, "how often to do a full re-scan as a safety net for missed filesystem events")
	debounce := fs.Duration("debounce", 500*time.Millisecond, "how long a session file must go quiet before it's re-ingested")
	embedInterval := fs.Duration("embed-interval", 30*time.Second, "how often to embed newly ingested chunks via Ollama for semantic search (0 disables)")
	batchSize := fs.Int("embed-batch", 50, "chunks to embed per Ollama round trip")
	ollamaURL := fs.String("ollama-url", embed.DefaultBaseURL, "Ollama server URL")
	model := fs.String("model", embed.DefaultModel, "embedding model name")
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

	sources := make([]daemon.Source, len(agentSources))
	for i, src := range agentSources {
		sources[i] = daemon.Source{Agent: src.name, Root: src.root(home), NewAdapter: src.newAdapter}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The daemon owns ingest for as long as it runs: holding the ingest lock
	// makes MCP servers skip their own catch-ups (the daemon is already
	// current) and stops `ctx ingest` from racing it. If an MCP server is
	// midway through a catch-up, wait for it rather than give up.
	release, err := ingest.Lock(ingestLockPath(home))
	if errors.Is(err, ingest.ErrIngestBusy) {
		fmt.Println("ctx daemon: another ctx process is ingesting; waiting for it to finish")
		release, err = ingest.LockWait(ctx, ingestLockPath(home), 2*time.Second)
	}
	if err != nil {
		return err
	}
	defer release()

	fmt.Printf("ctx daemon: watching %d source(s), press Ctrl+C to stop\n", len(sources))
	for _, src := range sources {
		fmt.Printf("  %s: %s\n", src.Agent, src.Root)
	}

	// Embedding runs beside ingest, not inside it: ingest must never wait on
	// Ollama, and Ollama being down must never stop ingest.
	if *embedInterval > 0 {
		fmt.Printf("  embedding: every %s via %s (%s)\n", *embedInterval, *ollamaURL, *model)
		go embed.RunEvery(ctx, db, embed.NewOllamaClient(*ollamaURL, *model), *batchSize, *embedInterval,
			func(format string, args ...any) { fmt.Printf(format+"\n", args...) })
	}

	return daemon.Run(ctx, db, sources, *fallback, *debounce)
}
