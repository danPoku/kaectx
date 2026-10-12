// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/explain"
	"github.com/danPoku/kaectx/internal/repair"
)

func TestFormatDoctor(t *testing.T) {
	out := formatDoctor(doctor.Report{
		ProjectName: "ctx", Sources: 2, SourcesWithErrors: 1,
		LastIngestedAt: sql.NullString{String: "2026-09-30T12:00:00Z", Valid: true},
		Sessions:       4, Messages: 20, Chunks: 8, PendingEmbeddings: 3,
		UnassignedChunks: 1, RecentRetrievals: 10, RecentZeroResultRuns: 2,
		RecentHybridRuns: 6, RecentPolicyRuns: 1, Outcomes: 3,
		RecentOpenedResults: 5, RecentInferredOpens: 2, RecentLabelledOpens: 3,
		RetrievedEvidence: 4, ObservedEvidence: 2, CredibleEvidence: 1,
	})
	for _, want := range []string{"ctx doctor: ctx", "2 (1 with errors)", "4 / 20 / 8", "pending: 3", "10 (2 zero-result, 6 hybrid, 1 risk-aware)", "5 (3 exact, 2 inferred; 3 labelled, 2 awaiting outcome)", "2 / 4 evidence keys (50.0%)", "1 / 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestReorderFlagsFirst(t *testing.T) {
	flagsWithValue := map[string]bool{"agent": true, "limit": true, "risk-aware": false}

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "boolean flag after query does not consume a word",
			in:   []string{"some", "query", "--risk-aware"},
			want: []string{"--risk-aware", "some", "query"},
		},
		{
			name: "flags already first pass through unchanged",
			in:   []string{"--agent", "codex", "some", "query"},
			want: []string{"--agent", "codex", "some", "query"},
		},
		{
			name: "flags after the query get hoisted",
			in:   []string{"some", "query", "--agent", "codex"},
			want: []string{"--agent", "codex", "some", "query"},
		},
		{
			name: "flag interleaved mid-query",
			in:   []string{"some", "--limit", "5", "query"},
			want: []string{"--limit", "5", "some", "query"},
		},
		{
			name: "--flag=value form doesn't consume the next token",
			in:   []string{"some", "query", "--limit=5"},
			want: []string{"--limit=5", "some", "query"},
		},
		{
			name: "a query word that happens to start with a dash is left alone",
			in:   []string{"-foo", "bar"},
			want: []string{"-foo", "bar"},
		},
		{
			name: "bare words matching flag names remain query terms",
			in:   []string{"risk-aware", "agent", "limit"},
			want: []string{"risk-aware", "agent", "limit"},
		},
		{
			name: "no flags at all",
			in:   []string{"just", "a", "query"},
			want: []string{"just", "a", "query"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderFlagsFirst(c.in, flagsWithValue)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("reorderFlagsFirst(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestCommitRange(t *testing.T) {
	long1, long2 := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	cases := []struct{ name, start, end, want string }{
		{"neither", "", "", ""},
		{"unchanged", long1, long1, "111111111111"},
		{"span", long1, long2, "111111111111..222222222222"},
		{"only start", long1, "", "111111111111"},
		{"only end", "", long2, "222222222222"},
		{"short hashes are not truncated", "abc", "def", "abc..def"},
	}
	for _, c := range cases {
		if got := commitRange(c.start, c.end); got != c.want {
			t.Errorf("%s: commitRange(%q,%q) = %q, want %q", c.name, c.start, c.end, got, c.want)
		}
	}
}

func TestFormatRepair(t *testing.T) {
	r := repair.Result{Sessions: 4, CommitsUpdated: 3, CommitsUnresolved: 1, TouchesRewritten: 3, TouchesRooted: 6, TouchesPending: 1}

	real := formatRepair(r, false)
	for _, want := range []string{"examined 4 sessions", "updated 3 sessions", "1 could not be resolved", "updated 3 file paths", "6 rows now anchored", "1 rows skipped"} {
		if !strings.Contains(real, want) {
			t.Errorf("output missing %q:\n%s", want, real)
		}
	}
	if strings.Contains(real, "dry run") {
		t.Errorf("real run mentions dry run:\n%s", real)
	}

	dry := formatRepair(r, true)
	if !strings.Contains(dry, "would update 3 sessions") || !strings.Contains(dry, "nothing was written") {
		t.Errorf("dry-run output:\n%s", dry)
	}
	if strings.Contains(formatRepair(repair.Result{Sessions: 1}, false), "skipped") {
		t.Error("mentions skipped rows when there are none")
	}
}

func TestGitLineFlagsGuessedCommitSpans(t *testing.T) {
	base := explain.Session{GitBranch: "main", StartingCommit: "aaaaaaaaaaaaaaaa", EndingCommit: "bbbbbbbbbbbbbbbb"}
	for source, want := range map[string]string{
		"":           "",
		"branch":     "",
		"logged":     "",
		"head":       "approximate",
		"unverified": "unverified",
	} {
		s := base
		s.CommitSource = source
		line := gitLine(s)
		if !strings.HasPrefix(line, "  git: main aaaaaaaaaaaa..bbbbbbbbbbbb") {
			t.Errorf("%q: line = %q, want branch and span", source, line)
		}
		if want == "" && strings.Contains(line, "(") {
			t.Errorf("%q: line = %q, must not carry a caveat", source, line)
		}
		if want != "" && !strings.Contains(line, want) {
			t.Errorf("%q: line = %q, want it to say %q", source, line, want)
		}
	}
}

func TestFormatRepairMentionsUnverifiedAndTouches(t *testing.T) {
	out := formatRepair(repair.Result{Sessions: 2, CommitsUnresolved: 2, CommitsUnverified: 1}, false)
	if !strings.Contains(out, "1 of those still carry a commit from an older version and are now marked unverified") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(formatRepair(repair.Result{Sessions: 1}, false), "unverified") {
		t.Error("mentions unverified when there are none")
	}

	real := formatTouches(repair.TouchesResult{Messages: 5, Added: 8, Empty: 1, Pending: 1}, false)
	for _, want := range []string{"found 5 apply_patch calls", "added 8 file records", "1 calls named no files", "no longer exist"} {
		if !strings.Contains(real, want) {
			t.Errorf("touches output missing %q:\n%s", want, real)
		}
	}
	dry := formatTouches(repair.TouchesResult{Added: 3}, true)
	if !strings.Contains(dry, "would add 3") || !strings.Contains(dry, "nothing was written") {
		t.Errorf("dry output:\n%s", dry)
	}
}

func TestGitLineWithoutABranchKeepsItsIndentAndSpacing(t *testing.T) {
	line := gitLine(explain.Session{StartingCommit: "abc", EndingCommit: "def"})
	if line != "  git: abc..def" {
		t.Errorf("line = %q", line)
	}
}
