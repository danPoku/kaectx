// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/store"
)

// fixedEmbedder returns one vector for any text, so the tests need no Ollama.
type fixedEmbedder struct{}

func (fixedEmbedder) Model() string { return "test-model" }
func (fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	vec := make([]float32, embed.Dims)
	for i := range vec {
		vec[i] = 0.1
	}
	return vec, nil
}

// fixtureDB holds one project with two embedded exchanges and three recorded
// hybrid searches of different durations, plus a keyword-only one that must
// be ignored. It returns the path with the database closed.
func fixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctx.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Migrate(db)
	if err != nil {
		t.Fatal(err)
	}
	if !result.VectorsLoaded {
		db.Close()
		t.Skip("vec0 did not load on this machine, skipping the stage-timing test")
	}
	for _, stmt := range []string{
		`INSERT INTO projects(id, name) VALUES (1, 'p')`,
		`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES ('s', 'claude-code', 's', 1, '2026-09-21T09:00:00Z')`,
		`INSERT INTO messages(session_id, seq, role, content, created_at) VALUES
			('s', 0, 'user', 'Why WAL mode?', '2026-09-21T09:00:00Z'),
			('s', 1, 'assistant', 'WAL lets readers and the writer work together.', '2026-09-21T09:00:05Z'),
			('s', 2, 'user', 'And busy_timeout?', '2026-09-21T09:05:00Z'),
			('s', 3, 'assistant', 'busy_timeout waits for a lock instead of failing.', '2026-09-21T09:05:05Z')`,
		`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES
			('s', 0, 1, 'User: Why WAL mode? Assistant: WAL lets readers and the writer work together.'),
			('s', 2, 3, 'User: And busy_timeout? Assistant: busy_timeout waits for a lock instead of failing.')`,
		`INSERT INTO retrieval_runs(id, project_id, surface, query, agent_filter, requested_limit, mode, duration_ms, result_count, created_at) VALUES
			('run-fast', 1, 'mcp', 'WAL', NULL, 10, 'hybrid', 120, 1, '2026-10-01T10:00:00.000Z'),
			('run-slowest', 1, 'mcp', 'busy_timeout', NULL, 10, 'hybrid', 9000, 1, '2026-10-02T10:00:00.000Z'),
			('run-slow-agent', 1, 'mcp', 'WAL OR busy_timeout', 'claude-code', 5, 'hybrid', 4000, 2, '2026-10-03T10:00:00.000Z'),
			('run-keyword', 1, 'cli', 'WAL', NULL, 10, 'keyword', 99000, 1, '2026-10-04T10:00:00.000Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt[:40], err)
		}
	}
	if n, err := embed.Run(context.Background(), db, fixedEmbedder{}, 10); err != nil || n != 2 {
		t.Fatalf("embed.Run = %d, %v; want both chunks embedded", n, err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSlowestPicksHybridRunsByDuration: the replay set is the slowest hybrid
// searches, slowest first. A keyword-only search has no embedding stage to
// time, however long it took.
func TestSlowestPicksHybridRunsByDuration(t *testing.T) {
	db, err := openReadOnly(fixtureDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range []struct {
		n    int
		want []string
	}{
		{2, []string{"run-slowest", "run-slow-agent"}},
		{10, []string{"run-slowest", "run-slow-agent", "run-fast"}},
	} {
		got, err := slowest(db, c.n)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.id)
		}
		if strings.Join(ids, ",") != strings.Join(c.want, ",") {
			t.Errorf("slowest(%d) = %v, want %v", c.n, ids, c.want)
		}
	}
}

// TestReplayTimesEveryStage runs each recorded search through every stage,
// including the agent-scoped variant of the fused query, and leaves the
// database file untouched.
func TestReplayTimesEveryStage(t *testing.T) {
	path := fixtureDB(t)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM retrieval_runs`); err == nil {
		t.Error("a write through the read-only handle succeeded")
	}
	runs, err := slowest(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	var timings []timing
	for _, r := range runs {
		got := replay(context.Background(), db, fixedEmbedder{}, r)
		if got.err != nil {
			t.Fatalf("replay(%s): %v", r.id, got.err)
		}
		if got.total() != got.embed+got.fused {
			t.Errorf("%s: total %v is not embedding %v plus fused %v", r.id, got.total(), got.embed, got.fused)
		}
		timings = append(timings, got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("the database file changed: %v/%d before, %v/%d after", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}

	var out bytes.Buffer
	writeReport(&out, timings, true)
	report := out.String()
	for _, want := range []string{"Ollama cold", "| run-slow | 9000 |", "| Embedding |", "| Fused query, windowed |", "Embedding is "} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	// The report is meant to be posted: it must not carry what was searched for.
	if strings.Contains(report, "busy_timeout") {
		t.Errorf("the report contains query text:\n%s", report)
	}
}

func TestPercentile(t *testing.T) {
	ds := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{{0, 1}, {0.5, 6}, {0.9, 10}, {1, 10}} {
		if got := percentile(ds, c.p); got != c.want {
			t.Errorf("percentile(%v) = %v, want %v", c.p, got, c.want)
		}
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile of nothing = %v, want 0", got)
	}
}
