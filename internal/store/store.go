// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package store manages the ctx SQLite database: opening it with the
// connection pragmas every ctx component depends on, and applying the
// embedded schema migrations.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"

	"github.com/danPoku/kaectx/internal/vecext"
	"github.com/danPoku/kaectx/migrations"
)

func init() {
	// Registers sqlite-vec's vec0 module on every SQLite connection opened
	// in this process from here on. Must run before Open is ever called.
	// See internal/vecext's doc for why this needs a vendored package
	// rather than importing sqlite-vec's own Go bindings directly. If the
	// module still can't be used for some reason, that surfaces later as a
	// "no such module: vec0" error when 002_vectors.sql runs, which Migrate
	// treats as optional rather than fatal.
	vecext.Register()
}

// Open opens (creating if necessary) the SQLite database at path with the
// four connection pragmas required by every ctx component: journal_mode=WAL,
// foreign_keys=ON, busy_timeout=5000, synchronous=NORMAL.
//
// The pragmas ride on the DSN rather than a post-Open Exec. database/sql
// pools connections lazily and opens more of them under concurrent load, and
// PRAGMA journal_mode/foreign_keys/synchronous are per-connection state in
// SQLite — an Exec run once right after Open only ever reaches whichever
// single connection the pool happened to create first. Putting the pragmas
// in the DSN's query string makes mattn/go-sqlite3 apply them to every
// connection it opens, present and future.
func Open(path string) (*sql.DB, error) {
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_synchronous=NORMAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	return db, nil
}

// MigrateResult reports what Migrate actually applied, so callers (ctx init,
// tests) can tell whether semantic search is available without parsing error
// strings.
type MigrateResult struct {
	VectorsLoaded bool
	VectorsErr    error // non-nil iff !VectorsLoaded; the reason, for logging
}

// Migrate applies the embedded schema migrations. 001_core.sql is
// non-optional: sessions, messages, keyword search, notes all depend on it,
// so a failure there is fatal. 002_vectors.sql needs the sqlite-vec
// extension's vec0 virtual table module; if that module isn't available the
// CREATE VIRTUAL TABLE statement fails with "no such module: vec0", and
// Migrate reports that in the result instead of returning an error — the
// catalogue stays open even when the "find similar cases" desk is closed.
//
// Both migration files are idempotent at the SQL level (CREATE ... IF NOT
// EXISTS, INSERT OR IGNORE), so calling Migrate more than once on the same
// database is a no-op, not an error.
func Migrate(db *sql.DB) (MigrateResult, error) {
	core, err := migrations.FS.ReadFile("001_core.sql")
	if err != nil {
		return MigrateResult{}, fmt.Errorf("read 001_core.sql: %w", err)
	}
	if _, err := db.Exec(string(core)); err != nil {
		return MigrateResult{}, fmt.Errorf("apply 001_core.sql: %w", err)
	}

	vectors, err := migrations.FS.ReadFile("002_vectors.sql")
	if err != nil {
		return MigrateResult{}, fmt.Errorf("read 002_vectors.sql: %w", err)
	}
	result := MigrateResult{VectorsLoaded: true}
	if _, err := db.Exec(string(vectors)); err != nil {
		result = MigrateResult{VectorsLoaded: false, VectorsErr: err}
	}
	if err := migrateSessionGit(db); err != nil {
		return MigrateResult{}, err
	}
	if err := migrateTouchRooted(db); err != nil {
		return MigrateResult{}, err
	}
	if err := migrateCommitSource(db); err != nil {
		return MigrateResult{}, err
	}
	if err := applySQLMigration(db, 6, "006_retrieval_telemetry.sql"); err != nil {
		return MigrateResult{}, err
	}
	if err := applySQLMigration(db, 7, "007_memory_observations.sql"); err != nil {
		return MigrateResult{}, err
	}
	if err := migrateRetrievalGovernance(db); err != nil {
		return MigrateResult{}, err
	}
	if err := migrateRetrievalOpens(db); err != nil {
		return MigrateResult{}, err
	}
	return result, nil
}

