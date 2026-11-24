// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package datadir decides where kae keeps its database, and moves a
// catalogue left by the tool's old name (`ctx`, folder ~/.ctx) to the new
// one (~/.kae) the first time it is safe to.
//
// Think of a shop that changes its name and its premises. The stock is moved
// only when nobody is serving a customer from the old shelves, and a note is
// left on the old door saying where everything went. Until the move can
// happen the shop keeps trading from the old address, so no customer is
// turned away and nothing is sold from two places at once.
package datadir

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3" // the "sqlite3" driver the hold check opens the old database with
)

const (
	// DirName and DBName are the folder under the home directory and the
	// database file inside it.
	DirName = ".kae"
	DBName  = "kae.db"

	// LegacyDirName and LegacyDBName are what they were called while the
	// command was `ctx`.
	LegacyDirName = ".ctx"
	LegacyDBName  = "ctx.db"

	// PointerName is the note left in the old folder after a move.
	PointerName = "MOVED-TO-KAE.txt"

	lockName = "ingest.lock"
)

// Location is where this process should keep its data.
type Location struct {
	// Dir is the data folder, DB the database file and Lock the ingest lock
	// file inside it.
	Dir, DB, Lock string
	// Legacy is true when the old ~/.ctx folder is still in use because it
	// could not be moved yet. Notice then says why.
	Legacy bool
	// Moved is true when this call moved the old folder to the new name.
	Moved bool
	// Notice is a one-line message for the user: where the data moved to, or
	// why it has not moved. Empty when there is nothing to say.
	Notice string
}

func at(dir, db string) Location {
	return Location{Dir: dir, DB: filepath.Join(dir, db), Lock: filepath.Join(dir, lockName)}
}

// Resolve returns the data location under home, moving a legacy catalogue
// first when that is safe. It creates nothing itself except as part of a
// move; the caller creates and secures Dir.
//
// The rules, in order:
//   - ~/.kae exists: use it. A ~/.ctx beside it is left alone.
//   - only ~/.ctx exists, with a database: move it, unless another process
//     has that database open, in which case keep using ~/.ctx for this run.
//   - otherwise: ~/.kae, which the caller will create.
func Resolve(home string) (Location, error) {
	newDir := filepath.Join(home, DirName)
	oldDir := filepath.Join(home, LegacyDirName)
	current := at(newDir, DBName)

	if exists(newDir) {
		return current, nil
	}
	oldDB := filepath.Join(oldDir, LegacyDBName)
	if !exists(oldDB) {
		return current, nil
	}

	if err := move(oldDir, newDir); err != nil {
		// Another kae process may have finished the same move between our
		// check and our attempt; if so the new folder is simply there now.
		if exists(newDir) {
			return current, nil
		}
		var held *HeldError
		if !errors.As(err, &held) {
			return Location{}, err
		}
		legacy := at(oldDir, LegacyDBName)
		legacy.Legacy = true
		legacy.Notice = fmt.Sprintf("data is still in %s: %v. Close the agents and stop any daemon using it, then run any kae command to move it to %s.", oldDir, held, newDir)
		return legacy, nil
	}

	current.Moved = true
	current.Notice = fmt.Sprintf("moved %s to %s (the database is now %s)", oldDir, newDir, DBName)
	return current, nil
}

// HeldError says the old database could not be moved because something still
// has it open.
type HeldError struct{ Reason string }

func (e *HeldError) Error() string { return e.Reason }

// move renames oldDir to newDir and its database files to the new name, then
// leaves a pointer file in a fresh oldDir. It refuses, with a *HeldError,
// while another process holds the old database.
func move(oldDir, newDir string) error {
	if err := checkNotHeld(filepath.Join(oldDir, LegacyDBName)); err != nil {
		return err
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		// On Windows a folder cannot be renamed while any file in it is
		// open, so this is the same condition reported by the OS.
		return &HeldError{Reason: fmt.Sprintf("the folder could not be renamed (%v), which usually means another process has a file in it open", err)}
	}
	// The -wal and -shm files are normally gone after the hold check closes
	// its connection, but rename them when present: SQLite finds them by the
	// database's name, and a WAL left under the old name would be ignored.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from := filepath.Join(newDir, LegacyDBName+suffix)
		if !exists(from) {
			continue
		}
		if err := os.Rename(from, filepath.Join(newDir, DBName+suffix)); err != nil {
			return fmt.Errorf("rename %s: %w", from, err)
		}
	}
	// The pointer is a courtesy; failing to write it must not undo a move
	// that has already succeeded.
	if err := os.MkdirAll(oldDir, 0700); err == nil {
		note := fmt.Sprintf("ctx is now called kae. On %s the contents of this folder were moved to:\n\n  %s\n\nThe database is %s in that folder. Nothing was deleted.\nThis folder can be removed once no old `ctx` binary is in use.\n",
			time.Now().UTC().Format("2006-01-02"), newDir, DBName)
		_ = os.WriteFile(filepath.Join(oldDir, PointerName), []byte(note), 0600)
	}
	return nil
}

// checkNotHeld returns a *HeldError if another connection, in this or any
// other process, has the database at path open.
//
// It asks SQLite itself rather than looking for process names: a connection
// opened in exclusive locking mode needs the database file to itself, and
// every WAL-mode connection (which is what every ctx and kae process uses)
// keeps a shared lock on that file for as long as it is open. So the
// exclusive open fails with "database is locked" exactly when a daemon, an
// MCP server or a running command still has the file. This also catches
// binaries from before the rename, which know nothing about any lock file
// kae could invent now.
//
// When the check passes, closing its connection checkpoints the write-ahead
// log into the main file, so the folder is moved as one settled database.
func checkNotHeld(path string) error {
	db, err := sql.Open("sqlite3", path+"?_locking_mode=EXCLUSIVE&_busy_timeout=300")
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		return &HeldError{Reason: fmt.Sprintf("another process has %s open (%v)", filepath.Base(path), err)}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
