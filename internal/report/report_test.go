// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/store"
)

var (
	now   = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	since = now.AddDate(0, 0, -7) // 17 Nov 12:00
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// Times used by the seed: inside this period, inside the previous one, and
// before both. The two on the boundary pin down which side it belongs to.
const (
	thisWeek   = "2026-11-20T10:00:00.000Z"
	lastWeek   = "2026-11-12T10:00:00.000Z"
	longAgo    = "2026-10-01T10:00:00.000Z"
	atSince    = "2026-11-17T12:00:00.000Z" // the first instant of this period
	justBefore = "2026-11-17T11:59:59.999Z" // the last instant of the previous one
)

// seed fills two projects with activity at known times and returns their ids.
func seed(t *testing.T, db *sql.DB) (api, web int64) {
	t.Helper()
	api = exec(t, db, `INSERT INTO projects(name) VALUES ('api')`)
	web = exec(t, db, `INSERT INTO projects(name) VALUES ('web')`)

	session := func(id, agent string, project int64, created string) {
		exec(t, db, `INSERT INTO sessions(id, agent, native_id, project_id, started_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, agent, id, project, created, created[:19]+"Z")
	}
	session("claude-code:a", "claude-code", api, thisWeek)
	session("claude-code:b", "claude-code", api, atSince)
	session("codex:c", "codex", api, thisWeek)
	session("codex:d", "codex", api, lastWeek)
	session("claude-code:e", "claude-code", api, longAgo)
	session("claude-code:w", "claude-code", web, thisWeek)

	message := func(sessionID string, seq int, content, at string) {
		exec(t, db, `INSERT INTO messages(session_id, seq, role, kind, content, created_at) VALUES (?, ?, 'tool', 'tool_result', ?, ?)`, sessionID, seq, content, at)
	}
	message("claude-code:a", 0, "API_KEY=[REDACTED] and TOKEN=[REDACTED]", thisWeek)
	message("claude-code:a", 1, "nothing secret here", thisWeek)
	message("codex:d", 0, "password [REDACTED]", lastWeek)
	message("claude-code:w", 0, "[REDACTED] [REDACTED] [REDACTED]", thisWeek)

	run := func(id string, project int64, surface string, results int, at string) {
		exec(t, db, `INSERT INTO retrieval_runs(id, project_id, surface, query, requested_limit, mode, duration_ms, result_count, created_at)
			VALUES (?, ?, ?, 'q', 10, 'keyword', 5, ?, ?)`, id, project, surface, results, at)
		for rank := 1; rank <= results; rank++ {
			exec(t, db, `INSERT INTO retrieval_candidates(run_id, rank, evidence_key, session_id, first_seq, last_seq, score, score_kind)
				VALUES (?, ?, ?, 'claude-code:a', 0, 1, 1.0, 'bm25')`, id, rank, id+"-"+string(rune('0'+rank)))
		}
	}
	run("r1", api, "mcp", 2, thisWeek)
	run("r2", api, "mcp", 0, thisWeek)
	run("r3", api, "cli", 1, atSince)
	run("r4", api, "mcp", 1, justBefore)
	run("r5", api, "mcp", 1, lastWeek)
	run("r6", web, "mcp", 1, thisWeek)

	open := func(runID string, rank int, at string) {
		exec(t, db, `INSERT INTO retrieval_open_events(run_id, rank, opened_at) VALUES (?, ?, ?)`, runID, rank, at)
	}
	open("r1", 1, thisWeek)
	open("r1", 1, thisWeek) // the same result opened again: still one result
	open("r1", 2, thisWeek)
	open("r5", 1, lastWeek)
	open("r6", 1, thisWeek)

	label := func(project int64, outcome, at string) {
		exec(t, db, `INSERT INTO memory_observations(project_id, evidence_key, outcome, created_at) VALUES (?, 'k', ?, ?)`, project, outcome, at)
	}
	label(api, "helpful", thisWeek)
	label(api, "helpful", thisWeek)
	label(api, "stale", thisWeek)
	label(api, "unhelpful", lastWeek)
	label(web, "helpful", thisWeek)

	verdict := func(project int64, outcome string, reason any, at string) {
		exec(t, db, `INSERT INTO retrieval_run_observations(project_id, outcome, reason, created_at) VALUES (?, ?, ?, ?)`, project, outcome, reason, at)
	}
	verdict(api, "helpful", nil, thisWeek)
	verdict(api, "partial", "ranking", thisWeek)
	verdict(api, "unhelpful", "not_indexed", lastWeek)

	// Handoffs: one tagged note saved and read this week, one tagged note
	// saved this week and never read, one checkpoint handoff from last week.
	read := exec(t, db, `INSERT INTO notes(project_id, kind, title, tags, created_at) VALUES (?, 'todo', 'handoff 1', '["handoff"]', ?)`, api, thisWeek)
	exec(t, db, `INSERT INTO notes(project_id, kind, title, tags, created_at) VALUES (?, 'todo', 'handoff 2', '["handoff"]', ?)`, api, thisWeek)
	exec(t, db, `INSERT INTO note_retrievals(run_id, rank, note_id, created_at) VALUES ('n1', 1, ?, '2026-11-21T10:00:00.000Z')`, read)
	old := exec(t, db, `INSERT INTO notes(project_id, kind, title, created_at) VALUES (?, 'todo', 'checkpoint handoff', ?)`, api, lastWeek)
	exec(t, db, `INSERT INTO checkpoint_observations(session_id, project_id, level, message_count, duration_minutes, files_changed, outcome, note_id, resolved_at)
		VALUES ('codex:d', ?, 'checkpoint_recommended', 400, 240, 3, 'saved_handoff', ?, ?)`, api, old, lastWeek)
	return api, web
}

func TestBuildCountsEachPeriod(t *testing.T) {
	db := openDB(t)
	api, _ := seed(t, db)

	r, err := Build(db, api, since, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Project != "api" || r.Period.Since != "2026-11-17T12:00:00Z" || r.Period.Until != "2026-11-24T12:00:00Z" ||
		r.Previous.Since != "2026-11-10T12:00:00Z" || r.Previous.Until != "2026-11-17T12:00:00Z" {
		t.Fatalf("periods wrong: %+v", r)
	}

	n := r.Now
	checks := []struct {
		name      string
		got, want int
	}{
		// r1, r2, r3 (r3 sits exactly on the start, which is inside).
		{"searches", n.Searches, 3},
		{"searches mcp", n.SearchesBySurface["mcp"], 2},
		{"searches cli", n.SearchesBySurface["cli"], 1},
		{"searches empty", n.SearchesEmpty, 1},
		{"results opened", n.ResultsOpened, 2},
		{"labels", n.LabelsTotal, 3},
		{"labels helpful", n.Labels["helpful"], 2},
		{"labels stale", n.Labels["stale"], 1},
		{"labels unhelpful", n.Labels["unhelpful"], 0},
		{"verdicts", n.VerdictsTotal, 2},
		{"verdicts partial", n.Verdicts["partial"], 1},
		{"handoffs saved", n.HandoffsSaved, 2},
		{"handoffs picked up", n.HandoffsPickedUp, 1},
		{"sessions", n.SessionsIngested, 3},
		{"sessions claude-code", n.SessionsByAgent["claude-code"], 2},
		{"sessions codex", n.SessionsByAgent["codex"], 1},
		{"redactions", n.Redactions, 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("this period: %s = %d, want %d", c.name, c.got, c.want)
		}
	}

	p := r.Before
	before := []struct {
		name      string
		got, want int
	}{
		// r4 (one millisecond before the start) and r5.
		{"searches", p.Searches, 2},
		{"results opened", p.ResultsOpened, 1},
		{"labels", p.LabelsTotal, 1},
		{"labels unhelpful", p.Labels["unhelpful"], 1},
		{"verdicts unhelpful", p.Verdicts["unhelpful"], 1},
		{"handoffs saved", p.HandoffsSaved, 1},
		{"handoffs picked up", p.HandoffsPickedUp, 0},
		{"sessions", p.SessionsIngested, 1},
		{"redactions", p.Redactions, 1},
	}
	for _, c := range before {
		if c.got != c.want {
			t.Errorf("previous period: %s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// Every figure must equal a direct query against the same database. The
// queries here are written independently of the package's own: plain string
// comparison on timestamps that are all in one format in this seed.
func TestFiguresMatchDirectQueries(t *testing.T) {
	db := openDB(t)
	api, _ := seed(t, db)
	r, err := Build(db, api, since, now)
	if err != nil {
		t.Fatal(err)
	}
	direct := func(query string, args ...any) int {
		var n int
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	const lo, hi = "2026-11-17T12:00:00.000Z", "2026-11-24T12:00:00.000Z"
	for name, pair := range map[string][2]int{
		"searches":       {r.Now.Searches, direct(`SELECT count(*) FROM retrieval_runs WHERE project_id = ? AND created_at >= ? AND created_at < ?`, api, lo, hi)},
		"searches empty": {r.Now.SearchesEmpty, direct(`SELECT count(*) FROM retrieval_runs WHERE project_id = ? AND result_count = 0 AND created_at >= ? AND created_at < ?`, api, lo, hi)},
		"results opened": {r.Now.ResultsOpened, direct(`SELECT count(DISTINCT e.run_id || '/' || e.rank) FROM retrieval_open_events e JOIN retrieval_runs rr ON rr.id = e.run_id WHERE rr.project_id = ? AND e.opened_at >= ? AND e.opened_at < ?`, api, lo, hi)},
		"labels":         {r.Now.LabelsTotal, direct(`SELECT count(*) FROM memory_observations WHERE project_id = ? AND created_at >= ? AND created_at < ?`, api, lo, hi)},
		"verdicts":       {r.Now.VerdictsTotal, direct(`SELECT count(*) FROM retrieval_run_observations WHERE project_id = ? AND created_at >= ? AND created_at < ?`, api, lo, hi)},
		"sessions":       {r.Now.SessionsIngested, direct(`SELECT count(*) FROM sessions WHERE project_id = ? AND created_at >= '2026-11-17T12:00:00Z' AND created_at < '2026-11-24T12:00:00Z'`, api)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: report says %d, a direct query says %d", name, pair[0], pair[1])
		}
	}
	// Redactions, counted in Go instead of in SQL.
	rows, err := db.Query(`SELECT m.content FROM messages m JOIN sessions s ON s.id = m.session_id WHERE s.project_id = ? AND m.created_at >= ? AND m.created_at < ?`, api, lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	redactions := 0
	for rows.Next() {
		var content string
		rows.Scan(&content)
		redactions += strings.Count(content, "[REDACTED]")
	}
	if r.Now.Redactions != redactions {
		t.Errorf("redactions: report says %d, counting the text says %d", r.Now.Redactions, redactions)
	}
}

func TestAllProjectsAddsThemUp(t *testing.T) {
	db := openDB(t)
	api, web := seed(t, db)
	all, err := Build(db, 0, since, now)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Build(db, api, since, now)
	w, _ := Build(db, web, since, now)
	if all.Project != "" {
		t.Errorf("all-projects report is named %q", all.Project)
	}
	if all.Now.Searches != a.Now.Searches+w.Now.Searches || all.Now.Redactions != a.Now.Redactions+w.Now.Redactions ||
		all.Now.SessionsIngested != a.Now.SessionsIngested+w.Now.SessionsIngested || all.Now.LabelsTotal != a.Now.LabelsTotal+w.Now.LabelsTotal {
		t.Errorf("all projects (%+v) is not api (%+v) plus web (%+v)", all.Now, a.Now, w.Now)
	}
}

// A period with no activity is all zeros, with every key present, in both
// the text and the JSON; it is not an error.
func TestEmptyPeriodIsZerosNotAnError(t *testing.T) {
	db := openDB(t)
	api, _ := seed(t, db)
	// A week in 2030: nothing happened in it or in the week before.
	until := time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC)
	r, err := Build(db, api, until.AddDate(0, 0, -7), until)
	if err != nil {
		t.Fatal(err)
	}
	text := Format(r)
	for _, want := range []string{
		"kae report: api, 2030-01-01 to 2030-01-08",
		"searches run              0  (=)     mcp 0, cli 0; 0 returned nothing",
		"results labelled          0  (=)     helpful 0, unhelpful 0, stale 0, incorrect 0, superseded 0",
		"search verdicts           0  (=)     helpful 0, partial 0, unhelpful 0",
		"handoffs saved            0  (=)     0 picked up within 7 days",
		"secrets redacted          0  (=)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"searches":0`, `"labels":{"helpful":0,"incorrect":0,"stale":0,"superseded":0,"unhelpful":0}`, `"sessions_by_agent":{}`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("JSON lacks %s:\n%s", want, data)
		}
	}
	// A database with nothing in it at all behaves the same.
	if _, err := Build(openDB(t), 0, since, now); err != nil {
		t.Fatalf("empty database: %v", err)
	}
}

func TestFormatShowsChange(t *testing.T) {
	db := openDB(t)
	api, _ := seed(t, db)
	r, err := Build(db, api, since, now)
	if err != nil {
		t.Fatal(err)
	}
	text := Format(r)
	for _, want := range []string{
		"kae report: api, 2026-11-17 to 2026-11-24",
		"the period before it (2026-11-10 to 2026-11-17)",
		"searches run              3  (+1)    mcp 2, cli 1; 1 returned nothing",
		"results opened            2  (+1)",
		"handoffs saved            2  (+1)    1 picked up within 7 days",
		"sessions ingested         3  (+2)    claude-code 2, codex 1",
		"secrets redacted          2  (+1)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestParseSince(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"7d", now.AddDate(0, 0, -7), false},
		{"2w", now.AddDate(0, 0, -14), false},
		{"36h", now.Add(-36 * time.Hour), false},
		{"2026-11-17", time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), false},
		{"2026-11-17T09:30:00Z", time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC), false},
		{"", time.Time{}, true},
		{"0d", time.Time{}, true},
		{"-3d", time.Time{}, true},
		{"7x", time.Time{}, true},
		{"soon", time.Time{}, true},
		{"2030-01-01", time.Time{}, true}, // not in the past
	}
	for _, c := range cases {
		got, err := ParseSince(c.in, now)
		if (err != nil) != c.wantErr || (!c.wantErr && !got.Equal(c.want)) {
			t.Errorf("ParseSince(%q) = %v, %v; want %v, error %v", c.in, got, err, c.want, c.wantErr)
		}
	}
}
