// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package report counts what kae did over a period, for a person to read.
//
// `kae doctor` prints much of the same data as diagnostics: is the pipeline
// healthy? This is the other question: what did I get for it? It is the
// monthly statement to doctor's engineer's readout. A statement lists what
// happened and compares it with last month; it does not estimate what the
// customer would have spent elsewhere. So every figure here is a count of
// rows kae recorded, and nothing is an estimate of time or tokens saved.
package report

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danPoku/kaectx/internal/doctor"
)

// Counts is what happened in one period. Maps hold every known key, at zero
// when nothing was recorded, so the output has the same shape every time.
type Counts struct {
	// Searches is every recorded search (search_context and `kae search`);
	// SearchesBySurface splits it by where it was run, and SearchesEmpty is
	// how many returned nothing.
	Searches          int            `json:"searches"`
	SearchesBySurface map[string]int `json:"searches_by_surface"`
	SearchesEmpty     int            `json:"searches_empty"`
	// ResultsOpened is the number of distinct search results an agent
	// opened in full.
	ResultsOpened int `json:"results_opened"`
	// Labels counts the labels agents put on results, by outcome.
	Labels      map[string]int `json:"labels"`
	LabelsTotal int            `json:"labels_total"`
	// Verdicts counts whole-search verdicts, by outcome.
	Verdicts      map[string]int `json:"verdicts"`
	VerdictsTotal int            `json:"verdicts_total"`
	// HandoffsSaved is handoffs saved in the period; HandoffsPickedUp is how
	// many of those were read again within doctor.HandoffPickupDays.
	HandoffsSaved    int `json:"handoffs_saved"`
	HandoffsPickedUp int `json:"handoffs_picked_up"`
	// SessionsIngested is sessions first stored in the period, by agent.
	SessionsIngested int            `json:"sessions_ingested"`
	SessionsByAgent  map[string]int `json:"sessions_by_agent"`
	// Redactions is the number of places a secret was replaced in messages
	// written in the period.
	Redactions int `json:"redactions"`
}

// Period is a half-open time range [Since, Until), in RFC 3339 UTC.
type Period struct {
	Since string `json:"since"`
	Until string `json:"until"`
}

// Report is one period beside the period of the same length before it.
type Report struct {
	// Project is the project's name, or "" for every project.
	Project  string `json:"project"`
	Period   Period `json:"period"`
	Previous Period `json:"previous"`
	Now      Counts `json:"now"`
	Before   Counts `json:"before"`
}

var (
	surfaces = []string{"mcp", "cli", "internal"}
	labels   = []string{"helpful", "unhelpful", "stale", "incorrect", "superseded"}
	verdicts = []string{"helpful", "partial", "unhelpful"}
)

// ParseSince reads the --since value: a number of days, hours or weeks
// ("7d", "36h", "2w") counted back from now, or a date or RFC 3339 time.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("since is empty")
	}
	if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n > 0 {
		switch s[len(s)-1] {
		case 'h':
			return now.Add(-time.Duration(n) * time.Hour), nil
		case 'd':
			return now.AddDate(0, 0, -n), nil
		case 'w':
			return now.AddDate(0, 0, -7*n), nil
		}
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			if !t.Before(now) {
				return time.Time{}, fmt.Errorf("since %s is not in the past", s)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("since %q is not a period (7d, 36h, 2w), a date (2026-11-17) or a time", s)
}

// Build counts activity in [since, until) and in the period of the same
// length immediately before it. projectID 0 means every project.
func Build(db *sql.DB, projectID int64, since, until time.Time) (Report, error) {
	var r Report
	if projectID != 0 {
		if err := db.QueryRow(`SELECT name FROM projects WHERE id = ?`, projectID).Scan(&r.Project); err != nil {
			return r, fmt.Errorf("read project: %w", err)
		}
	}
	length := until.Sub(since)
	r.Period = period(since, until)
	r.Previous = period(since.Add(-length), since)
	var err error
	if r.Now, err = count(db, projectID, r.Period); err != nil {
		return r, err
	}
	if r.Before, err = count(db, projectID, r.Previous); err != nil {
		return r, err
	}
	return r, nil
}

func period(since, until time.Time) Period {
	return Period{Since: since.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339)}
}

