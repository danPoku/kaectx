// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// expectedObjects is the full set of tables/views 001_core.sql must produce,
// keyed by name with the sqlite_master type it should carry.
// v_session_overview is a VIEW, not a TABLE — checking each name against its
// specific expected type (rather than a blanket type='table' filter) is the
// point: a filter that only looked for tables would miss a silently-failed
// view creation entirely.
var expectedObjects = map[string]string{
	"schema_migrations":    "table",
	"projects":             "table",
	"project_paths":        "table",
	"sources":              "table",
	"sessions":             "table",
	"messages":             "table",
	"chunks":               "table",
	"chunks_fts":           "table", // fts5 virtual tables register as type=table
	"file_touches":         "table",
	"notes":                "table",
	"notes_fts":            "table",
	"retrieval_runs":       "table",
	"retrieval_candidates": "table",
	"v_session_overview":   "view",
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ctx.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateCreatesCoreSchema(t *testing.T) {
	db := openTestDB(t)
	result, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	rows, err := db.Query(`SELECT name, type FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for name, wantType := range expectedObjects {
		gotType, ok := got[name]
		if !ok {
			t.Errorf("missing schema object %q", name)
			continue
		}
		if gotType != wantType {
			t.Errorf("%q: type = %q, want %q", name, gotType, wantType)
		}
	}

	if result.VectorsLoaded {
		if gotType, ok := got["chunk_vectors"]; !ok || gotType != "table" {
			t.Errorf("chunk_vectors missing or wrong type (%q, ok=%v) despite VectorsLoaded=true", gotType, ok)
		}
	}
}

// TestPragmasAppliedToEveryConnection is the actual point of putting the
// pragmas on the DSN instead of running them once after Open: database/sql
// hands out whichever pooled connection is free, so a test that only ever
// touches one connection would pass even if the DSN's pragma params were
// silently ignored (mattn/go-sqlite3 does not error on an unrecognized
// _param). This forces several connections open concurrently and checks
// each one individually.
func TestPragmasAppliedToEveryConnection(t *testing.T) {
	db := openTestDB(t)
	db.SetMaxOpenConns(4)

	ctx := context.Background()
	const n = 4
	conns := make([]*sql.Conn, n)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		conns[i] = c
	}
	t.Cleanup(func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	})

	for i, c := range conns {
		var journalMode string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if journalMode != "wal" {
			t.Errorf("conn %d: journal_mode = %q, want wal", i, journalMode)
		}

		var foreignKeys int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1", i, foreignKeys)
		}

		var busyTimeout int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if busyTimeout != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, busyTimeout)
		}

		var synchronous int
		if err := c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatalf("conn %d synchronous: %v", i, err)
		}
		if synchronous != 1 { // 0=OFF, 1=NORMAL, 2=FULL, 3=EXTRA
			t.Errorf("conn %d: synchronous = %d, want 1 (NORMAL)", i, synchronous)
		}
	}
}

// TestMigrateTwiceIsANoOp doesn't assume whether sqlite-vec loads in the
// test environment: it reads VectorsLoaded from the first call and expects
// the second call to land on the same schema_migrations row count, whichever
// that is.
func TestMigrateTwiceIsANoOp(t *testing.T) {
	db := openTestDB(t)

	first, err := Migrate(db)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	want := 5 // core, session_git, touch_rooted, commit_source, retrieval_telemetry
	if first.VectorsLoaded {
		want = 6
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != want {
		t.Fatalf("schema_migrations has %d rows after first Migrate, want %d", count, want)
	}

	second, err := Migrate(db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second.VectorsLoaded != first.VectorsLoaded {
		t.Errorf("VectorsLoaded changed between calls: first=%v second=%v", first.VectorsLoaded, second.VectorsLoaded)
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations after second Migrate: %v", err)
	}
	if count != want {
		t.Errorf("schema_migrations has %d rows after second Migrate, want %d (idempotent)", count, want)
	}
}

func TestMigrateAddsSessionGitColumnsToExistingDatabase(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	) STRICT`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		agent TEXT NOT NULL,
		native_id TEXT NOT NULL,
		started_at TEXT NOT NULL
	) STRICT`); err != nil {
		t.Fatalf("create old sessions table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name) VALUES (1, 'core')`); err != nil {
		t.Fatalf("seed migration row: %v", err)
	}

	if err := migrateSessionGit(db); err != nil {
		t.Fatalf("migrateSessionGit: %v", err)
	}

	cols := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if !cols["starting_commit"] || !cols["ending_commit"] {
		t.Fatalf("sessions columns = %v, want starting_commit and ending_commit", cols)
	}
}

// TestVec0ActuallyWorks is more than a schema check: it inserts a real
// vector and runs a real KNN query, proving vec0's compiled implementation
// is actually linked and callable — not just that CREATE VIRTUAL TABLE
// happened to parse. See internal/vecext's doc for why this needed
// vendored C source rather than just importing sqlite-vec's Go bindings.
//
// If VectorsLoaded is false (a machine without a working cgo toolchain, or
// a future mattn/go-sqlite3 upgrade whose bundled SQLite has drifted from
// the vendored header), this skips rather than fails — matching Migrate's
// own contract that semantic search is optional, not required.
func TestVec0ActuallyWorks(t *testing.T) {
	db := openTestDB(t)
	result, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !result.VectorsLoaded {
		t.Skipf("vec0 did not load, skipping: %v", result.VectorsErr)
	}

	var version string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&version); err != nil {
		t.Fatalf("vec_version(): %v", err)
	}
	if version == "" {
		t.Error("vec_version() returned an empty string")
	}

	if _, err := db.Exec(`CREATE VIRTUAL TABLE t_vec_check USING vec0(id INTEGER PRIMARY KEY, embedding FLOAT[4])`); err != nil {
		t.Fatalf("create vec0 table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t_vec_check(id, embedding) VALUES (1, '[0.1, 0.2, 0.3, 0.4]'), (2, '[0.9, 0.9, 0.9, 0.9]')`); err != nil {
		t.Fatalf("insert vectors: %v", err)
	}

	var gotID int
	err = db.QueryRow(`SELECT id FROM t_vec_check WHERE embedding MATCH '[0.1, 0.2, 0.3, 0.4]' AND k = 1`).Scan(&gotID)
	if err != nil {
		t.Fatalf("KNN query: %v", err)
	}
	if gotID != 1 {
		t.Errorf("KNN match = id %d, want 1 (the identical vector, not the distant one)", gotID)
	}
}

