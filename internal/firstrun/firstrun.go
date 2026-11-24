// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package firstrun builds what kae shows after its first ingest: how much
// history it found, and one search to try.
//
// kae reads logs the agents have already written, so on the day it is
// installed it can know months of work. A tool that captures through hooks
// starts empty. The plain "ingested N file(s)" line hid that difference; this
// is the librarian's first report after cataloguing a house full of
// unsorted papers: how many, from whom, how far back, and here is one you
// may want to read.
package firstrun

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"

	"github.com/danPoku/kaectx/internal/search"
)

// Count is a name with a number of sessions.
type Count struct {
	Name     string
	Sessions int
}

// Overview is what the catalogue holds.
type Overview struct {
	Sessions int
	Projects int
	// ByAgent is every agent, most sessions first.
	ByAgent []Count
	// TopProjects is the five projects with most sessions.
	TopProjects []Count
	// Oldest and Newest are session start times (RFC 3339, UTC).
	Oldest, Newest string
	// Redactions is the number of places where a secret was replaced
	// before the text was stored.
	Redactions int
}

// CountSessions is how a caller tells an empty database from a used one
// before ingesting.
func CountSessions(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	return n, err
}

// Load reads the overview. It scans the whole messages table once to count
// redactions, which is acceptable because it runs once per database, after
// an ingest that has just read every one of those rows anyway.
func Load(db *sql.DB) (Overview, error) {
	var o Overview
	var oldest, newest sql.NullString
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT project_id), min(started_at), max(started_at) FROM sessions`).
		Scan(&o.Sessions, &o.Projects, &oldest, &newest); err != nil {
		return o, err
	}
	o.Oldest, o.Newest = oldest.String, newest.String

	var err error
	if o.ByAgent, err = counts(db, `SELECT agent, count(*) FROM sessions GROUP BY agent ORDER BY count(*) DESC, agent`); err != nil {
		return o, err
	}
	if o.TopProjects, err = counts(db, `SELECT p.name, count(*) FROM sessions s JOIN projects p ON p.id = s.project_id
		GROUP BY p.id ORDER BY count(*) DESC, p.name LIMIT 5`); err != nil {
		return o, err
	}
	// The placeholder is fixed text, so the number of redactions in a row is
	// the length it adds divided by its own length.
	const placeholder = "[REDACTED]"
	if err := db.QueryRow(`SELECT coalesce(sum((length(content) - length(replace(content, ?1, ''))) / length(?1)), 0)
		FROM messages WHERE instr(content, ?1) > 0`, placeholder).Scan(&o.Redactions); err != nil {
		return o, err
	}
	return o, nil
}

func counts(db *sql.DB, query string) ([]Count, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Name, &c.Sessions); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Suggestion is a search worth trying, with the best result it returns.
type Suggestion struct {
	Query string
	// AllProjects is true when the suggestion is drawn from the whole
	// catalogue because the current project has no sessions; the command
	// shown then needs --all-projects.
	AllProjects bool
	Hit         search.Result
}

// stopwords are words too common to make a search specific. The list is
// short on purpose: it only has to keep "the" and "with" from using up the
// few words a suggestion gets.
var stopwords = map[string]bool{
	"about": true, "after": true, "again": true, "also": true, "been": true, "before": true,
	"can": true, "could": true, "does": true, "from": true, "have": true, "here": true,
	"how": true, "into": true, "just": true, "like": true, "make": true, "need": true,
	"please": true, "should": true, "some": true, "that": true, "the": true, "then": true,
	"there": true, "these": true, "this": true, "what": true, "when": true, "where": true,
	"which": true, "will": true, "with": true, "would": true, "your": true, "and": true,
	"for": true, "not": true, "you": true, "are": true, "why": true,
}

// queryFromTitle picks up to three distinctive words from a session title.
// Search requires every word to match, so a short query finds the exchange
// the title came from, where a whole sentence often finds nothing.
func queryFromTitle(title string) string {
	words := strings.FieldsFunc(title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	var picked []string
	seen := map[string]bool{}
	for _, w := range words {
		lower := strings.ToLower(w)
		// REDACTED is what is left of a secret in a title, not a topic.
		if len(w) < 3 || stopwords[lower] || seen[lower] || lower == "redacted" {
			continue
		}
		seen[lower] = true
		picked = append(picked, lower)
		if len(picked) == 3 {
			break
		}
	}
	return strings.Join(picked, " ")
}

// candidates lists the searches worth trying for a title's words, most
// specific first: all of them, then each pair, then each word alone.
//
// A title is not always text from the conversation. Claude Code writes its
// own ("WAL mode question"), and a word like "question" may appear nowhere
// in the exchange, so the full query can find nothing while "wal mode" finds
// exactly the right one.
func candidates(query string) []string {
	w := strings.Fields(query)
	switch len(w) {
	case 0:
		return nil
	case 1:
		return w
	case 2:
		return []string{query, w[0], w[1]}
	}
	return []string{query, w[0] + " " + w[1], w[1] + " " + w[2], w[0] + " " + w[2], w[0], w[1], w[2]}
}

// Suggest builds a search from the most recent titled session of project
// projectID and runs it. It only returns a suggestion that finds something:
// advising a new user to run a search that prints "no results" would be the
// worst possible first impression. If the project has nothing usable, it
// looks across every project. ok is false when no suggestion could be made.
func Suggest(ctx context.Context, db *sql.DB, projectID int64) (Suggestion, bool, error) {
	for _, all := range []bool{false, true} {
		query := `SELECT title FROM sessions WHERE project_id = ? AND title IS NOT NULL AND title != ''
			AND parent_session_id IS NULL ORDER BY started_at DESC LIMIT 8`
		args := []any{projectID}
		if all {
			query = `SELECT title FROM sessions WHERE title IS NOT NULL AND title != ''
				AND parent_session_id IS NULL ORDER BY started_at DESC LIMIT 8`
			args = nil
		}
		titles, err := titleList(db, query, args...)
		if err != nil {
			return Suggestion{}, false, err
		}
		scope := search.Scope{ProjectID: projectID, AllProjects: all}
		for _, title := range titles {
			for _, q := range candidates(queryFromTitle(title)) {
				// Keyword-only (no embedding client): the suggestion must
				// not wait on Ollama, and must work on a machine without it.
				got, err := search.BestScoped(ctx, db, nil, q, scope, "", search.Window{}, 1)
				if err != nil {
					return Suggestion{}, false, err
				}
				if len(got.Results) > 0 {
					return Suggestion{Query: q, AllProjects: all, Hit: got.Results[0]}, true, nil
				}
			}
		}
	}
	return Suggestion{}, false, nil
}

func titleList(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Format renders the summary. s may be nil when no suggestion was found.
func Format(o Overview, s *Suggestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kae found %s across %s", plural(o.Sessions, "session", "sessions"), plural(o.Projects, "project", "projects"))
	if o.Oldest != "" {
		if day(o.Oldest) == day(o.Newest) {
			fmt.Fprintf(&b, ", from %s", day(o.Oldest))
		} else {
			fmt.Fprintf(&b, ", from %s to %s", day(o.Oldest), day(o.Newest))
		}
	}
	b.WriteString(".\n")
	b.WriteString("This history was already on your disk: kae read the logs your agents had written.\n")

	width := 0
	for _, c := range append(append([]Count{}, o.ByAgent...), o.TopProjects...) {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	b.WriteString("\nBy agent:\n")
	for _, c := range o.ByAgent {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Name, plural(c.Sessions, "session", "sessions"))
	}
	if len(o.TopProjects) > 0 {
		b.WriteString("\nMost active projects:\n")
		for _, c := range o.TopProjects {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Name, plural(c.Sessions, "session", "sessions"))
		}
	}
	fmt.Fprintf(&b, "\nSecrets redacted before storing: %d\n", o.Redactions)

	if s != nil {
		flag := ""
		if s.AllProjects {
			flag = " --all-projects"
		}
		fmt.Fprintf(&b, "\nTry:\n\n  kae search %q%s\n\nIts top result:\n\n  [%s] %s  %s\n    %s\n",
			s.Query, flag, s.Hit.Agent, s.Hit.At, s.Hit.SessionID, strings.TrimSpace(s.Hit.Snippet))
	}
	return b.String()
}
