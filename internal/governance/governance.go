// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package governance turns explicit retrieval outcomes into conservative
// evidence reliability estimates. It does not rank results itself; keeping
// measurement separate from policy lets the estimates be evaluated before
// they influence retrieval.
package governance

import "database/sql"

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

// Assess estimates reliability with beta smoothing and actuarial credibility.
// The portfolio prior is deliberately 1.0: without enough evidence, this
// observer cannot penalize an otherwise relevant memory.
func Assess(db *sql.DB, projectID int64, evidenceKey string) (Assessment, error) {
	a := Assessment{EvidenceKey: evidenceKey, Reliability: 1, Neutral: true}
	err := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN outcome='helpful' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN outcome IN ('unhelpful','stale','incorrect','superseded') THEN 1 ELSE 0 END),0)
		FROM memory_observations WHERE project_id=? AND evidence_key=?`, projectID, evidenceKey).
		Scan(&a.Observations, &a.Positive, &a.Negative)
	if err != nil {
		return a, err
	}
	if a.Observations < MinCredibleObservations {
		return a, nil
	}
	a.Neutral = false
	a.Credibility = float64(a.Observations) / (float64(a.Observations) + CredibilityK)
	experience := float64(a.Positive+1) / float64(a.Observations+2)
	a.Reliability = (1 - a.Credibility) + a.Credibility*experience
	return a, nil
}
