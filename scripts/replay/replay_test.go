// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/store"
)

// fixture is a small ctx database: one project, one claude-code session with
// two exchanges (A at messages 0-1, B at 2-3), and whatever searches and
// labels a test adds.
type fixture struct {
	t    *testing.T
	path string
	db   *sql.DB
	runs int
}

const (
	keyA = "evidence-a"
	keyB = "evidence-b"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctx.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, path: path, db: db}
	f.exec(`INSERT INTO projects(id, name) VALUES (1, 'p')`)
	f.exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES ('s', 'claude-code', 's', 1, '2026-10-01T00:00:00Z')`)
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

// search records a search at time at that returned A at rank 1 and B at rank
// 2, and returns its run ID.
func (f *fixture) search(at string) string {
	f.t.Helper()
	f.runs++
	id := fmt.Sprintf("run-%02d", f.runs)
	f.exec(`INSERT INTO retrieval_runs(id, project_id, surface, query, requested_limit, mode, duration_ms, result_count, created_at)
		VALUES (?, 1, 'mcp', 'q', 10, 'keyword', 1, 2, ?)`, id, at)
	f.exec(`INSERT INTO retrieval_candidates(run_id, rank, evidence_key, session_id, first_seq, last_seq, score, score_kind) VALUES
		(?, 1, ?, 's', 0, 1, 0, 'bm25'), (?, 2, ?, 's', 2, 3, 0, 'bm25')`, id, keyA, id, keyB)
	return id
}

// label records an outcome at time at for the hit at rank in run.
func (f *fixture) label(run string, rank int, outcome, at string) {
	f.t.Helper()
	key, anchor := keyA, 0
	if rank == 2 {
		key, anchor = keyB, 2
	}
	f.exec(`INSERT INTO memory_observations(project_id, evidence_key, session_id, anchor_seq, run_id, candidate_rank, outcome, created_at)
		VALUES (1, ?, 's', ?, ?, ?, ?, ?)`, key, anchor, run, rank, outcome, at)
}

func (f *fixture) load() ([]run, []label) {
	f.t.Helper()
	runs, err := loadRuns(f.db, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	labels, err := loadLabels(f.db, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return runs, labels
}

func runByID(t *testing.T, runs []run, id string) run {
	t.Helper()
	for _, r := range runs {
		if r.id == id {
			return r
		}
	}
	t.Fatalf("run %s not loaded", id)
	return run{}
}

// TestNoLookAhead is #37's acceptance case: a label created after a search
// does not affect that search's replay, under either method. Three searches
// on 1, 2 and 3 October each collect an "unhelpful" label for the rank-1 hit
// a day later, so by the fourth search A is credibly bad.
func TestNoLookAhead(t *testing.T) {
	f := newFixture(t)
	first := f.search("2026-10-01T10:00:00.000Z")
	second := f.search("2026-10-02T10:00:00.000Z")
	third := f.search("2026-10-03T10:00:00.000Z")
	f.label(first, 1, "unhelpful", "2026-10-04T09:00:00.000Z")
	f.label(second, 1, "unhelpful", "2026-10-04T09:00:01.000Z")
	f.label(third, 1, "unhelpful", "2026-10-04T09:00:02.000Z")
	late := f.search("2026-10-05T10:00:00.000Z")
	runs, labels := f.load()

	asIs, swapped := []int{0, 1}, []int{1, 0}
	cases := []struct {
		name          string
		run           string
		wantBaseline  []int
		wantClassRank []int
	}{
		// Every label postdates these three searches: nothing may move.
		{"first search, all labels in its future", first, asIs, asIs},
		{"third search, all labels in its future", third, asIs, asIs},
		// The same hits, searched after the labels exist: A drops.
		{"later search sees the labels", late, swapped, swapped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := replay(runByID(t, runs, c.run), labels)
			if !sameOrder(got.baseline, c.wantBaseline) {
				t.Errorf("baseline order = %v, want %v", got.baseline, c.wantBaseline)
			}
			if !sameOrder(got.class, c.wantClassRank) {
				t.Errorf("class-prior order = %v, want %v", got.class, c.wantClassRank)
			}
		})
	}

	// The cutoff is the only thing holding the early replays still: backdate
	// the labels to before the third search and it moves too.
	f.exec(`UPDATE memory_observations SET created_at = '2026-10-03T09:00:0' || (id % 10) || '.000Z'`)
	_, backdated := f.load()
	if got := replay(runByID(t, runs, third), backdated); !sameOrder(got.baseline, swapped) || !sameOrder(got.class, swapped) {
		t.Errorf("with the labels backdated, baseline = %v and class = %v; want both swapped", got.baseline, got.class)
	}
	// A label created at the very instant of the search is not "before" it.
	f.exec(`UPDATE memory_observations SET created_at = '2026-10-03T10:00:00.000Z'`)
	_, sameInstant := f.load()
	if got := replay(runByID(t, runs, third), sameInstant); !sameOrder(got.baseline, asIs) || !sameOrder(got.class, asIs) {
		t.Errorf("labels at the search's own instant moved it: baseline = %v, class = %v", got.baseline, got.class)
	}
}

// TestBaselineMatchesGovernance holds this tool's restatement of the baseline
// to the real thing. With every label in the past, the replayed reliability of
// each hit must equal governance.AssessMany's, across the neutral zone and
// both sides of the credibility threshold.
func TestBaselineMatchesGovernance(t *testing.T) {
	cases := []struct {
		name      string
		outcomesA []string // labels on exchange A
		outcomesB []string // labels on exchange B
	}{
		{"no labels", nil, nil},
		{"two labels stay neutral", []string{"unhelpful", "unhelpful"}, nil},
		{"three unhelpful", []string{"unhelpful", "stale", "incorrect"}, nil},
		{"mixed record", []string{"helpful", "unhelpful", "helpful", "superseded"}, []string{"helpful", "helpful", "helpful"}},
		{"long clean record", nil, []string{"helpful", "helpful", "helpful", "helpful", "helpful", "helpful"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			n := 0
			add := func(rank int, outcomes []string) {
				for _, o := range outcomes {
					n++
					id := f.search(fmt.Sprintf("2026-10-01T10:%02d:00.000Z", n))
					f.label(id, rank, o, fmt.Sprintf("2026-10-02T10:%02d:00.000Z", n))
				}
			}
			add(1, c.outcomesA)
			add(2, c.outcomesB)
			last := f.search("2026-11-01T00:00:00.000Z")
			runs, labels := f.load()
			r := runByID(t, runs, last)

			want, err := governance.AssessMany(context.Background(), f.db, 1, []governance.Evidence{
				{Key: keyA, SessionID: "s", FirstSeq: 0, LastSeq: 1},
				{Key: keyB, SessionID: "s", FirstSeq: 2, LastSeq: 3},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, cand := range r.cands {
				var own tally
				for _, l := range labels {
					if l.matches(cand, r.projectID) {
						own.add(l.helpful)
					}
				}
				if got := baselineReliability(own); math.Abs(got-want[cand.key].Reliability) > 1e-12 {
					t.Errorf("%s: replayed reliability %v, governance says %v (n=%d)", cand.key, got, want[cand.key].Reliability, own.n)
				}
				if own.n != want[cand.key].Observations {
					t.Errorf("%s: replay matched %d labels, governance %d", cand.key, own.n, want[cand.key].Observations)
				}
			}
		})
	}
}

func TestClassReliability(t *testing.T) {
	cases := []struct {
		name       string
		own, class tally
		want       float64
	}{
		{"no labels anywhere is neutral", tally{}, tally{}, 1},
		{"no own labels takes the class rate", tally{}, tally{n: 10, helpful: 8}, 0.8},
		{"own labels, empty class, blends with 1.0", tally{n: 5, helpful: 0}, tally{}, 0.5},
		// Z = 5/(5+5) = 0.5: half own rate (1.0), half class rate (0.6).
		{"own and class blend by credibility", tally{n: 5, helpful: 5}, tally{n: 10, helpful: 6}, 0.8},
		// Z = 1/6: one label of its own barely moves a hit off its class.
		{"one own label counts for a sixth", tally{n: 1, helpful: 0}, tally{n: 10, helpful: 9}, 0.75},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classReliability(c.own, c.class); math.Abs(got-c.want) > 1e-12 {
				t.Errorf("classReliability = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRankBucket(t *testing.T) {
	for rank, want := range map[int]string{1: "1", 2: "2-3", 3: "2-3", 4: "4+", 10: "4+", 25: "4+"} {
		if got := rankBucket(rank); got != want {
			t.Errorf("rankBucket(%d) = %q, want %q", rank, got, want)
		}
	}
}

// TestReplayNeverWrites: the tool opens the database so SQLite refuses
// writes, and a whole run leaves the file's size and modification time alone.
func TestReplayNeverWrites(t *testing.T) {
	f := newFixture(t)
	first := f.search("2026-10-01T10:00:00.000Z")
	f.label(first, 1, "unhelpful", "2026-10-02T10:00:00.000Z")
	f.search("2026-10-03T10:00:00.000Z")
	// Fold the WAL into the main file and close, so the file on disk is the
	// whole database, as a copy handed to the tool would be.
	f.exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := openReadOnly(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Exec(`DELETE FROM memory_observations`); err == nil {
		t.Error("a write through the read-only handle succeeded")
	}
	ro.Close()

	before, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runReplay(f.path, 0, &out); err != nil {
		t.Fatalf("runReplay: %v", err)
	}
	after, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("the database file changed: %v/%d before, %v/%d after", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
	for _, want := range []string{"Searches replayed: 2", "Per-hit labels: 1", "Class prior", "Database file unchanged by this run (size and modification time): true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
}
