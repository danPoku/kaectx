// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package audit re-checks what is already stored against the redaction rules.
// Redaction runs once, at ingest; this is the inspection after the fact, like
// a compliance officer sampling filed claims to confirm nothing sensitive
// slipped through, whether because a rule was added after a row was written
// or because a code path never redacted at all.
package audit

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/danPoku/kaectx/internal/redact"
)

// maxExamples caps how many example rows a Location keeps: enough to go
// look, few enough that a badly leaking column doesn't flood the report.
const maxExamples = 5

// target is one text column to inspect. query must yield (id, session, seq,
// text); session and seq are NULL where the table has no such notion.
type target struct {
	table, column, query string
}

var targets = []target{
	{"messages", "content", `SELECT id, session_id, seq, content FROM messages`},
	{"messages", "raw", `SELECT id, session_id, seq, raw FROM messages WHERE raw IS NOT NULL`},
	{"chunks", "text", `SELECT id, session_id, first_seq, text FROM chunks`},
	{"sessions", "title", `SELECT rowid, id, NULL, title FROM sessions WHERE title IS NOT NULL`},
	{"sessions", "summary", `SELECT rowid, id, NULL, summary FROM sessions WHERE summary IS NOT NULL`},
	{"notes", "title", `SELECT id, session_id, NULL, title FROM notes`},
	{"notes", "body", `SELECT id, session_id, NULL, body FROM notes`},
}

// Example points at one offending row. It carries no text: the report must
// not repeat the secret it is warning about.
type Example struct {
	ID        int64
	SessionID string
	Seq       *int64
	Rules     []string
}

// Location is the outcome for one table.column.
type Location struct {
	Table, Column string
	Scanned       int
	Hits          int            // rows with at least one residual match
	ByRule        map[string]int // rule name -> rows it matched
	Examples      []Example
}

// Report is the outcome of Secrets.
type Report struct {
	Locations []Location
}

// Hits is the total number of rows with a residual match, across locations.
func (r Report) Hits() int {
	n := 0
	for _, l := range r.Locations {
		n += l.Hits
	}
	return n
}

// Secrets scans every stored text column against the current redaction rules
// and reports rows that would still be changed by redact.Redact. It streams
// rows rather than loading them, since messages.raw is the bulk of the
// database.
func Secrets(db *sql.DB) (Report, error) {
	var rep Report
	for _, t := range targets {
		loc, err := scan(db, t)
		if err != nil {
			return rep, fmt.Errorf("audit %s.%s: %w", t.table, t.column, err)
		}
		rep.Locations = append(rep.Locations, loc)
	}
	return rep, nil
}

func scan(db *sql.DB, t target) (Location, error) {
	loc := Location{Table: t.table, Column: t.column, ByRule: map[string]int{}}
	rows, err := db.Query(t.query)
	if err != nil {
		return loc, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var session sql.NullString
		var seq sql.NullInt64
		var text string
		if err := rows.Scan(&id, &session, &seq, &text); err != nil {
			return loc, err
		}
		loc.Scanned++
		rules := redact.Scan(text)
		if len(rules) == 0 {
			continue
		}
		loc.Hits++
		for _, r := range rules {
			loc.ByRule[r]++
		}
		if len(loc.Examples) < maxExamples {
			ex := Example{ID: id, SessionID: session.String, Rules: rules}
			if seq.Valid {
				s := seq.Int64
				ex.Seq = &s
			}
			loc.Examples = append(loc.Examples, ex)
		}
	}
	return loc, rows.Err()
}

// RuleNames returns the location's matched rule names in a stable order.
func (l Location) RuleNames() []string {
	names := make([]string, 0, len(l.ByRule))
	for n := range l.ByRule {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
