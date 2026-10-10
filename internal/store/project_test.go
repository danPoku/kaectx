// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os/exec"
	"testing"
)

func TestNormalizeGitRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:dan/forex-model.git":       "github.com/dan/forex-model",
		"https://github.com/Dan/Forex-Model.git":   "github.com/dan/forex-model",
		"https://github.com/dan/forex-model":       "github.com/dan/forex-model",
		"ssh://git@github.com/dan/forex-model.git": "github.com/dan/forex-model",
		"git@gitlab.internal.corp:team/repo.git":   "gitlab.internal.corp/team/repo",
	}
	for input, want := range cases {
		if got := normalizeGitRemote(input); got != want {
			t.Errorf("normalizeGitRemote(%q) = %q, want %q", input, got, want)
		}
	}
}

// migratedTestDB is openTestDB plus Migrate — project_paths/projects only
// exist after migration, and TestMigrateCreatesCoreSchema in store_test.go
// already covers the unmigrated case, so these tests need the full schema.
func migratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestResolveProjectCreatesAndReusesByPathPrefix(t *testing.T) {
	db := migratedTestDB(t)

	id1, err := ResolveProject(db, "/home/dev/repo")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}

	// A subdirectory of a known project root resolves to the same project.
	id2, err := ResolveProject(db, "/home/dev/repo/internal/store")
	if err != nil {
		t.Fatalf("ResolveProject (subdir): %v", err)
	}
	if id1 != id2 {
		t.Errorf("ResolveProject(subdir) = %d, want the same project id %d", id2, id1)
	}

	// An unrelated path creates a new project.
	id3, err := ResolveProject(db, "/home/dev/other-repo")
	if err != nil {
		t.Fatalf("ResolveProject (unrelated): %v", err)
	}
	if id3 == id1 {
		t.Errorf("ResolveProject(unrelated path) reused project %d, want a new one", id1)
	}
}

// TestResolveProjectDeduplicatesByGitRemote is the motivating scenario from
// CLAUDE.md: the same repo cloned at two different paths (WSL vs. a
// Codespace) must resolve to one project, not two. Uses real `git init` +
// `git remote add` in two temp directories rather than faking the git
// plumbing, since that plumbing (exec'ing `git remote get-url origin`) is
// exactly what's under test.
func TestResolveProjectDeduplicatesByGitRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := migratedTestDB(t)

	cloneA := initGitRepoWithRemote(t, "git@github.com:dan/forex-model.git")
	cloneB := initGitRepoWithRemote(t, "https://github.com/dan/forex-model.git")

	idA, err := ResolveProject(db, cloneA)
	if err != nil {
		t.Fatalf("ResolveProject(cloneA): %v", err)
	}
	idB, err := ResolveProject(db, cloneB)
	if err != nil {
		t.Fatalf("ResolveProject(cloneB): %v", err)
	}
	if idA != idB {
		t.Errorf("ResolveProject(cloneA)=%d, ResolveProject(cloneB)=%d, want the same project (same normalized remote)", idA, idB)
	}

	var projectCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projectCount); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projectCount != 1 {
		t.Errorf("projects = %d, want 1 (both clones share one project)", projectCount)
	}

	var pathCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_paths WHERE project_id = ?`, idA).Scan(&pathCount); err != nil {
		t.Fatalf("count project_paths: %v", err)
	}
	if pathCount != 2 {
		t.Errorf("project_paths for the shared project = %d, want 2 (one per clone)", pathCount)
	}
}

func TestLookupProjectNeverRegistersProjectsOrPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := migratedTestDB(t)
	known := initGitRepoWithRemote(t, "git@github.com:dan/known.git")
	projectID, err := ResolveProject(db, known)
	if err != nil {
		t.Fatal(err)
	}
	newCheckout := initGitRepoWithRemote(t, "https://github.com/dan/known.git")
	got, ok, err := LookupProject(db, newCheckout)
	if err != nil || !ok || got != projectID {
		t.Fatalf("LookupProject(known remote) = %d, %v, %v; want %d, true, nil", got, ok, err, projectID)
	}
	unknown := initGitRepoWithRemote(t, "https://github.com/dan/unknown.git")
	if got, ok, err := LookupProject(db, unknown); err != nil || ok || got != 0 {
		t.Fatalf("LookupProject(unknown) = %d, %v, %v; want 0, false, nil", got, ok, err)
	}
	var projects, paths int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_paths`).Scan(&paths); err != nil {
		t.Fatal(err)
	}
	if projects != 1 || paths != 1 {
		t.Errorf("lookup mutated catalogue: projects=%d paths=%d; want 1, 1", projects, paths)
	}
}

func initGitRepoWithRemote(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", remote)
	return dir
}
