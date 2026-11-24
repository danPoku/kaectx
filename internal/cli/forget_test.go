// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

func TestForgetAsksFirstAndAcceptsAShortID(t *testing.T) {
	db, _ := handoffDB(t)

	// No answer: nothing is deleted.
	var out bytes.Buffer
	if err := forgetSession(&out, strings.NewReader(""), db, "claude-code:sess-1", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing was changed") {
		t.Fatalf("silence should change nothing:\n%s", out.String())
	}
	var sessions int
	db.QueryRow(`SELECT count(*) FROM sessions WHERE id = 'claude-code:sess-1'`).Scan(&sessions)
	if sessions != 1 {
		t.Fatal("the session was forgotten without a yes")
	}

	// "sess-1" alone names two sessions here (one per agent): refuse to guess.
	out.Reset()
	err := forgetSession(&out, strings.NewReader("y\n"), db, "sess-1", false)
	if err == nil || !strings.Contains(err.Error(), "more than one session") {
		t.Fatalf("an ambiguous id should be refused, got %v", err)
	}

	// An unambiguous prefix, confirmed.
	out.Reset()
	if err := forgetSession(&out, strings.NewReader("y\n"), db, "claude-code:sess", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "forgot claude-code:sess-1: removed") || !strings.Contains(out.String(), "will not be ingested again") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	if forgotten, _ := store.IsForgotten(db, "claude-code:sess-1"); !forgotten {
		t.Fatal("session not recorded as forgotten")
	}

	if err := forgetSession(&out, strings.NewReader("y\n"), db, "no-such-session", true); err == nil {
		t.Fatal("an unknown session should be an error")
	}
}

func TestProjectExcludeAndInclude(t *testing.T) {
	db, projectID := handoffDB(t)
	id := []string{"1"}
	if projectID != 1 {
		t.Fatalf("fixture project id is %d, the test assumes 1", projectID)
	}

	var out bytes.Buffer
	if err := projectExclude(&out, strings.NewReader(""), db, id, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `excluded "proj" (project 1)`) || !strings.Contains(out.String(), "add --forget to delete them") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	out.Reset()
	if err := projectExclude(&out, strings.NewReader(""), db, id, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "was already excluded") {
		t.Fatalf("second exclude should say so:\n%s", out.String())
	}

	out.Reset()
	if err := projectExclude(&out, strings.NewReader(""), db, id, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `included "proj"`) {
		t.Fatalf("unexpected output:\n%s", out.String())
	}

	// --forget asks before deleting, and deletes on yes.
	out.Reset()
	if err := projectExclude(&out, strings.NewReader("n\n"), db, []string{"1", "--forget"}, true); err != nil {
		t.Fatal(err)
	}
	var sessions int
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	if excluded, _ := store.ProjectExcluded(db, 1); excluded || sessions != 2 {
		t.Fatalf("declining --forget should change nothing: excluded=%v sessions=%d", excluded, sessions)
	}
	out.Reset()
	if err := projectExclude(&out, strings.NewReader("yes\n"), db, []string{"--forget", "1"}, true); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	if excluded, _ := store.ProjectExcluded(db, 1); !excluded || sessions != 0 {
		t.Fatalf("after --forget: excluded=%v sessions=%d\n%s", excluded, sessions, out.String())
	}

	for _, bad := range [][]string{{}, {"abc"}, {"999"}} {
		if err := projectExclude(&out, strings.NewReader(""), db, bad, true); err == nil {
			t.Errorf("args %v should be an error", bad)
		}
	}
	if err := projectExclude(&out, strings.NewReader(""), db, []string{"1", "--forget"}, false); err == nil {
		t.Error("--forget with include should be an error")
	}
}
