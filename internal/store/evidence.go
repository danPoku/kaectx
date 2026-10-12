// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
)

// EvidenceKey identifies one exchange's exact message range. It names what a
// search returned; it is not stable across rechunking, which is why labels
// also carry an anchor (see migrateEvidenceAnchors).
func EvidenceKey(sessionID string, firstSeq, lastSeq int) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + strconv.Itoa(firstSeq) + "\x00" + strconv.Itoa(lastSeq)))
	return hex.EncodeToString(sum[:])
}

// migrateEvidenceAnchors is migration 012. It gives every memory observation
// an anchor, the session and first message seq of the exchange that was
// labelled, so governance can match a label to whichever chunk contains that
// message today instead of to one exact, hashed range.
//
// Existing labels are anchored from the best source still available:
//  1. the candidate they were recorded against, if its run survives;
//  2. any other candidate with the same evidence key (runs are pruned, but
//     the same exchange is often retrieved again);
//  3. a chunk whose current range hashes to that key.
//
// A label none of these recovers keeps a NULL anchor and goes on matching its
// exact key, as it always did. The whole migration, columns included, runs
// under BEGIN IMMEDIATE so a concurrent ingest or a second ctx process cannot
// interleave with the backfill.
func migrateEvidenceAnchors(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration 012 connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin migration 012: %w", err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)

	var applied int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 12`).Scan(&applied); err != nil {
		return fmt.Errorf("inspect migration 012: %w", err)
	}
	if applied > 0 {
		_, err := conn.ExecContext(ctx, `COMMIT`)
		return err
	}
	have := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info('memory_observations')`)
	if err != nil {
		return fmt.Errorf("inspect memory_observations: %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	for _, c := range []column{{"session_id", "TEXT"}, {"anchor_seq", "INTEGER"}} {
		if have[c.name] {
			continue
		}
		if _, err := conn.ExecContext(ctx, `ALTER TABLE memory_observations ADD COLUMN `+c.name+` `+c.decl); err != nil {
			return fmt.Errorf("add memory_observations.%s: %w", c.name, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_memory_observations_anchor
		ON memory_observations(project_id, session_id, anchor_seq)`); err != nil {
		return fmt.Errorf("index memory_observations anchor: %w", err)
	}
	if err := backfillEvidenceAnchors(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version, name) VALUES (12, 'evidence_anchors')`); err != nil {
		return fmt.Errorf("record migration 012: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit migration 012: %w", err)
	}
	return nil
}

// execQuerier is what the backfill needs; *sql.Conn and *sql.Tx both have it.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func backfillEvidenceAnchors(ctx context.Context, db execQuerier) error {
	// 1 and 2 in one statement: prefer the label's own candidate, else any
	// candidate that returned the same range.
	if _, err := db.ExecContext(ctx, `UPDATE memory_observations AS mo
		SET session_id = src.session_id, anchor_seq = src.first_seq
		FROM (
			SELECT mo2.id AS id,
			       COALESCE(own.session_id, (SELECT rc.session_id FROM retrieval_candidates rc WHERE rc.evidence_key = mo2.evidence_key LIMIT 1)) AS session_id,
			       COALESCE(own.first_seq,  (SELECT rc.first_seq  FROM retrieval_candidates rc WHERE rc.evidence_key = mo2.evidence_key LIMIT 1)) AS first_seq
			  FROM memory_observations mo2
			  LEFT JOIN retrieval_candidates own ON own.run_id = mo2.run_id AND own.rank = mo2.candidate_rank
			 WHERE mo2.anchor_seq IS NULL
		) AS src
		WHERE mo.id = src.id AND src.first_seq IS NOT NULL`); err != nil {
		return fmt.Errorf("anchor observations from candidates: %w", err)
	}

	// 3: hash current chunks only for keys still unresolved, so a database
	// with nothing left to do never scans the chunks table.
	missing := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT evidence_key FROM memory_observations WHERE anchor_seq IS NULL`)
	if err != nil {
		return fmt.Errorf("list unanchored observations: %w", err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		missing[k] = true
	}
	rows.Close()
	if len(missing) == 0 {
		return nil
	}
	type anchor struct {
		session string
		seq     int
	}
	found := map[string]anchor{}
	rows, err = db.QueryContext(ctx, `SELECT session_id, first_seq, last_seq FROM chunks`)
	if err != nil {
		return fmt.Errorf("list chunks: %w", err)
	}
	for rows.Next() {
		var s string
		var first, last int
		if err := rows.Scan(&s, &first, &last); err != nil {
			rows.Close()
			return err
		}
		if k := EvidenceKey(s, first, last); missing[k] {
			found[k] = anchor{s, first}
		}
	}
	rows.Close()
	for k, a := range found {
		if _, err := db.ExecContext(ctx, `UPDATE memory_observations SET session_id = ?, anchor_seq = ?
			WHERE evidence_key = ? AND anchor_seq IS NULL`, a.session, a.seq, k); err != nil {
			return fmt.Errorf("anchor observations from chunks: %w", err)
		}
	}
	return nil
}
