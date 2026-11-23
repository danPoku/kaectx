// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// exampleDB builds the small corpus testdata/example-cases.json is written
// against: one project, a claude-code session with two exchanges and a codex
// session with one. It returns the path with the database closed and its WAL
// folded in, as a copy handed to the tool would be.
func exampleDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctx.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO projects(id, name, git_remote) VALUES (1, 'example-api', 'github.com/example/example-api')`,
		`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES
			('claude-code:example-1', 'claude-code', 'example-1', 1, '2026-09-21T09:00:00Z'),
			('codex:example-2', 'codex', 'example-2', 1, '2026-09-21T10:00:00Z')`,
		`INSERT INTO messages(session_id, seq, role, content, created_at) VALUES
			('claude-code:example-1', 0, 'user', 'Why WAL mode?', '2026-09-21T09:00:00Z'),
			('claude-code:example-1', 1, 'assistant', 'WAL lets readers and the writer work together.', '2026-09-21T09:00:05Z'),
			('claude-code:example-1', 2, 'user', 'And busy_timeout?', '2026-09-21T09:05:00Z'),
			('claude-code:example-1', 3, 'assistant', 'busy_timeout waits for a lock instead of failing.', '2026-09-21T09:05:05Z'),
			('codex:example-2', 4, 'user', 'Should I checkpoint now?', '2026-09-21T10:00:00Z'),
			('codex:example-2', 5, 'assistant', 'Yes, checkpoint before the migration.', '2026-09-21T10:00:05Z')`,
		`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES
			('claude-code:example-1', 0, 1, 'User: Why WAL mode? Assistant: WAL lets readers and the writer work together.'),
			('claude-code:example-1', 2, 3, 'User: And busy_timeout? Assistant: busy_timeout waits for a lock instead of failing.'),
			('codex:example-2', 4, 5, 'User: Should I checkpoint now? Assistant: Yes, checkpoint before the migration.')`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt[:40], err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScoreResults(t *testing.T) {
	hit := func(session string, first, last int) search.Result {
		return search.Result{SessionID: session, FirstSeq: first, LastSeq: last}
	}
	// Eleven unrelated hits, to push an answer past the cut-off.
	var filler []search.Result
	for i := 0; i < 11; i++ {
		filler = append(filler, hit("other", i*10, i*10+1))
	}
	one := evalCase{Expected: []expectedRange{{SessionID: "s", FirstSeq: 10, LastSeq: 20}}}
	two := evalCase{Expected: []expectedRange{{SessionID: "s", FirstSeq: 10, LastSeq: 20}, {SessionID: "t", FirstSeq: 0, LastSeq: 5}}}
	cases := []struct {
		name       string
		c          evalCase
		results    []search.Result
		wantRecall float64
		wantRR     float64
		wantDeep   int
	}{
		{"no results", one, nil, 0, 0, 0},
		{"exact range at rank 1", one, []search.Result{hit("s", 10, 20)}, 1, 1, 1},
		{"overlapping range counts", one, []search.Result{hit("s", 18, 30)}, 1, 1, 1},
		{"touching the last message counts", one, []search.Result{hit("s", 20, 25)}, 1, 1, 1},
		{"adjacent range does not", one, []search.Result{hit("s", 21, 30)}, 0, 0, 0},
		{"same range in another session does not", one, []search.Result{hit("x", 10, 20)}, 0, 0, 0},
		{"answer at rank 3", one, []search.Result{hit("a", 0, 1), hit("b", 0, 1), hit("s", 10, 20)}, 1, 1.0 / 3, 3},
		{"half of two answers found", two, []search.Result{hit("a", 0, 1), hit("t", 2, 3)}, 0.5, 0.5, 2},
		{"both answers found, first one sets the rank", two, []search.Result{hit("t", 0, 5), hit("s", 10, 20)}, 1, 1, 1},
		// Rank 12 is read (deepRank) but does not count toward either measure.
		{"answer past the cut-off", one, append(append([]search.Result{}, filler...), hit("s", 10, 20)), 0, 0, 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scoreResults(c.c, c.results)
			if math.Abs(got.recall-c.wantRecall) > 1e-12 || math.Abs(got.reciprocalRank-c.wantRR) > 1e-12 || got.deepRank != c.wantDeep {
				t.Errorf("recall = %v, reciprocal rank = %v, deep rank = %d; want %v, %v, %d",
					got.recall, got.reciprocalRank, got.deepRank, c.wantRecall, c.wantRR, c.wantDeep)
			}
		})
	}
}

func TestLoadCasesRejectsBadFiles(t *testing.T) {
	cases := []struct {
		name, body, wantInErr string
	}{
		{"no id", `{"cases":[{"query":"q","expect_none":true}]}`, "has no id"},
		{"duplicate id", `{"cases":[{"id":"a","query":"q","expect_none":true},{"id":"a","query":"q","expect_none":true}]}`, "duplicate case id"},
		{"no query", `{"cases":[{"id":"a","expect_none":true}]}`, "has no query"},
		{"neither answer nor expect_none", `{"cases":[{"id":"a","query":"q"}]}`, "expected ranges or expect_none"},
		{"both answer and expect_none", `{"cases":[{"id":"a","query":"q","expect_none":true,"expected":[{"session_id":"s","first_seq":0,"last_seq":1}]}]}`, "expected ranges or expect_none"},
		{"bad window", `{"cases":[{"id":"a","query":"q","expect_none":true,"since":"last tuesday"}]}`, "case a"},
		{"not json", `{`, "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cases.json")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadCases(path)
			if err == nil || !strings.Contains(err.Error(), c.wantInErr) {
				t.Errorf("loadCases error = %v, want one containing %q", err, c.wantInErr)
			}
		})
	}
}

// TestEvaluateExampleCases runs the shipped example file end to end, keyword
// only so it needs no Ollama, and checks each case's score.
func TestEvaluateExampleCases(t *testing.T) {
	path := exampleDB(t)
	cases, err := loadCases("testdata/example-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := map[string]outcome{}
	for _, o := range evaluate(context.Background(), db, nil, cases) {
		got[o.c.ID] = o
	}
	want := []struct {
		id       string
		recall   float64
		rr       float64
		returned int
	}{
		{"helpful-001", 1, 1, 1},
		{"miss-001", 0.5, 1, 1},
		// The window opens after the answer, so the search finds nothing.
		{"stale-001", 0, 0, 0},
		{"empty-001", 0, 0, 0},
	}
	for _, w := range want {
		o, ok := got[w.id]
		if !ok || o.skipped != "" {
			t.Fatalf("%s: missing or skipped (%q)", w.id, o.skipped)
		}
		for mode, s := range map[string]score{"normal": o.normal, "risk-aware": o.riskAware} {
			if math.Abs(s.recall-w.recall) > 1e-12 || math.Abs(s.reciprocalRank-w.rr) > 1e-12 || s.returned != w.returned {
				t.Errorf("%s %s: recall %v, reciprocal rank %v, %d returned; want %v, %v, %d",
					w.id, mode, s.recall, s.reciprocalRank, s.returned, w.recall, w.rr, w.returned)
			}
		}
	}

	all := summarise(evaluate(context.Background(), db, nil, cases))
	if all.cases != 3 || all.emptyCases != 1 || all.emptyCorrect != 1 || all.foundNormal != 2 || len(all.failingNormal) != 1 {
		t.Errorf("summary = %+v, want 3 answerable cases (2 found, 1 failing) and 1 correct empty", all)
	}
	if math.Abs(all.recallNormal-0.5) > 1e-12 || math.Abs(all.mrrNormal-2.0/3) > 1e-12 {
		t.Errorf("recall = %v, MRR = %v; want 0.5 and 0.667", all.recallNormal, all.mrrNormal)
	}
}

// A case naming a project this database does not hold is skipped and
// reported, not silently scored as a miss.
func TestEvaluateSkipsUnknownProject(t *testing.T) {
	db, err := openReadOnly(exampleDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := evaluate(context.Background(), db, nil, []evalCase{{ID: "x", Query: "WAL", Project: "nowhere", ExpectNone: true}})
	if len(out) != 1 || !strings.Contains(out[0].skipped, "nowhere") {
		t.Fatalf("outcome = %+v, want it skipped with the project named", out)
	}
	if s := summarise(out); s.skipped != 1 || s.cases != 0 || s.emptyCases != 0 {
		t.Errorf("summary = %+v, want the case counted only as skipped", s)
	}
}

// TestRunEvalNeverWrites is #43's acceptance check: the database file's size
// and modification time are the same after a full run, and SQLite refuses a
// write through the tool's handle.
func TestRunEvalNeverWrites(t *testing.T) {
	path := exampleDB(t)
	ro, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Exec(`DELETE FROM chunks`); err == nil {
		t.Error("a write through the read-only handle succeeded")
	}
	ro.Close()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var full, public bytes.Buffer
	if err := runEval(path, "testdata/example-cases.json", nil, false, &full); err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if err := runEval(path, "testdata/example-cases.json", nil, true, &public); err != nil {
		t.Fatalf("runEval (public): %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("the database file changed: %v/%d before, %v/%d after", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
	// No telemetry row either: the searches went through the untracked path.
	check, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var runs int
	if err := check.QueryRow(`SELECT COUNT(*) FROM retrieval_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("the evaluation recorded %d retrieval runs, want none", runs)
	}

	for _, want := range []string{"3 with a known answer, 1 expecting no result", "keyword only", "Recall@10 normal",
		"| All | 3 | 0.500 | 0.500 | 0.667 | 0.667 | 2 | 2 |", "1 of 1 correctly returned nothing",
		"`stale-001` (stale): busy_timeout; not in the top 50",
		"Database file unchanged by this run (size and modification time): true"} {
		if !strings.Contains(full.String(), want) {
			t.Errorf("report missing %q:\n%s", want, full.String())
		}
	}
	// The public report names the failing case but not what was asked.
	if !strings.Contains(public.String(), "`stale-001` (stale); not in the top 50") || strings.Contains(public.String(), "busy_timeout") {
		t.Errorf("the public report must list failing cases by ID with no query text:\n%s", public.String())
	}
}
