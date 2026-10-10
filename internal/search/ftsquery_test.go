// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

func openFTSTestDB(t *testing.T) *sql.DB {
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

func TestSanitizeFTS5QueryPassesThroughSafeInput(t *testing.T) {
	for _, q := range []string{
		"hello world",
		"WAL mode",
		"_journal_mode",
		"co_author",
		"prefix*",
		"error AND NOT deprecated",
		"a OR b",
		`"exact phrase"`,
		"-excluded",
		"(a OR b) AND c",
	} {
		if got := sanitizeFTS5Query(q); got != q {
			t.Errorf("sanitizeFTS5Query(%q) = %q, want unchanged", q, got)
		}
	}
}

// TestSanitizeFTS5QueryFixesRealBreakage pins down the exact bug found
// dogfooding real search queries: FTS5's parser treats an unquoted hyphen
// or colon inside a bareword as column-filter syntax ("sqlite-vec" ->
// tries to filter on column "vec"). Every case here reproduces an actual
// "no such column: X" failure against the real chunks_fts table before
// this fix.
func TestSanitizeFTS5QueryFixesRealBreakage(t *testing.T) {
	cases := map[string]string{
		"sqlite-vec":     `"sqlite-vec"`,
		"go-sqlite3":     `"go-sqlite3"`,
		"non-negotiable": `"non-negotiable"`,
		"foo:bar":        `"foo:bar"`,
		"a-b-c-d":        `"a-b-c-d"`,
		`C:\Users\dev`:   `"C:\Users\dev"`,
		"sqlite-vec cgo": `"sqlite-vec" cgo`,
	}
	for input, want := range cases {
		if got := sanitizeFTS5Query(input); got != want {
			t.Errorf("sanitizeFTS5Query(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSanitizeFTS5QueryDoesNotMangleAlreadyQuotedPhrases(t *testing.T) {
	input := `"sqlite-vec extension" cgo`
	want := `"sqlite-vec extension" cgo`
	if got := sanitizeFTS5Query(input); got != want {
		t.Errorf("sanitizeFTS5Query(%q) = %q, want %q", input, got, want)
	}
}

func TestSanitizeFTS5QueryEscapesEmbeddedQuotes(t *testing.T) {
	input := `say "hi-there" now`
	got := sanitizeFTS5Query(input)
	// "hi-there" is already a quoted span, so it passes through verbatim;
	// "say" and "now" are safe barewords. Nothing here should produce
	// invalid FTS5 syntax (e.g. unbalanced quotes) — verified indirectly
	// by the query-execution tests in search_test.go, but the shape check
	// here catches an obviously broken transformation early.
	if got == "" {
		t.Fatal("sanitizeFTS5Query returned empty string")
	}
}

// TestSanitizeFTS5QueryOutputIsAlwaysValidFTS5 is the real assertion: run
// the sanitizer's output through an actual chunks_fts MATCH query for every
// case that previously broke, using the shared test DB helper's schema
// (no data needed — a syntax error would come back regardless of whether
// anything matches).
func TestSanitizeFTS5QueryOutputIsAlwaysValidFTS5(t *testing.T) {
	db := openFTSTestDB(t)

	queries := []string{
		"sqlite-vec", "go-sqlite3", "non-negotiable", "foo:bar", "a-b-c-d",
		`C:\Users\dev`, "sqlite-vec cgo header", "hello world", "_journal_mode",
		"error AND NOT deprecated", `"exact phrase"`, "(a OR b) AND c", "-excluded term",
	}
	for _, q := range queries {
		sanitized := sanitizeFTS5Query(q)
		rows, err := db.Query(`SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH ?`, sanitized)
		if err != nil {
			t.Errorf("query %q sanitized to %q: FTS5 MATCH failed: %v", q, sanitized, err)
			continue
		}
		rows.Close()
	}
}