// applySQLMigration executes a self-recording SQL migration once. The version
// check happens again inside the write transaction so two ctx processes that
// start together cannot both perform the migration work.
func applySQLMigration(db *sql.DB, version int, file string) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("open migration %03d connection: %w", version, err)
	}
	defer conn.Close()
	// A deferred transaction can read an old snapshot and then fail with
	// SQLITE_BUSY_SNAPSHOT when another process migrates first. Take the write
	// reservation before checking the version so contenders serialize here.
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin migration %03d: %w", version, err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	var applied int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied); err != nil {
		return fmt.Errorf("inspect migration %03d: %w", version, err)
	}
	if applied > 0 {
		_, err := conn.ExecContext(context.Background(), `COMMIT`)
		return err
	}
	body, err := migrations.FS.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	if _, err := conn.ExecContext(context.Background(), string(body)); err != nil {
		return fmt.Errorf("apply %s: %w", file, err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		return fmt.Errorf("commit migration %03d: %w", version, err)
	}
	return nil
}

type column struct{ name, decl string }

// migrateSessionGit is migration 003: it adds sessions.starting_commit and
// sessions.ending_commit to databases created before file-aware memory landed. Fresh databases
// already have both from 001_core.sql.
func migrateSessionGit(db *sql.DB) error {
	return addColumns(db, 3, "session_git", "sessions",
		column{"starting_commit", "TEXT"}, column{"ending_commit", "TEXT"})
}

// migrateTouchRooted is migration 004: file_touches.rooted says whether a
// row's path was computed relative to the git repository root (1) or by the
// older rule, relative to the session's working directory (0). Rows from
// before it default to 0, which is what `ctx repair git` looks for.
func migrateTouchRooted(db *sql.DB) error {
	return addColumns(db, 4, "touch_rooted", "file_touches",
		column{"rooted", "INTEGER NOT NULL DEFAULT 0"})
}

// migrateCommitSource is migration 005: sessions.commit_source records how
// starting/ending_commit were obtained (see the Source* constants), so a
// commit that is only a best guess can be shown as one. NULL means either no
// commit, or one stored before this column existed and not yet re-derived by
// `ctx repair git`.
func migrateCommitSource(db *sql.DB) error {
	return addColumns(db, 5, "commit_source", "sessions", column{"commit_source", "TEXT"})
}

// migrateRetrievalGovernance is migration 008: it records the policy inputs
// used for each candidate without changing the append-only run schema.
func migrateRetrievalGovernance(db *sql.DB) error {
	return addColumns(db, 8, "retrieval_governance", "retrieval_candidates",
		column{"original_rank", "INTEGER"},
		column{"policy_applied", "INTEGER NOT NULL DEFAULT 0"},
		column{"reliability", "REAL"},
		column{"policy_score", "REAL"},
		column{"governance_neutral", "INTEGER"})
}

// migrateRetrievalOpens is migration 009: opened_at and open_count measure
// selection separately from an explicit helpful or harmful outcome.
func migrateRetrievalOpens(db *sql.DB) error {
	return addColumns(db, 9, "retrieval_opens", "retrieval_candidates",
		column{"opened_at", "TEXT"},
		column{"open_count", "INTEGER NOT NULL DEFAULT 0"})
}

// addColumns is the shared shape of the ALTER-based migrations. It lives in
// Go rather than a .sql file because SQLite has no ADD COLUMN IF NOT EXISTS:
// the honest way to be idempotent is to look at PRAGMA table_info and add
// only what is missing. Doing the check-then-alter inside one transaction
// also means a crash between two ALTERs cannot leave a half-migrated table
// with no version row, and two processes racing to migrate (the daemon and a
// CLI call) serialise on the write lock — the loser re-checks inside its own
// transaction and finds nothing left to do.
func addColumns(db *sql.DB, version int, name, table string, cols ...column) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %03d: %w", version, err)
	}
	defer tx.Rollback() // no-op once committed

	have := map[string]bool{}
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, c := range cols {
		if have[c.name] {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + c.name + ` ` + c.decl); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, c.name, err)
		}
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO schema_migrations(version, name) VALUES (?, ?)`, version, name); err != nil {
		return fmt.Errorf("record migration %03d: %w", version, err)
	}
	return tx.Commit()
}
