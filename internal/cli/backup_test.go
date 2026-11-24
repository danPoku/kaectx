// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/datadir"
	"github.com/danPoku/kaectx/internal/store"
)

// olderCatalogue puts a database one migration behind into home's data
// folder, the state a newer binary finds after an upgrade.
func olderCatalogue(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, datadir.DirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, datadir.DBName)
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(db); err != nil {
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
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Any command that opens an older database backs it up before migrating it,
// says where, and does so once.
func TestOpenStoreBacksUpBeforeMigrating(t *testing.T) {
	home := useHome(t)
	dir := olderCatalogue(t, home)

	var db interface{ Close() error }
	notice := captureStderr(t, func() {
		opened, _, err := openStore()
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		db = opened
	})
	db.Close()
	if !strings.Contains(notice, "updating the database schema") || !strings.Contains(notice, filepath.Join(dir, "backups")) {
		t.Errorf("the user was not told about the backup: %q", notice)
	}
	backups, err := store.ListBackups(filepath.Join(dir, "backups"))
	if err != nil || len(backups) != 1 || backups[0].SchemaVersion != 19 {
		t.Fatalf("want one backup at schema 19, got %+v, %v", backups, err)
	}

	// The next command finds nothing pending: no new backup, no notice.
	notice = captureStderr(t, func() {
		opened, _, err := openStore()
		if err != nil {
			t.Fatalf("second openStore: %v", err)
		}
		opened.Close()
	})
	if notice != "" {
		t.Errorf("an up-to-date database printed %q", notice)
	}
	if again, _ := store.ListBackups(filepath.Join(dir, "backups")); len(again) != 1 {
		t.Errorf("%d backups after a second open, want 1", len(again))
	}
}

// If the backup cannot be written the schema is left alone, and the error
// says how to proceed. KAE_NO_BACKUP=1 is the deliberate way past.
func TestOpenStoreRefusesToMigrateWithoutABackup(t *testing.T) {
	home := useHome(t)
	dir := olderCatalogue(t, home)
	// A file where the backups folder should be.
	if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	_, _, err := openStore()
	if err == nil || !strings.Contains(err.Error(), "KAE_NO_BACKUP=1") {
		t.Fatalf("want a refusal that names the way past, got %v", err)
	}
	check, err := store.Open(filepath.Join(dir, datadir.DBName))
	if err != nil {
		t.Fatal(err)
	}
	var version int
	check.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version)
	check.Close()
	if version != 19 {
		t.Fatalf("the schema was updated to %d although the backup failed", version)
	}

	t.Setenv("KAE_NO_BACKUP", "1")
	db, _, err := openStore()
	if err != nil {
		t.Fatalf("with KAE_NO_BACKUP=1: %v", err)
	}
	defer db.Close()
	db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version)
	if version != store.LatestMigration {
		t.Fatalf("schema at %d, want %d", version, store.LatestMigration)
	}
}

func TestFormatBackups(t *testing.T) {
	if out := formatBackups(nil); !strings.Contains(out, "backups (taken before a schema update; the newest 3 are kept)\n  none:") {
		t.Errorf("empty list:\n%s", out)
	}
	out := formatBackups([]store.Backup{
		{Path: filepath.Join("data", "backups", "kae-s19-20261124T093000Z.db"), SchemaVersion: 19, At: time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC), Size: 432013312},
	})
	for _, want := range []string{"  kae-s19-20261124T093000Z.db  schema 19  2026-11-24 09:30  412.0 MB\n", "with the older binary"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
}
