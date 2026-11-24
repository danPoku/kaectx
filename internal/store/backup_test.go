// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var backupNow = time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)

// olderDatabase builds a migrated database, then winds its schema back one
// migration: the newest migration's row and table are removed, which is the
// state a database is in when a newer binary first opens it. It holds one
// project so the backup's contents can be recognised.
func olderDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kae.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO projects(name) VALUES ('before-upgrade')`,
		`DELETE FROM schema_migrations WHERE version = 20`,
		`DROP TABLE forgotten_sessions`,
		`ALTER TABLE projects DROP COLUMN excluded_at`,
		`ALTER TABLE sources DROP COLUMN skipped_excluded`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return db, path
}

// LatestMigration is written by hand; this fails when a migration is added
// without raising it, which would silently stop backups being taken.
func TestLatestMigration(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var max int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&max); err != nil {
		t.Fatal(err)
	}
	if max != LatestMigration {
		t.Fatalf("the newest migration is %d but LatestMigration is %d; update the constant", max, LatestMigration)
	}
}

// An older database opened by a newer binary gets a backup first, and that
// backup is the database as the older binary left it.
func TestBackupBeforeAPendingMigration(t *testing.T) {
	db, _ := olderDatabase(t)
	dir := filepath.Join(t.TempDir(), "backups")

	path, err := BackupIfPending(db, dir, backupNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "kae-s19-20261124T093000Z.db"); path != want {
		t.Fatalf("backup path %q, want %q", path, want)
	}
	// Then the migration runs, as openStore does next.
	if _, err := Migrate(db); err != nil {
		t.Fatal(err)
	}

	// The backup opens on its own and is at the old schema: it has the data,
	// it lacks the new migration, and so the older binary can use it.
	backup, err := Open(path)
	if err != nil {
		t.Fatalf("the backup does not open: %v", err)
	}
	defer backup.Close()
	var projects, version, newTable int
	if err := backup.QueryRow(`SELECT count(*) FROM projects WHERE name = 'before-upgrade'`).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	backup.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version)
	backup.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'forgotten_sessions'`).Scan(&newTable)
	if projects != 1 || version != 19 || newTable != 0 {
		t.Fatalf("backup has %d project(s), schema %d, new table present=%d; want 1, 19, 0", projects, version, newTable)
	}
	var check string
	if err := backup.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("backup integrity: %q, %v", check, err)
	}

	// The live database did migrate, and now nothing is pending.
	var live int
	db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&live)
	if live != LatestMigration {
		t.Fatalf("live database is at %d after Migrate", live)
	}
	again, err := BackupIfPending(db, dir, backupNow.Add(time.Hour))
	if err != nil || again != "" {
		t.Fatalf("a second backup was taken with nothing pending: %q, %v", again, err)
	}
}

func TestNoBackupWhenNothingIsPending(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")

	// A file that was only just created: no schema, nothing to lose.
	fresh, err := Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if path, err := BackupIfPending(fresh, dir, backupNow); err != nil || path != "" {
		t.Fatalf("a new database was backed up: %q, %v", path, err)
	}
	// An up-to-date database.
	if _, err := Migrate(fresh); err != nil {
		t.Fatal(err)
	}
	if path, err := BackupIfPending(fresh, dir, backupNow); err != nil || path != "" {
		t.Fatalf("an up-to-date database was backed up: %q, %v", path, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the backups folder was created although no backup was taken")
	}
}

func TestOnlyThreeBackupsAreKept(t *testing.T) {
	db, _ := olderDatabase(t)
	dir := filepath.Join(t.TempDir(), "backups")
	// A file of the user's own in the folder is never deleted or listed.
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "my-own-copy.db")
	if err := os.WriteFile(keep, []byte("mine"), 0600); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for i := range 5 {
		path, err := BackupIfPending(db, dir, backupNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	backups, err := ListBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != BackupsKept {
		t.Fatalf("%d backups kept, want %d", len(backups), BackupsKept)
	}
	// Newest first, and they are the three newest.
	for i, b := range backups {
		if want := paths[len(paths)-1-i]; b.Path != want {
			t.Errorf("backup %d is %s, want %s", i, filepath.Base(b.Path), filepath.Base(want))
		}
		if b.SchemaVersion != 19 || b.Size == 0 {
			t.Errorf("backup %d: schema %d, size %d", i, b.SchemaVersion, b.Size)
		}
	}
	for _, old := range paths[:2] {
		if _, err := os.Stat(old); err == nil {
			t.Errorf("old backup %s was not removed", filepath.Base(old))
		}
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
		t.Fatal("a file that is not a kae backup was touched")
	}
}

// Two processes starting in the same second pick the same name; the second
// must accept the first's backup instead of failing.
func TestSameSecondBackupIsNotAnError(t *testing.T) {
	db, _ := olderDatabase(t)
	dir := filepath.Join(t.TempDir(), "backups")
	first, err := BackupIfPending(db, dir, backupNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BackupIfPending(db, dir, backupNow)
	if err != nil || second != first {
		t.Fatalf("second backup in the same second: %q, %v", second, err)
	}
}

func TestBackupFailureIsReported(t *testing.T) {
	db, _ := olderDatabase(t)
	// A regular file where the backups folder should be.
	blocker := filepath.Join(t.TempDir(), "backups")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupIfPending(db, blocker, backupNow); err == nil || !strings.Contains(err.Error(), "backups") {
		t.Fatalf("want an error naming the folder, got %v", err)
	}
}

func TestListBackupsOfAMissingFolder(t *testing.T) {
	got, err := ListBackups(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}
