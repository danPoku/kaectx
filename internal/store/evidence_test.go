// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"testing"
)

// TestBackfillEvidenceAnchors covers each source migration 012 recovers an
// anchor from, and the label it cannot recover.
func TestBackfillEvidenceAnchors(t *testing.T) {
	db := openTestDB(t)
	if _, err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO projects(id, name) VALUES (1, 'p');
		INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES ('s', 'codex', 's', 1, '2026-10-01T00:00:00Z');
		INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES ('s', 20, 25, 'chunk only');
		INSERT INTO retrieval_runs(id, project_id, surface, query, requested_limit, mode, duration_ms, result_count)
			VALUES ('live', 1, 'mcp', 'q', 10, 'keyword', 0, 2);
		INSERT INTO retrieval_candidates(run_id, rank, evidence_key, session_id, first_seq, last_seq, score, score_kind)
			VALUES ('live', 1, '`+EvidenceKey("s", 1, 4)+`', 's', 1, 4, 0, 'bm25'),
			       ('live', 2, '`+EvidenceKey("s", 10, 12)+`', 's', 10, 12, 0, 'bm25');`)

	insert := func(key string, runID any, rank any) int64 {
		t.Helper()
		res, err := db.Exec(`INSERT INTO memory_observations(project_id, evidence_key, run_id, candidate_rank, outcome)
			VALUES (1, ?, ?, ?, 'helpful')`, key, runID, rank)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	own := insert(EvidenceKey("s", 1, 4), "live", 1)        // its own candidate survives
	pruned := insert(EvidenceKey("s", 10, 12), nil, nil)    // run pruned; another candidate has the key
	chunkOnly := insert(EvidenceKey("s", 20, 25), nil, nil) // only a current chunk hashes to it
	lost := insert(EvidenceKey("s", 30, 31), nil, nil)      // nothing left to recover from
	mustExec(t, db, `UPDATE memory_observations SET session_id = NULL, anchor_seq = NULL`)

	if err := backfillEvidenceAnchors(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   int64
		want sql.NullInt64
	}{
		{"own candidate", own, sql.NullInt64{Int64: 1, Valid: true}},
		{"pruned run, same key elsewhere", pruned, sql.NullInt64{Int64: 10, Valid: true}},
		{"current chunk", chunkOnly, sql.NullInt64{Int64: 20, Valid: true}},
		{"unrecoverable", lost, sql.NullInt64{}},
	} {
		var session sql.NullString
		var anchor sql.NullInt64
		if err := db.QueryRow(`SELECT session_id, anchor_seq FROM memory_observations WHERE id = ?`, c.id).Scan(&session, &anchor); err != nil {
			t.Fatal(err)
		}
		if anchor != c.want || (c.want.Valid && session.String != "s") {
			t.Errorf("%s: anchor %v session %v, want %v in s", c.name, anchor, session, c.want)
		}
	}
}

// TestMigrateEvidenceAnchorsUpgradesExistingDatabase runs the migration on a
// database whose memory_observations predates 012.
func TestMigrateEvidenceAnchorsUpgradesExistingDatabase(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;
		CREATE TABLE chunks (session_id TEXT, first_seq INTEGER, last_seq INTEGER) STRICT;
		CREATE TABLE retrieval_candidates (run_id TEXT, rank INTEGER, evidence_key TEXT, session_id TEXT, first_seq INTEGER) STRICT;
		CREATE TABLE memory_observations (id INTEGER PRIMARY KEY, project_id INTEGER, evidence_key TEXT,
			run_id TEXT, candidate_rank INTEGER, outcome TEXT) STRICT;
		INSERT INTO chunks VALUES ('s', 3, 7);
		INSERT INTO memory_observations(project_id, evidence_key, outcome) VALUES (1, '`+EvidenceKey("s", 3, 7)+`', 'stale');`)
	if err := migrateEvidenceAnchors(db); err != nil {
		t.Fatalf("migrateEvidenceAnchors: %v", err)
	}
	if err := migrateEvidenceAnchors(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var anchor, versions, indexes int
	if err := db.QueryRow(`SELECT anchor_seq FROM memory_observations`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 12`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'idx_memory_observations_anchor'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if anchor != 3 || versions != 1 || indexes != 1 {
		t.Errorf("anchor=%d version rows=%d index=%d, want 3, 1, 1", anchor, versions, indexes)
	}
}
