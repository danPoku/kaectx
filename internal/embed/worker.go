// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

// Run processes up to batchSize pending chunks (chunks.embedded_at IS
// NULL — PendingEmbeddings is the embed worker's to-do list, per that
// query's own comment): embeds each with client, writes it into
// chunk_vectors, and marks the chunk embedded.
//
// A chunk whose session never resolved a project is never embedded —
// chunk_vectors.project_id is vec0's PARTITION KEY and can't be NULL, and a
// session with no project is already a degenerate case (see
// internal/ingest/session.go). PendingEmbeddings leaves those chunks out, so
// they can't crowd embeddable chunks out of a batch; they stay pending until
// project resolution is fixed upstream rather than given a fake value here.
//
// One chunk failing to embed (a transient network error, model unloaded)
// stops the batch and returns the error — unlike ingest's per-line
// tolerance, there's no useful way to "skip and continue" a chunk that
// still needs embedding: it just stays pending and gets retried next Run.
func Run(ctx context.Context, db *sql.DB, client Embedder, batchSize int) (processed int, err error) {
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "PendingEmbeddings")
	if err != nil {
		return 0, err
	}

	rows, err := db.Query(sqlText, sql.Named("limit", batchSize))
	if err != nil {
		return 0, err
	}
	type pending struct {
		chunkID   int64
		text      string
		projectID sql.NullInt64
		agent     string
		sessionID string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.chunkID, &p.text, &p.projectID, &p.agent, &p.sessionID); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, p := range batch {
		if !p.projectID.Valid {
			// Unreachable while the query filters these out; kept so a
			// future query change can't turn it into a NOT NULL failure.
			continue
		}

		vec, err := client.Embed(ctx, p.text)
		if err != nil {
			return processed, fmt.Errorf("embed chunk %d: %w", p.chunkID, err)
		}
		vecJSON, err := json.Marshal(vec)
		if err != nil {
			return processed, fmt.Errorf("encode embedding for chunk %d: %w", p.chunkID, err)
		}

		if _, err := db.Exec(`INSERT INTO chunk_vectors(chunk_id, project_id, agent, embedding, session_id) VALUES (?,?,?,?,?)`,
			p.chunkID, p.projectID.Int64, p.agent, string(vecJSON), p.sessionID); err != nil {
			return processed, fmt.Errorf("insert vector for chunk %d: %w", p.chunkID, err)
		}
		if _, err := db.Exec(`UPDATE chunks SET embedded_at = strftime('%Y-%m-%dT%H:%M:%SZ','now'), embedding_model = ? WHERE id = ?`,
			client.Model(), p.chunkID); err != nil {
			return processed, fmt.Errorf("mark chunk %d embedded: %w", p.chunkID, err)
		}
		processed++
	}
	return processed, nil
}
