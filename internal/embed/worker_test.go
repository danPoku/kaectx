// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/store"
)

type fakeEmbedder struct {
	calls  int
	failAt int // 1-based call number to fail on; 0 = never fail
	model  string
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		return nil, errors.New("simulated embed failure")
	}
	return fixedVector(Dims, 0.1), nil
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedChunk inserts a minimal session (with or without a project) and one
// chunk belonging to it, returning the chunk's id.
func seedChunk(t *testing.T, db *sql.DB, sessionID string, withProject bool, text string) int64 {
	t.Helper()
	var projectID any
	if withProject {
		res, err := db.Exec(`INSERT INTO projects(name) VALUES (?)`, sessionID)
		if err != nil {
			t.Fatalf("insert project: %v", err)
		}
		id, _ := res.LastInsertId()
		projectID = id
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES (?,?,?,?, '2026-09-20T10:00:00Z')`,
		sessionID, "claude-code", sessionID, projectID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	res, err := db.Exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES (?,0,0,?)`, sessionID, text)
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestRunEmbedsPendingChunks(t *testing.T) {
	db := openTestDB(t)
	c1 := seedChunk(t, db, "claude-code:s1", true, "how do I open the db in WAL mode")
	c2 := seedChunk(t, db, "claude-code:s2", true, "how do I redact api keys")

	client := &fakeEmbedder{model: "nomic-embed-text"}
	n, err := Run(context.Background(), db, client, 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 2 {
		t.Fatalf("processed = %d, want 2", n)
	}
	if client.calls != 2 {
		t.Errorf("Embed called %d times, want 2", client.calls)
	}

	for _, id := range []int64{c1, c2} {
		var embeddedAt, model sql.NullString
		if err := db.QueryRow(`SELECT embedded_at, embedding_model FROM chunks WHERE id = ?`, id).Scan(&embeddedAt, &model); err != nil {
			t.Fatalf("query chunk %d: %v", id, err)
		}
		if !embeddedAt.Valid || embeddedAt.String == "" {
			t.Errorf("chunk %d: embedded_at not set", id)
		}
		if model.String != "nomic-embed-text" {
			t.Errorf("chunk %d: embedding_model = %q, want nomic-embed-text", id, model.String)
		}

		var vecCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_vectors WHERE chunk_id = ?`, id).Scan(&vecCount); err != nil {
			t.Fatalf("query chunk_vectors for %d: %v", id, err)
		}
		if vecCount != 1 {
			t.Errorf("chunk %d: chunk_vectors rows = %d, want 1", id, vecCount)
		}
	}
}

func TestRunSkipsChunksWithNoProject(t *testing.T) {
	db := openTestDB(t)
	seedChunk(t, db, "claude-code:orphan", false, "a session with no resolved project")

	client := &fakeEmbedder{model: "nomic-embed-text"}
	n, err := Run(context.Background(), db, client, 10)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Errorf("processed = %d, want 0 (no project to partition the vector on)", n)
	}
	if client.calls != 0 {
		t.Errorf("Embed called %d times, want 0 — should never even try", client.calls)
	}

	// Still pending, not silently marked embedded.
	var embeddedAt sql.NullString
	if err := db.QueryRow(`SELECT embedded_at FROM chunks`).Scan(&embeddedAt); err != nil {
		t.Fatalf("query chunk: %v", err)
	}
	if embeddedAt.Valid {
		t.Error("embedded_at is set, want it to stay NULL (still pending)")
	}
}

func TestRunRespectsBatchSize(t *testing.T) {
	db := openTestDB(t)
	seedChunk(t, db, "claude-code:s1", true, "one")
	seedChunk(t, db, "claude-code:s2", true, "two")
	seedChunk(t, db, "claude-code:s3", true, "three")

	client := &fakeEmbedder{model: "nomic-embed-text"}
	n, err := Run(context.Background(), db, client, 2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 2 {
		t.Fatalf("processed = %d, want 2 (limited by batchSize)", n)
	}

	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE embedded_at IS NULL`).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending after batch = %d, want 1", pending)
	}
}

func TestRunStopsOnEmbedErrorButKeepsPriorProgress(t *testing.T) {
	db := openTestDB(t)
	seedChunk(t, db, "claude-code:s1", true, "one")
	seedChunk(t, db, "claude-code:s2", true, "two")
	seedChunk(t, db, "claude-code:s3", true, "three")

	client := &fakeEmbedder{model: "nomic-embed-text", failAt: 2}
	n, err := Run(context.Background(), db, client, 10)
	if err == nil {
		t.Fatal("Run: nil error, want the simulated failure surfaced")
	}
	if n != 1 {
		t.Fatalf("processed = %d, want 1 (the one chunk that succeeded before the failure)", n)
	}

	var embedded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE embedded_at IS NOT NULL`).Scan(&embedded); err != nil {
		t.Fatalf("count embedded: %v", err)
	}
	if embedded != 1 {
		t.Errorf("embedded chunks in DB = %d, want 1 (not rolled back)", embedded)
	}
}

func TestDrainEmbedsEverythingAcrossBatchesAndStopsOnSkippedOnly(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 5; i++ {
		seedChunk(t, db, fmt.Sprintf("claude-code:s%d", i), true, "chunk text")
	}
	// A chunk whose session has no project is skipped by Run and stays
	// pending; Drain must still terminate instead of retrying it forever.
	seedChunk(t, db, "claude-code:orphan", false, "no project")

	client := &fakeEmbedder{model: "nomic-embed-text"}
	total, err := Drain(context.Background(), db, client, 2) // 5 chunks over 3 batches
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if total != 5 {
		t.Errorf("Drain embedded %d, want 5", total)
	}
}

func TestDrainReportsPartialProgressOnError(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 4; i++ {
		seedChunk(t, db, fmt.Sprintf("claude-code:s%d", i), true, "chunk text")
	}
	client := &fakeEmbedder{model: "nomic-embed-text", failAt: 3}
	total, err := Drain(context.Background(), db, client, 10)
	if err == nil {
		t.Fatal("expected the simulated failure to surface")
	}
	if total != 2 {
		t.Errorf("partial progress = %d, want 2 (chunks embedded before the failing one)", total)
	}
}

func TestRunEveryLogsAFailureOnceThenRecovery(t *testing.T) {
	db := openTestDB(t)
	seedChunk(t, db, "claude-code:s1", true, "chunk text")

	client := &flakyEmbedder{fakeEmbedder: fakeEmbedder{model: "nomic-embed-text"}, failFirst: 3}
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	RunEvery(ctx, db, client, 10, 30*time.Millisecond, logf)

	mu.Lock()
	defer mu.Unlock()
	var paused, recovered, embedded int
	for _, l := range lines {
		switch {
		case strings.Contains(l, "paused"):
			paused++
		case strings.Contains(l, "recovered"):
			recovered++
		case strings.Contains(l, "embedded 1 chunk"):
			embedded++
		}
	}
	if paused != 1 {
		t.Errorf("failure logged %d times, want exactly once despite %d failing ticks: %v", paused, client.failFirst, lines)
	}
	if recovered != 1 || embedded != 1 {
		t.Errorf("recovered=%d embedded=%d, want 1 each: %v", recovered, embedded, lines)
	}
}

// flakyEmbedder fails its first failFirst Embed calls, like an Ollama that is
// down for a while and then comes back.
type flakyEmbedder struct {
	fakeEmbedder
	failFirst int
}

func (f *flakyEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return nil, errors.New("connection refused")
	}
	return fixedVector(Dims, 0.1), nil
}

// A batch that is entirely no-project chunks must not hide the embeddable
// chunks queued behind it: with batchSize 2 and two orphans first, the old
// query returned only the orphans, Run processed 0 and Drain stopped.
func TestDrainReachesEmbeddableChunksBehindNoProjectChunks(t *testing.T) {
	db := openTestDB(t)
	seedChunk(t, db, "claude-code:orphan1", false, "no project one")
	seedChunk(t, db, "claude-code:orphan2", false, "no project two")
	good := seedChunk(t, db, "claude-code:good", true, "has a project")

	client := &fakeEmbedder{model: "nomic-embed-text"}
	total, err := Drain(context.Background(), db, client, 2)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if total != 1 {
		t.Fatalf("Drain embedded %d, want 1 (the chunk behind the orphans)", total)
	}

	var embeddedAt sql.NullString
	if err := db.QueryRow(`SELECT embedded_at FROM chunks WHERE id = ?`, good).Scan(&embeddedAt); err != nil {
		t.Fatalf("query chunk: %v", err)
	}
	if !embeddedAt.Valid {
		t.Error("embeddable chunk still pending")
	}
	var stillPending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE embedded_at IS NULL`).Scan(&stillPending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if stillPending != 2 {
		t.Errorf("pending = %d, want 2 (the orphans stay pending)", stillPending)
	}
}
