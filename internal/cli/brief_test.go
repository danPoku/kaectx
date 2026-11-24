// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/agentcfg"
	"github.com/danPoku/kaectx/internal/brief"
	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/store"
)

// briefDB makes a database with one project at dir, one session in it, and a
// handoff note that a checkpoint recorded two days ago.
func briefDB(t *testing.T, dir string) (*sql.DB, int64, int64) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	projectID, err := store.ResolveProject(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) int64 {
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	mustExec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at, ended_at, title, message_count)
		VALUES ('claude-code:s1', 'claude-code', 's1', ?, strftime('%Y-%m-%dT%H:%M:%SZ','now','-2 days'), strftime('%Y-%m-%dT%H:%M:%SZ','now','-2 days'), 'Move billing to the new ledger', 420)`, projectID)
	note := mustExec(`INSERT INTO notes(project_id, agent, kind, title, body, tags)
		VALUES (?, 'claude-code', 'todo', 'Handoff: ledger migration', 'Half the accounts are moved. Next: run the reconciliation.', '["handoff"]')`, projectID)
	mustExec(`INSERT INTO checkpoint_observations(session_id, project_id, level, message_count, duration_minutes, files_changed, outcome, note_id, resolved_at)
		VALUES ('claude-code:s1', ?, 'checkpoint_recommended', 420, 250, 4, 'saved_handoff', ?, strftime('%Y-%m-%dT%H:%M:%fZ','now','-2 days'))`, projectID, note)
	return db, projectID, note
}

// A handoff delivered in a brief counts as picked up in `kae doctor`, and a
// brief printed for a person to read does not.
func TestBriefDeliveryCountsAsHandoffPickup(t *testing.T) {
	dir := t.TempDir()
	db, projectID, _ := briefDB(t, dir)

	pickedUp := func() int {
		r, err := doctor.Collect(context.Background(), db, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if r.HandoffsSaved != 1 || r.HandoffsLinked != 1 {
			t.Fatalf("setup wrong: %d saved, %d linked", r.HandoffsSaved, r.HandoffsLinked)
		}
		return r.HandoffsPickedUp
	}
	if pickedUp() != 0 {
		t.Fatal("nothing has read the handoff yet")
	}

	// `kae brief` typed by a person: shown, not recorded.
	shown, err := briefFor(db, dir, brief.DefaultBudget, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown.Text, "Handoff: ledger migration") || !strings.Contains(shown.Text, "Move billing to the new ledger") {
		t.Fatalf("brief lacks the handoff or the session:\n%s", shown.Text)
	}
	if pickedUp() != 0 {
		t.Fatal("a brief read by a person was counted as a pickup")
	}

	// The same from a session-start hook: recorded.
	if _, err := briefFor(db, dir, brief.DefaultBudget, true); err != nil {
		t.Fatal(err)
	}
	if pickedUp() != 1 {
		t.Fatal("a handoff delivered by the hook was not counted as picked up")
	}
}

func TestBriefForUnknownFolderIsEmptyAndCreatesNothing(t *testing.T) {
	db, _, _ := briefDB(t, t.TempDir())
	var before int
	db.QueryRow(`SELECT count(*) FROM projects`).Scan(&before)

	r, err := briefFor(db, t.TempDir(), brief.DefaultBudget, true)
	if err != nil || r.Text != "" {
		t.Fatalf("an unknown folder should get an empty brief: %q, %v", r.Text, err)
	}
	var after int
	db.QueryRow(`SELECT count(*) FROM projects`).Scan(&after)
	if after != before {
		t.Fatal("asking for a brief created a project")
	}
}

// As a hook, the command must never fail or print an error, even when its
// flags are wrong: the agent's session would start with that error in it.
func TestBriefHookNeverFails(t *testing.T) {
	useHome(t)
	for _, args := range [][]string{
		{"--hook"},
		{"--hook", "--budget", "not-a-number"},
		{"--hook", "--no-such-flag"},
	} {
		out := captureStdout(t, func() error { return runBrief(args) })
		if out != "" {
			t.Errorf("%v: a project with no history should print nothing, got %q", args, out)
		}
	}
	// Without --hook a bad flag is an ordinary error.
	if err := runBrief([]string{"--no-such-flag"}); err == nil {
		t.Error("a bad flag without --hook should be an error")
	}
}

// Setup offers the brief to each connected agent, and installs it once.
func TestSetupInstallsTheBriefForBothAgents(t *testing.T) {
	home, m := newSetupMachine(t)
	exe := filepath.Join(home, "bin", "kae")
	opts := setupOptions{Home: home, Exe: exe, GOOS: "linux", Yes: true, NoDaemon: true, Run: m}

	out := runSetupWith(t, opts)
	for _, want := range []string{"claude-code: added a SessionStart hook", "codex: added an instruction"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if !agentcfg.HasClaudeBriefHook(home) || !agentcfg.HasCodexBriefInstruction(home) {
		t.Fatal("brief not installed for both agents")
	}
	settings, _ := os.ReadFile(agentcfg.ClaudeSettingsPath(home))
	if !strings.Contains(string(settings), "brief --hook") {
		t.Fatalf("settings.json has no brief hook:\n%s", settings)
	}

	out = runSetupWith(t, opts)
	if !strings.Contains(out, "nothing to change") {
		t.Errorf("second run should change nothing:\n%s", out)
	}
	again, _ := os.ReadFile(agentcfg.ClaudeSettingsPath(home))
	if string(again) != string(settings) {
		t.Error("second run rewrote settings.json")
	}
}

// Declining the brief leaves the agent's settings alone.
func TestSetupBriefIsOptional(t *testing.T) {
	home, m := newSetupMachine(t)
	// Answers: Codex approval no, Claude Code brief no, Codex brief no.
	runSetupWith(t, setupOptions{Home: home, Exe: "/opt/kae", GOOS: "linux", NoDaemon: true, Run: m, In: strings.NewReader("n\nn\nn\n")})
	if agentcfg.HasClaudeBriefHook(home) || agentcfg.HasCodexBriefInstruction(home) {
		t.Fatal("the brief was installed although it was declined")
	}
	if _, err := os.Stat(agentcfg.ClaudeSettingsPath(home)); err == nil {
		t.Fatal("settings.json was created although the hook was declined")
	}
}
