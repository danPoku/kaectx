// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package governance turns explicit retrieval outcomes into conservative
// evidence reliability estimates. It does not rank results itself; keeping
// measurement separate from policy lets the estimates be evaluated before
// they influence retrieval.
package governance

import (
	"context"
	"database/sql"
	"strings"
)

const (
	// Sparse observations are too easy to overinterpret. Until this many
	// outcomes exist, evidence remains neutral in any downstream policy.
	MinCredibleObservations = 3
	// CredibilityK controls how quickly evidence-specific experience earns
	// weight. At n=5, individual experience receives half the weight.
	CredibilityK = 5.0
)

type Assessment struct {
	EvidenceKey  string
	Observations int
	Positive     int
	Negative     int
	Credibility  float64
	Reliability  float64
	Neutral      bool
}

// Assess estimates reliability with actuarial credibility.
// The portfolio prior is deliberately 1.0: without enough evidence, this
// observer cannot penalize an otherwise relevant memory.
func Assess(db *sql.DB, projectID int64, evidenceKey string) (Assessment, error) {
	return AssessContext(context.Background(), db, projectID, evidenceKey)
}

func AssessContext(ctx context.Context, db *sql.DB, projectID int64, evidenceKey string) (Assessment, error) {
	a := Assessment{EvidenceKey: evidenceKey, Reliability: 1, Neutral: true}
	err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN outcome='helpful' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN outcome IN ('unhelpful','stale','incorrect','superseded') THEN 1 ELSE 0 END),0)
		FROM memory_observations WHERE project_id=? AND evidence_key=?`, projectID, evidenceKey).
		Scan(&a.Observations, &a.Positive, &a.Negative)
	if err != nil {
		return a, err
	}
	finalize(&a)
	return a, nil
}

// AssessMany evaluates a result set in bounded batches, avoiding one query
// and one busy-timeout window per candidate. Keys without observations are
// returned as explicitly neutral assessments.
func AssessMany(ctx context.Context, db *sql.DB, projectID int64, evidenceKeys []string) (map[string]Assessment, error) {
	out := make(map[string]Assessment, len(evidenceKeys))
	unique := make([]string, 0, len(evidenceKeys))
	for _, key := range evidenceKeys {
		if _, ok := out[key]; ok {
			continue
		}
		out[key] = Assessment{EvidenceKey: key, Reliability: 1, Neutral: true}
		unique = append(unique, key)
	}
	const batchSize = 500 // comfortably below SQLite's common variable limit
	for start := 0; start < len(unique); start += batchSize {
		end := start + batchSize
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, projectID)
		marks := make([]string, len(batch))
		for i, key := range batch {
			marks[i] = "?"
			args = append(args, key)
		}
		rows, err := db.QueryContext(ctx, `SELECT evidence_key, COUNT(*),
			COALESCE(SUM(CASE WHEN outcome='helpful' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN outcome IN ('unhelpful','stale','incorrect','superseded') THEN 1 ELSE 0 END),0)
			FROM memory_observations WHERE project_id=? AND evidence_key IN (`+strings.Join(marks, ",")+`)
			GROUP BY evidence_key`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a Assessment
			if err := rows.Scan(&a.EvidenceKey, &a.Observations, &a.Positive, &a.Negative); err != nil {
				rows.Close()
				return nil, err
			}
			a.Reliability, a.Neutral = 1, true
			finalize(&a)
			out[a.EvidenceKey] = a
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// finalize blends the evidence's own helpful rate with the 1.0 prior,
// weighting it by credibility Z = n/(n+K).
//
// The helpful rate is used raw, not Laplace-smoothed. Credibility already
// shrinks sparse experience toward the prior; smoothing it again toward 0.5
// shrank twice, so even a perfect record scored below an unlabelled result
// (3 of 3 helpful gave 0.925) and helpful labels demoted what they praised.
// Like an underwriter blending a policy's own claims record with the book
// rate: a clean record should never price worse than no record at all.
func finalize(a *Assessment) {
	if a.Observations < MinCredibleObservations {
		return
	}
	a.Neutral = false
	a.Credibility = float64(a.Observations) / (float64(a.Observations) + CredibilityK)
	experience := float64(a.Positive) / float64(a.Observations)
	a.Reliability = (1 - a.Credibility) + a.Credibility*experience
}
