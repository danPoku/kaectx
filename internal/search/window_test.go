// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/danPoku/kaectx/internal/search"
)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		name         string
		since, until string
		want         search.Window
		wantErr      bool
	}{
		{name: "no bounds", want: search.Window{}},
		{
			name: "dates cover whole days, until inclusive of its day",
			since: "2026-09-28", until: "2026-10-04",
			want: search.Window{Since: "2026-09-28T00:00:00.000Z", Until: "2026-10-05T00:00:00.000Z"},
		},
		{
			name: "timestamps are exact and converted to UTC",
			since: "2026-09-28T09:00:00+02:00", until: "2026-09-28T18:30:00Z",
			want: search.Window{Since: "2026-09-28T07:00:00.000Z", Until: "2026-09-28T18:30:00.000Z"},
		},
		{
			name: "sub-millisecond bounds round up, never admitting an earlier turn",
			since: "2026-09-28T09:00:00.0005Z", until: "2026-09-28T10:00:00.0005Z",
			want: search.Window{Since: "2026-09-28T09:00:00.001Z", Until: "2026-09-28T10:00:00.001Z"},
		},
		{name: "open-ended since", since: "2026-09-28", want: search.Window{Since: "2026-09-28T00:00:00.000Z"}},
		{name: "same day for both bounds is that one day", since: "2026-10-01", until: "2026-10-01",
			want: search.Window{Since: "2026-10-01T00:00:00.000Z", Until: "2026-10-02T00:00:00.000Z"}},
		{name: "reversed bounds are rejected", since: "2026-10-04", until: "2026-09-28", wantErr: true},
		{name: "garbage is rejected", since: "last week", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := search.ParseWindow(c.since, c.until)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseWindow = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWindow: %v", err)
			}
			if got != c.want {
				t.Errorf("ParseWindow = %+v, want %+v", got, c.want)
			}
		})
	}
}

// seedWeek puts the same "banana" exchange in three sessions: one long
// before the week asked about, one inside it, and one just after it. Each
// turn's time is its first message, as in real data.
func seedWeek(t *testing.T, db *sql.DB, p int64) {
	t.Helper()
	// Inserted out of time order on purpose: chunk ids (insertion order)
	// must not stand in for recency, because `ctx repair messages`
	// re-inserts old sessions' chunks with the newest ids.
	for _, s := range []struct{ id, agent, at string }{
		{"s-week", "codex", "2026-09-29T10:00:00.000Z"},
		{"s-after", "claude-code", "2026-10-06T10:00:00.000Z"},
		{"s-march", "claude-code", "2026-03-10T10:00:00.000Z"},
	} {
		seedChunkWithVector(t, db, p, s.agent, s.id, s.at, "banana bread recipe", vecAtDistance(0.05))
		if _, err := db.Exec(`INSERT INTO messages(session_id, seq, role, kind, content, created_at) VALUES (?, 0, 'user', 'text', 'x', ?)`, s.id, s.at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchWindowKeepsOnlyTurnsInsideIt(t *testing.T) {
	db := newVecDB(t)
	p := newProject(t, db)
	seedWeek(t, db, p)
	w, err := search.ParseWindow("2026-09-28", "2026-10-04")
	if err != nil {
		t.Fatal(err)
	}

	sessions := func(ids ...string) []string { return ids }
	check := func(name string, got []string) {
		t.Helper()
		if len(got) != 1 || got[0] != "s-week" {
			t.Errorf("%s returned sessions %v, want only s-week", name, got)
		}
	}

	kw, err := search.KeywordIn(db, "banana", p, "", w, 10)
	if err != nil {
		t.Fatalf("KeywordIn: %v", err)
	}
	var got []string
	for _, r := range kw {
		got = append(got, r.SessionID)
	}
	check("KeywordIn", got)

	// The vector half matches every chunk equally (same distance), so this
	// proves the window is applied to the semantic candidates too, not just
	// the keyword ones.
	for _, agent := range []string{"", "codex"} {
		hy, err := search.HybridIn(context.Background(), db, axisEmbedder{}, "banana", p, agent, w, 10)
		if err != nil {
			t.Fatalf("HybridIn(agent=%q): %v", agent, err)
		}
		got = sessions()
		for _, r := range hy {
			got = append(got, r.SessionID)
		}
		check("HybridIn agent="+agent, got)
	}
}

func TestEqualMatchesRankNewestFirst(t *testing.T) {
	db := newVecDB(t)
	p := newProject(t, db)
	seedWeek(t, db, p)
	want := []string{"s-after", "s-week", "s-march"}

	kw, err := search.Keyword(db, "banana", p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	hy, err := search.Hybrid(context.Background(), db, axisEmbedder{}, "banana", p, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]string{
		"Keyword": func() (s []string) {
			for _, r := range kw {
				s = append(s, r.SessionID)
			}
			return
		}(),
		"Hybrid": func() (s []string) {
			for _, r := range hy {
				s = append(s, r.SessionID)
			}
			return
		}(),
	} {
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s order = %v, want %v (identical matches, newest first)", name, got, want)
				break
			}
		}
	}
}

// Agents log some timestamps with milliseconds and some without. As text,
// '...:05Z' sorts after '...:05.500Z' though it is earlier, so the window
// must compare instants, not strings.
func TestSearchWindowComparesInstantsNotText(t *testing.T) {
	db := newVecDB(t)
	p := newProject(t, db)
	seedChunkWithVector(t, db, p, "codex", "s-edge", "2026-09-28T09:00:05Z", "kiwi edge", nil)
	if _, err := db.Exec(`INSERT INTO messages(session_id, seq, role, kind, content, created_at) VALUES ('s-edge', 0, 'user', 'text', 'x', '2026-09-28T09:00:05Z')`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		since, until string
		want         int
	}{
		{since: "2026-09-28T09:00:05.500Z", want: 0}, // turn is half a second before since
		{since: "2026-09-28T09:00:05Z", want: 1},     // inclusive
		{until: "2026-09-28T09:00:05.500Z", want: 1}, // turn is before until
		{until: "2026-09-28T09:00:05Z", want: 0},     // exclusive
	} {
		w, err := search.ParseWindow(c.since, c.until)
		if err != nil {
			t.Fatal(err)
		}
		got, err := search.KeywordIn(db, "kiwi", p, "", w, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("since=%q until=%q: %d results, want %d", c.since, c.until, len(got), c.want)
		}
	}
}
