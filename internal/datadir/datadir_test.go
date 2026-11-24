// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

// seedLegacy builds a ~/.ctx/ctx.db under home the way a pre-rename binary
// left it, holding one project row to recognise after the move.
func seedLegacy(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, LegacyDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, LegacyDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (name) VALUES ('carried-over')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func projectCount(t *testing.T, path string) int {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE name = 'carried-over'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolveFreshMachineUsesNewFolder(t *testing.T) {
	home := t.TempDir()
	loc, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Dir != filepath.Join(home, DirName) || loc.Legacy || loc.Moved || loc.Notice != "" {
		t.Fatalf("unexpected location: %+v", loc)
	}
	if exists(loc.Dir) {
		t.Fatal("Resolve should leave creating the folder to the caller")
	}
}

func TestResolveMovesLegacyFolderOnce(t *testing.T) {
	home := t.TempDir()
	seedLegacy(t, home)

	loc, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if !loc.Moved || loc.Legacy {
		t.Fatalf("want a move, got %+v", loc)
	}
	if got := projectCount(t, loc.DB); got != 1 {
		t.Fatalf("moved database lost its rows: %d", got)
	}
	if !exists(loc.Lock) {
		t.Fatal("the lock file should move with the folder")
	}
	oldDir := filepath.Join(home, LegacyDirName)
	note, err := os.ReadFile(filepath.Join(oldDir, PointerName))
	if err != nil {
		t.Fatalf("no pointer file left behind: %v", err)
	}
	if !strings.Contains(string(note), loc.Dir) {
		t.Fatalf("pointer does not name the new folder:\n%s", note)
	}
	if exists(filepath.Join(oldDir, LegacyDBName)) {
		t.Fatal("old database still present after the move")
	}

	// A second run finds the new folder and does nothing more, even though
	// the old folder (now holding only the pointer) still exists.
	again, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if again.Moved || again.Legacy || again.Notice != "" || again.DB != loc.DB {
		t.Fatalf("second run should be silent and unchanged: %+v", again)
	}
}

func TestResolveRefusesToMoveWhileDatabaseIsOpen(t *testing.T) {
	home := t.TempDir()
	seedLegacy(t, home)

	// Stand in for a running `ctx daemon` or `ctx mcp`: an open WAL
	// connection that has read from the database.
	oldDB := filepath.Join(home, LegacyDirName, LegacyDBName)
	holder, err := store.Open(oldDB)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := holder.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	loc, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if !loc.Legacy || loc.Moved || loc.DB != oldDB {
		t.Fatalf("want the legacy location while held, got %+v", loc)
	}
	if !strings.Contains(loc.Notice, "another process") && !strings.Contains(loc.Notice, "could not be renamed") {
		t.Fatalf("notice does not say why: %q", loc.Notice)
	}
	if exists(filepath.Join(home, DirName)) {
		t.Fatal("new folder created although the move was refused")
	}

	// Once the holder is gone the next run moves it.
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	loc, err = Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if !loc.Moved {
		t.Fatalf("want a move after the holder closed, got %+v", loc)
	}
	if got := projectCount(t, loc.DB); got != 1 {
		t.Fatalf("moved database lost its rows: %d", got)
	}
}

func TestResolvePrefersNewFolderWhenBothExist(t *testing.T) {
	home := t.TempDir()
	seedLegacy(t, home)
	if err := os.MkdirAll(filepath.Join(home, DirName), 0700); err != nil {
		t.Fatal(err)
	}
	loc, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Legacy || loc.Moved || loc.Dir != filepath.Join(home, DirName) {
		t.Fatalf("unexpected location: %+v", loc)
	}
	if !exists(filepath.Join(home, LegacyDirName, LegacyDBName)) {
		t.Fatal("the old database must be left alone when both folders exist")
	}
}

// The SQLite-level check is what protects Linux and macOS, where renaming a
// folder succeeds even while its files are open. Test it on its own so a
// platform where the rename happens to fail first cannot hide a broken check.
func TestCheckNotHeldSeesAnOpenConnection(t *testing.T) {
	home := t.TempDir()
	seedLegacy(t, home)
	oldDB := filepath.Join(home, LegacyDirName, LegacyDBName)

	if err := checkNotHeld(oldDB); err != nil {
		t.Fatalf("nothing holds the database, got %v", err)
	}
	holder, err := store.Open(oldDB)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := holder.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	err = checkNotHeld(oldDB)
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("want a HeldError while a connection is open, got %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkNotHeld(oldDB); err != nil {
		t.Fatalf("holder closed, got %v", err)
	}
}
