// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/store"
)

// End to end: a log with a line type kae has never seen is ingested, and
// `kae doctor` then names the type, the agent and its version, and the
// count, while leaving known bookkeeping types out.
func TestDoctorReportsUnknownLineTypes(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	projectID, err := store.ResolveProject(db, "/home/dev/proj")
	if err != nil {
		t.Fatal(err)
	}

	// Before any such line: the section says none, rather than being blank.
	report, err := doctor.Collect(context.Background(), db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if out := formatDoctor(report); !strings.Contains(out, "unknown log line types (all projects)\n  none:") {
		t.Fatalf("empty section should say none:\n%s", out)
	}

	fixture := filepath.Join("..", "ingest", "claudecode", "testdata", "unknown_type.jsonl")
	if err := ingest.Run(db, claudecode.New(), fixture); err != nil {
		t.Fatal(err)
	}
	report, err = doctor.Collect(context.Background(), db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	out := formatDoctor(report)
	for _, want := range []string{
		"claude-code 9.9.0  conversation-digest  2 lines in 1 file, 2026-11-20",
		"claude-code 9.9.0  content/document  1 line in 1 file, 2026-11-20",
		"These lines were skipped.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	for _, known := range []string{"file-history-snapshot", "  mode  "} {
		if strings.Contains(out, known) {
			t.Errorf("known bookkeeping type %q was reported:\n%s", known, out)
		}
	}
}

func TestFormatUnknownLinesWithoutAVersionAndAcrossDays(t *testing.T) {
	out := formatUnknownLines([]ingest.UnknownLines{
		{Agent: "codex", LineType: "response_item/function_call", Lines: 412, Files: 9, FirstSeen: "2026-11-20T08:00:00Z", LastSeen: "2026-11-23T19:00:00Z"},
	})
	want := "  codex (version not logged)  response_item/function_call  412 lines in 9 files, 2026-11-20 to 2026-11-23\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("got:\n%s\nwant it to start:\n%s", out, want)
	}
}
