// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/datadir"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// useHome points the process's home directory at a fresh temp folder and
// forgets any data location resolved for an earlier test. Both variables are
// set because os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows.
func useHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetLoc := func() {
		locMu.Lock()
		locDone, cachedLoc, locErr = false, datadir.Location{}, nil
		locMu.Unlock()
	}
	resetLoc()
	t.Cleanup(resetLoc)
	return home
}

// hits runs the keyword search the CLI runs and returns what identifies each
// result, so two runs can be compared.
func hits(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	got, err := search.BestScoped(context.Background(), db, nil, query, search.Scope{AllProjects: true}, "", search.Window{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range got.Results {
		out = append(out, r.SessionID+"|"+r.Snippet)
	}
	return out
}

// Upgrading from a `ctx` install must keep the catalogue: the first kae
// command moves ~/.ctx to ~/.kae, and a search then returns exactly what it
// returned before, without ingesting anything again.
func TestUpgradeKeepsSearchResultsWithoutReingest(t *testing.T) {
	home := useHome(t)

	// Build the catalogue the way the old binary left it.
	oldDir := filepath.Join(home, datadir.LegacyDirName)
	if err := os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	old, err := store.Open(filepath.Join(oldDir, datadir.LegacyDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(old); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("..", "ingest", "claudecode", "testdata", "session.jsonl")
	if err := ingest.Run(old, claudecode.New(), fixture); err != nil {
		t.Fatal(err)
	}
	before := hits(t, old, "sqlite WAL")
	if len(before) == 0 {
		t.Fatal("fixture produced no search hits; the test would prove nothing")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, _, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	loc, err := dataLocation()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, datadir.DirName, datadir.DBName); loc.DB != want || !loc.Moved {
		t.Fatalf("data not moved to %s: %+v", want, loc)
	}
	if got := ingestLockPath(); filepath.Dir(got) != loc.Dir {
		t.Fatalf("ingest lock %s is not beside the database in %s", got, loc.Dir)
	}
	if after := hits(t, db, "sqlite WAL"); !reflect.DeepEqual(before, after) {
		t.Fatalf("search changed across the move:\nbefore %v\nafter  %v", before, after)
	}
}

func TestFreshMachineUsesKaeFolder(t *testing.T) {
	home := useHome(t)
	db, _, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(home, datadir.DirName, datadir.DBName)); err != nil {
		t.Fatalf("database not created under the new name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, datadir.LegacyDirName)); err == nil {
		t.Fatal("a fresh machine should never get a ~/.ctx folder")
	}
}

func TestDeprecatedAliasNotice(t *testing.T) {
	var buf bytes.Buffer
	WarnDeprecatedAlias(&buf)
	for _, want := range []string{"now called kae", "removed", "kae migrate-agents"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("notice %q does not mention %q", buf.String(), want)
		}
	}
}

// With no agent configured, migrate-agents says so and changes nothing.
func TestMigrateAgentsNothingToDo(t *testing.T) {
	home := useHome(t)
	var buf bytes.Buffer
	if err := migrateAgents(&buf, home, noClaude{}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing to change") {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

// A Codex-only machine: the config is rewritten, and a settings file that
// allowlists the old tool names is pointed out rather than edited.
func TestMigrateAgentsReportsChangesAndOldToolNames(t *testing.T) {
	home := useHome(t)
	codex := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte("[mcp_servers.ctx]\ncommand = \"ctx\"\nargs = [\"mcp\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"permissions": {"allow": ["mcp__ctx__search_context"]}}`
	if err := os.WriteFile(settings, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := migrateAgents(&buf, home, noClaude{}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"codex: renamed [mcp_servers.ctx] to [mcp_servers.kae]", "still names mcp__ctx__ tools", "start a new chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	got, _ := os.ReadFile(codex)
	if want := "[mcp_servers.kae]\ncommand = \"kae\"\nargs = [\"mcp\"]\n"; string(got) != want {
		t.Fatalf("codex config:\n%s\nwant:\n%s", got, want)
	}
	if kept, _ := os.ReadFile(settings); string(kept) != original {
		t.Fatal("the permission allowlist must be reported, not rewritten")
	}
}

// noClaude is a machine without the claude command.
type noClaude struct{}

func (noClaude) LookPath(string) (string, error)       { return "", os.ErrNotExist }
func (noClaude) Run(string, ...string) (string, error) { return "", os.ErrNotExist }
