// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package doctor reports the health and observability of a ctx project.
package doctor

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/danPoku/kaectx/internal/governance"
)

type Report struct {
	ProjectName          string
	Sessions             int
	Messages             int
	Chunks               int
	PendingEmbeddings    int
	UnassignedChunks     int
	Sources              int
	SourcesWithErrors    int
	LastIngestedAt       sql.NullString
	RecentRetrievals     int
	RecentZeroResultRuns int
	RecentHybridRuns     int
	RecentPolicyRuns     int
	Outcomes             int
	RetrievedEvidence    int
	ObservedEvidence     int
	CredibleEvidence     int
}

func (r Report) OutcomeCoverage() float64 {
	if r.RetrievedEvidence == 0 {
		return 0
	}
	return 100 * float64(r.ObservedEvidence) / float64(r.RetrievedEvidence)
}

func (r Report) NeutralObservedEvidence() int {
	return r.ObservedEvidence - r.CredibleEvidence
}

// Collect reads a consistent diagnostic snapshot. It never mutates or repairs
// the store: doctor is safe to run while the daemon or MCP server is active.
func Collect(ctx context.Context, db *sql.DB, projectID int64) (Report, error) {
	var r Report
	if err := db.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, projectID).Scan(&r.ProjectName); err != nil {
		return r, fmt.Errorf("read project: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions WHERE project_id=?),
		(SELECT COUNT(*) FROM messages m JOIN sessions se ON se.id=m.session_id WHERE se.project_id=?),
		(SELECT COUNT(*) FROM chunks c JOIN sessions se ON se.id=c.session_id WHERE se.project_id=?),
		(SELECT COUNT(*) FROM chunks c JOIN sessions se ON se.id=c.session_id WHERE se.project_id=? AND c.embedded_at IS NULL)`,
		projectID, projectID, projectID, projectID).Scan(&r.Sessions, &r.Messages, &r.Chunks, &r.PendingEmbeddings); err != nil {
		return r, fmt.Errorf("read corpus health: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks c JOIN sessions se ON se.id=c.session_id WHERE se.project_id IS NULL`).Scan(&r.UnassignedChunks); err != nil {
		return r, fmt.Errorf("read unassigned chunks: %w", err)
	}
	// Source files are global because a parse failure may happen before a
	// session exists, leaving no honest project attribution.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN last_error IS NOT NULL AND last_error<>'' THEN 1 ELSE 0 END),0),
		MAX(last_ingested_at)
		FROM sources`).Scan(&r.Sources, &r.SourcesWithErrors, &r.LastIngestedAt); err != nil {
		return r, fmt.Errorf("read ingestion health: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN result_count=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN mode='hybrid' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM retrieval_candidates rc WHERE rc.run_id=rr.id AND rc.policy_applied=1
		) THEN 1 ELSE 0 END),0)
		FROM retrieval_runs rr
		WHERE project_id=? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-7 days')`, projectID).
		Scan(&r.RecentRetrievals, &r.RecentZeroResultRuns, &r.RecentHybridRuns, &r.RecentPolicyRuns); err != nil {
		return r, fmt.Errorf("read retrieval health: %w", err)
	}
	if err := db.QueryRowContext(ctx, `WITH retrieved AS (
		SELECT DISTINCT rc.evidence_key FROM retrieval_candidates rc
		JOIN retrieval_runs rr ON rr.id=rc.run_id WHERE rr.project_id=?
	), observed AS (
		SELECT evidence_key, COUNT(*) AS n FROM memory_observations
		WHERE project_id=? GROUP BY evidence_key
	)
	SELECT
		(SELECT COUNT(*) FROM memory_observations WHERE project_id=?),
		(SELECT COUNT(*) FROM retrieved),
		(SELECT COUNT(*) FROM observed JOIN retrieved USING(evidence_key)),
		(SELECT COUNT(*) FROM observed JOIN retrieved USING(evidence_key) WHERE observed.n>=?)`,
		projectID, projectID, projectID, governance.MinCredibleObservations).Scan(&r.Outcomes, &r.RetrievedEvidence, &r.ObservedEvidence, &r.CredibleEvidence); err != nil {
		return r, fmt.Errorf("read outcome coverage: %w", err)
	}
	return r, nil
}