func sessionColumns(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	cols := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = true
	}
	return cols
}

func migrationRecorded(t *testing.T, db *sql.DB, version int) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	return n == 1
}

// A database from before file-aware memory has neither column; the migration must add both and
// record itself, and running it again must be a no-op.
func TestMigrateSessionGitOnOldDatabaseIsRecordedAndIdempotent(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))) STRICT`)
	mustExec(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, agent TEXT NOT NULL, native_id TEXT NOT NULL, started_at TEXT NOT NULL) STRICT`)

	for i := 0; i < 2; i++ {
		if err := migrateSessionGit(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	cols := sessionColumns(t, db)
	if !cols["starting_commit"] || !cols["ending_commit"] {
		t.Fatalf("columns = %v, want both commit columns", cols)
	}
	if !migrationRecorded(t, db, 3) {
		t.Error("migration 003 not recorded exactly once")
	}
}

// A table that already has one of the columns (a crash mid-way under the old
// non-transactional code, or a fresh 001_core) must gain only the missing one
// instead of failing with "duplicate column name".
func TestMigrateSessionGitAddsOnlyTheMissingColumn(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))) STRICT`)
	mustExec(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, agent TEXT NOT NULL, native_id TEXT NOT NULL,
		started_at TEXT NOT NULL, starting_commit TEXT) STRICT`)
	mustExec(t, db, `INSERT INTO sessions VALUES ('a','codex','n','2026-01-01T00:00:00Z','abc')`)

	if err := migrateSessionGit(db); err != nil {
		t.Fatalf("migrateSessionGit: %v", err)
	}
	if !sessionColumns(t, db)["ending_commit"] {
		t.Error("ending_commit not added")
	}
	var start string
	if err := db.QueryRow(`SELECT starting_commit FROM sessions WHERE id='a'`).Scan(&start); err != nil || start != "abc" {
		t.Errorf("existing data = %q, %v; want it preserved", start, err)
	}
}

// If the migration fails part-way it must roll back completely: no version
// row, and no half-added column.
func TestMigrateSessionGitRollsBackOnFailure(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, agent TEXT NOT NULL, native_id TEXT NOT NULL, started_at TEXT NOT NULL) STRICT`)
	// No schema_migrations table: the final INSERT fails after both ALTERs.
	if err := migrateSessionGit(db); err == nil {
		t.Fatal("expected an error when schema_migrations is missing")
	}
	cols := sessionColumns(t, db)
	if cols["starting_commit"] || cols["ending_commit"] {
		t.Errorf("columns = %v, want the ALTERs rolled back", cols)
	}
}

// Fresh databases get the columns from 001_core; Migrate must still record 003
// and a second Migrate must not fail.
func TestMigrateFreshDatabaseRecordsSessionGit(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 2; i++ {
		if _, err := Migrate(db); err != nil {
			t.Fatalf("Migrate #%d: %v", i+1, err)
		}
	}
	for _, v := range []int{3, 4, 5} {
		if !migrationRecorded(t, db, v) {
			t.Errorf("migration %03d not recorded on a fresh database", v)
		}
	}
	if cols := sessionColumns(t, db); !cols["starting_commit"] || !cols["ending_commit"] {
		t.Errorf("columns = %v", cols)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// Migration 004: old file_touches rows must survive and read back as
// "not rooted" (0), so `ctx repair git` knows to upgrade them.
func TestMigrateTouchRootedKeepsOldRowsUnrooted(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))) STRICT`)
	mustExec(t, db, `CREATE TABLE file_touches (id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, path TEXT NOT NULL, action TEXT NOT NULL) STRICT`)
	mustExec(t, db, `INSERT INTO file_touches(session_id, path, action) VALUES ('s','x.go','edit')`)

	for i := 0; i < 2; i++ {
		if err := migrateTouchRooted(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var rooted int
	if err := db.QueryRow(`SELECT rooted FROM file_touches`).Scan(&rooted); err != nil || rooted != 0 {
		t.Errorf("rooted = %d, %v; want old rows to read 0", rooted, err)
	}
	if !migrationRecorded(t, db, 4) {
		t.Error("migration 004 not recorded exactly once")
	}
}

func TestMigrateCommitSourceAddsANullableColumn(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))) STRICT`)
	mustExec(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, agent TEXT NOT NULL, native_id TEXT NOT NULL, started_at TEXT NOT NULL) STRICT`)
	mustExec(t, db, `INSERT INTO sessions VALUES ('a','codex','n','2026-01-01T00:00:00Z')`)
	for i := 0; i < 2; i++ {
		if err := migrateCommitSource(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var src *string
	if err := db.QueryRow(`SELECT commit_source FROM sessions`).Scan(&src); err != nil || src != nil {
		t.Errorf("commit_source = %v, %v; want NULL for existing rows", src, err)
	}
	if !migrationRecorded(t, db, 5) {
		t.Error("migration 005 not recorded exactly once")
	}
}