// in is the SQL test "col falls in the period". The tables this reads were
// written by different code at different times, with and without fractional
// seconds and, for log timestamps, in whatever form the agent used;
// julianday compares them as times rather than as strings.
//
// The bounds are the numbered parameters ?1 and ?2 in every query here, with
// the project as ?3. Numbered rather than plain "?", because some queries
// mention ?3 or ?4 before the period test, and SQLite numbers a plain "?"
// one above the highest number already used.
func in(col string) string {
	return "julianday(" + col + ") >= julianday(?1) AND julianday(" + col + ") < julianday(?2)"
}

func count(db *sql.DB, projectID int64, p Period) (Counts, error) {
	c := Counts{
		SearchesBySurface: zeroes(surfaces), Labels: zeroes(labels), Verdicts: zeroes(verdicts),
		SessionsByAgent: map[string]int{},
	}
	// args are the period bounds, then the project twice for the
	// "?3 = 0 OR project = ?3" filter each query ends with.
	args := []any{p.Since, p.Until, projectID}

	if err := groupInto(db, c.SearchesBySurface, &c.Searches,
		`SELECT surface, count(*) FROM retrieval_runs WHERE `+in("created_at")+` AND (?3 = 0 OR project_id = ?3) GROUP BY surface`, args...); err != nil {
		return c, fmt.Errorf("count searches: %w", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM retrieval_runs
		WHERE `+in("created_at")+` AND (?3 = 0 OR project_id = ?3) AND result_count = 0`, args...).Scan(&c.SearchesEmpty); err != nil {
		return c, fmt.Errorf("count empty searches: %w", err)
	}
	// A result opened three times is one result opened.
	if err := db.QueryRow(`SELECT count(*) FROM (
			SELECT DISTINCT e.run_id, e.rank FROM retrieval_open_events e
			JOIN retrieval_runs rr ON rr.id = e.run_id
			WHERE `+in("e.opened_at")+` AND (?3 = 0 OR rr.project_id = ?3))`, args...).Scan(&c.ResultsOpened); err != nil {
		return c, fmt.Errorf("count opened results: %w", err)
	}
	if err := groupInto(db, c.Labels, &c.LabelsTotal,
		`SELECT outcome, count(*) FROM memory_observations WHERE `+in("created_at")+` AND (?3 = 0 OR project_id = ?3) GROUP BY outcome`, args...); err != nil {
		return c, fmt.Errorf("count labels: %w", err)
	}
	if err := groupInto(db, c.Verdicts, &c.VerdictsTotal,
		`SELECT outcome, count(*) FROM retrieval_run_observations WHERE `+in("created_at")+` AND (?3 = 0 OR project_id = ?3) GROUP BY outcome`, args...); err != nil {
		return c, fmt.Errorf("count search verdicts: %w", err)
	}

	// Handoffs, by the definition `kae doctor` uses: a checkpoint answered
	// with saved_handoff, or a note tagged 'handoff' that no checkpoint
	// names. Picked up means read again within HandoffPickupDays of being
	// saved, by a note search, the brief, `kae resume` or get_handoff.
	if err := db.QueryRow(`WITH handoffs(note_id, saved_at) AS (
			SELECT co.note_id, co.resolved_at FROM checkpoint_observations co
			 WHERE co.outcome = 'saved_handoff' AND (?3 = 0 OR co.project_id = ?3)
			UNION ALL
			SELECT n.id, n.created_at FROM notes n
			 WHERE (?3 = 0 OR n.project_id = ?3)
			   AND EXISTS (SELECT 1 FROM json_each(n.tags) t WHERE t.value = 'handoff')
			   AND NOT EXISTS (SELECT 1 FROM checkpoint_observations co WHERE co.note_id = n.id AND co.outcome = 'saved_handoff')
		)
		SELECT count(*), coalesce(sum(note_id IS NOT NULL AND EXISTS (
			SELECT 1 FROM note_retrievals nr WHERE nr.note_id = handoffs.note_id
			   AND julianday(nr.created_at) >= julianday(handoffs.saved_at)
			   AND julianday(nr.created_at) <= julianday(handoffs.saved_at) + ?4)), 0)
		FROM handoffs WHERE `+in("saved_at"), p.Since, p.Until, projectID, doctor.HandoffPickupDays).
		Scan(&c.HandoffsSaved, &c.HandoffsPickedUp); err != nil {
		return c, fmt.Errorf("count handoffs: %w", err)
	}

	if err := groupInto(db, c.SessionsByAgent, &c.SessionsIngested,
		`SELECT agent, count(*) FROM sessions WHERE `+in("created_at")+` AND (?3 = 0 OR project_id = ?3) GROUP BY agent`, args...); err != nil {
		return c, fmt.Errorf("count sessions: %w", err)
	}
	const placeholder = "[REDACTED]"
	if err := db.QueryRow(`SELECT coalesce(sum((length(m.content) - length(replace(m.content, ?4, ''))) / length(?4)), 0)
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE `+in("m.created_at")+` AND (?3 = 0 OR s.project_id = ?3) AND instr(m.content, ?4) > 0`,
		p.Since, p.Until, projectID, placeholder).Scan(&c.Redactions); err != nil {
		return c, fmt.Errorf("count redactions: %w", err)
	}
	return c, nil
}

func zeroes(keys []string) map[string]int {
	m := make(map[string]int, len(keys))
	for _, k := range keys {
		m[k] = 0
	}
	return m
}

// groupInto runs a "key, count" query into m and adds the counts to total.
func groupInto(db *sql.DB, m map[string]int, total *int, query string, args ...any) error {
	rows, err := db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		m[key] += n
		*total += n
	}
	return rows.Err()
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// delta formats the change from before to now: "+12", "-3", or "=" for
// none, so an unchanged figure is visibly unchanged rather than "+0".
func delta(now, before int) string {
	switch d := now - before; {
	case d > 0:
		return fmt.Sprintf("+%d", d)
	case d < 0:
		return fmt.Sprintf("%d", d)
	}
	return "="
}

func ordered(keys []string, m map[string]int) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// byCount lists a map's entries, largest first, for keys with no fixed order
// (agent names).
func byCount(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	var list []kv
	for k, n := range m {
		list = append(list, kv{k, n})
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && (list[j].n > list[j-1].n || list[j].n == list[j-1].n && list[j].k < list[j-1].k); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	parts := make([]string, len(list))
	for i, e := range list {
		parts[i] = fmt.Sprintf("%s %d", e.k, e.n)
	}
	return strings.Join(parts, ", ")
}

// Format renders the report as text. Each line is a figure, its change from
// the previous period, and its breakdown.
func Format(r Report) string {
	var b strings.Builder
	scope := r.Project
	if scope == "" {
		scope = "all projects"
	}
	fmt.Fprintf(&b, "kae report: %s, %s to %s\n", scope, day(r.Period.Since), day(r.Period.Until))
	fmt.Fprintf(&b, "Change is against the period before it (%s to %s). Every figure is a count of what kae recorded.\n\n",
		day(r.Previous.Since), day(r.Previous.Until))

	line := func(label string, now, before int, detail string) {
		fmt.Fprintf(&b, "%-20s %6d  %-7s %s\n", label, now, "("+delta(now, before)+")", detail)
	}
	n, p := r.Now, r.Before
	line("searches run", n.Searches, p.Searches,
		fmt.Sprintf("%s; %d returned nothing", ordered(surfaces[:2], n.SearchesBySurface), n.SearchesEmpty))
	line("results opened", n.ResultsOpened, p.ResultsOpened, "")
	line("results labelled", n.LabelsTotal, p.LabelsTotal, ordered(labels, n.Labels))
	line("search verdicts", n.VerdictsTotal, p.VerdictsTotal, ordered(verdicts, n.Verdicts))
	line("handoffs saved", n.HandoffsSaved, p.HandoffsSaved,
		fmt.Sprintf("%d picked up within %d days", n.HandoffsPickedUp, doctor.HandoffPickupDays))
	line("sessions ingested", n.SessionsIngested, p.SessionsIngested, byCount(n.SessionsByAgent))
	line("secrets redacted", n.Redactions, p.Redactions, "")
	return b.String()
}
