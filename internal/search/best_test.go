// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// brokenEmbedder always fails — simulates Ollama not being reachable, or
// not having the model pulled.
type brokenEmbedder struct{}

func (brokenEmbedder) Model() string { return "broken" }
func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("simulated: connection refused")
}

func TestBestFallsBackToKeywordWhenEmbeddingFails(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	fixture, err := os.ReadFile("../ingest/claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sessionPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionPath, fixture, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := ingest.Run(db, claudecode.New(), sessionPath); err != nil {
		t.Fatalf("ingest.Run: %v", err)
	}

	projectID, err := store.ResolveProject(db, "/home/dev/proj")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}

	got, err := search.Best(context.Background(), db, brokenEmbedder{}, "WAL", projectID, "", 10)
	if err != nil {
		t.Fatalf("Best: %v", err)
	}
	if got.UsedHybrid {
		t.Error("UsedHybrid = true, want false (the embedder always fails)")
	}
	if got.FallbackReason == nil {
		t.Error("FallbackReason = nil, want the embed error surfaced")
	}
	if len(got.Results) != 1 {
		t.Fatalf("Results = %d, want 1 (keyword search should still work)", len(got.Results))
	}
	if got.Results[0].Agent != "claude-code" {
		t.Errorf("Agent = %q, want claude-code", got.Results[0].Agent)
	}
}

func TestBestSkipsHybridWithNilClient(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	fixture, err := os.ReadFile("../ingest/claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sessionPath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionPath, fixture, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := ingest.Run(db, claudecode.New(), sessionPath); err != nil {
		t.Fatalf("ingest.Run: %v", err)
	}

	projectID, err := store.ResolveProject(db, "/home/dev/proj")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}

	got, err := search.Best(context.Background(), db, nil, "WAL", projectID, "", 10)
	if err != nil {
		t.Fatalf("Best: %v", err)
	}
	if got.UsedHybrid {
		t.Error("UsedHybrid = true, want false (nil client)")
	}
	if got.FallbackReason != nil {
		t.Errorf("FallbackReason = %v, want nil (a nil client isn't a failure)", got.FallbackReason)
	}
	if len(got.Results) != 1 {
		t.Fatalf("Results = %d, want 1", len(got.Results))
	}
}

func TestBestTrackedRecordsReturnedOrderWithoutChangingSearch(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	fixture, err := os.ReadFile("../ingest/claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, fixture, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	projectID, err := store.ResolveProject(db, "/home/dev/proj")
	if err != nil {
		t.Fatal(err)
	}

	got, err := search.BestTracked(context.Background(), db, nil, "WAL", projectID, "", 10, "mcp")
	if err != nil {
		t.Fatalf("BestTracked: %v", err)
	}
	if got.RunID == "" || got.TelemetryErr != nil {
		t.Fatalf("telemetry = (%q, %v), want a run id and no error", got.RunID, got.TelemetryErr)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(got.Results))
	}

	var query, surface, mode string
	var count int
	if err := db.QueryRow(`SELECT query, surface, mode, result_count FROM retrieval_runs WHERE id=?`, got.RunID).Scan(&query, &surface, &mode, &count); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if query != "WAL" || surface != "mcp" || mode != "keyword" || count != 1 {
		t.Errorf("run = (%q,%q,%q,%d), want (WAL,mcp,keyword,1)", query, surface, mode, count)
	}
	var rank int
	var evidence, scoreKind string
	var chunkID int64
	if err := db.QueryRow(`SELECT rank, evidence_key, chunk_id, score_kind FROM retrieval_candidates WHERE run_id=?`, got.RunID).Scan(&rank, &evidence, &chunkID, &scoreKind); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if rank != 1 || len(evidence) != 64 || chunkID != got.Results[0].ChunkID || scoreKind != "bm25" {
		t.Errorf("candidate = rank %d evidence %q chunk %d kind %q", rank, evidence, chunkID, scoreKind)
	}
}

func TestBestTrackedTelemetryFailureDoesNotFailSearch(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	fixture, _ := os.ReadFile("../ingest/claudecode/testdata/session.jsonl")
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, fixture, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatal(err)
	}
	projectID, _ := store.ResolveProject(db, "/home/dev/proj")

	got, err := search.BestTracked(context.Background(), db, nil, "WAL", projectID, "", 10, "invalid-surface")
	if err != nil {
		t.Fatalf("search failed with telemetry: %v", err)
	}
	if len(got.Results) != 1 || got.TelemetryErr == nil || got.RunID != "" {
		t.Fatalf("results=%d telemetry=(%q,%v), want successful search and failed telemetry", len(got.Results), got.RunID, got.TelemetryErr)
	}
}

func TestBestTrackedNormalizesNonPositiveLimit(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	projectID, err := store.ResolveProject(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := search.BestTracked(context.Background(), db, nil, "nothing", projectID, "", 0, "internal")
	if err != nil || got.TelemetryErr != nil || got.RunID == "" {
		t.Fatalf("BestTracked = run %q telemetry %v search %v", got.RunID, got.TelemetryErr, err)
	}
	var limit int
	if err := db.QueryRow(`SELECT requested_limit FROM retrieval_runs WHERE id=?`, got.RunID).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != 10 {
		t.Errorf("requested_limit = %d, want normalized default 10", limit)
	}
}
