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
	"encoding/json"
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

// Evidence is one retrieved exchange as it stands now: its exact range and
// that range's evidence key.
type Evidence struct {
	Key       string
	SessionID string
	FirstSeq  int
	LastSeq   int
}

// matchObservations is the one rule for which labels count toward a piece of
// evidence, shared by Assess, AssessMany and ctx doctor's coverage report.
//
// A label is anchored to the first message of the exchange it judged
// (session_id, anchor_seq). It counts toward whichever exchange contains that
// message today, so labels survive chunks being rebuilt with new bounds:
//   - a range that grows, or two chunks that merge, keeps every label anchored
//     inside it (the merged exchange contains exactly the judged text);
//   - a chunk that splits keeps its labels with the part holding the anchor,
//     the original question, and the other part starts unlabelled.
//
// A label whose anchor could not be recovered (anchor_seq NULL) still matches
// its exact original range by evidence key.
//
// It expects the candidate columns as c.k, c.s, c.f, c.l and the observation
// alias mo.
const matchObservations = `mo.project_id = ? AND (
		(mo.anchor_seq IS NOT NULL AND mo.session_id = c.s AND mo.anchor_seq BETWEEN c.f AND c.l)
		OR (mo.anchor_seq IS NULL AND mo.evidence_key = c.k))`

// MatchObservationsSQL exposes matchObservations to ctx doctor, which must
// count coverage by the same rule governance assesses by.
func MatchObservationsSQL() string { return matchObservations }

// Assess estimates reliability with actuarial credibility.
// The portfolio prior is deliberately 1.0: without enough evidence, this
// observer cannot penalize an otherwise relevant memory.
func Assess(db *sql.DB, projectID int64, e Evidence) (Assessment, error) {
	return AssessContext(context.Background(), db, projectID, e)
}

func AssessContext(ctx context.Context, db *sql.DB, projectID int64, e Evidence) (Assessment, error) {
	got, err := AssessMany(ctx, db, projectID, []Evidence{e})
	if err != nil {
		return Assessment{EvidenceKey: e.Key, Reliability: 1, Neutral: true}, err
	}
	return got[e.Key], nil
}

// AssessMany evaluates a result set in one query. The candidates travel as a
// single JSON parameter, so no result-set size can reach SQLite's bound
// variable limit. Evidence without matching labels comes back explicitly
// neutral. The map is keyed by evidence key.
func AssessMany(ctx context.Context, db *sql.DB, projectID int64, evidence []Evidence) (map[string]Assessment, error) {
	out := make(map[string]Assessment, len(evidence))
	type row struct {
		K string `json:"k"`
		S string `json:"s"`
		F int    `json:"f"`
		L int    `json:"l"`
	}
	unique := make([]row, 0, len(evidence))
	for _, e := range evidence {
		if _, ok := out[e.Key]; ok {
			continue
		}
		out[e.Key] = Assessment{EvidenceKey: e.Key, Reliability: 1, Neutral: true}
		unique = append(unique, row{e.Key, e.SessionID, e.FirstSeq, e.LastSeq})
	}
	if len(unique) == 0 {
		return out, nil
	}
	payload, err := json.Marshal(unique)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `WITH c(k, s, f, l) AS (
			SELECT json_extract(value,'$.k'), json_extract(value,'$.s'),
			       json_extract(value,'$.f'), json_extract(value,'$.l')
			  FROM json_each(?)
		)
		SELECT c.k, COUNT(*),
			COALESCE(SUM(CASE WHEN mo.outcome='helpful' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN mo.outcome IN ('unhelpful','stale','incorrect','superseded') THEN 1 ELSE 0 END),0)
		  FROM c JOIN memory_observations mo ON `+matchObservations+`
		 GROUP BY c.k`, string(payload), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Assessment
		if err := rows.Scan(&a.EvidenceKey, &a.Observations, &a.Positive, &a.Negative); err != nil {
			return nil, err
		}
		a.Reliability, a.Neutral = 1, true
		finalize(&a)
		out[a.EvidenceKey] = a
	}
	return out, rows.Err()
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
