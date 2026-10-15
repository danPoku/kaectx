// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package feedback records explicit outcomes for retrieved evidence. The MCP
// record_retrieval_outcome tool and the `ctx feedback` command both write
// through Record, so validation, redaction and anchoring happen in one place
// whichever way a label arrives. RecordSearch is the separate path for a
// verdict on a whole search.
package feedback

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/redact"
)

// Outcomes lists the labels an outcome may take, in the order shown to users.
var Outcomes = []string{"helpful", "unhelpful", "stale", "incorrect", "superseded"}

// ValidOutcome reports whether o is one of Outcomes.
func ValidOutcome(o string) bool { return contains(Outcomes, o) }

// ErrNotFound means no search result with that run ID and rank exists in the
// project: a mistyped ID, another project's run, or a run pruned by retention.
var ErrNotFound = errors.New("retrieval result not found in this project")

// Result reports a recorded outcome and the evidence's assessment after it.
//
// Recording and assessing are separate steps. If the outcome was stored but
// the assessment then failed, Record returns Recorded true and the failure in
// AssessErr, not as its error: the label is safely in place, and reporting the
// call as failed would invite a retry that changes nothing.
type Result struct {
	Recorded   bool
	Evidence   governance.Evidence
	Assessment governance.Assessment
	AssessErr  error
}

// Record stores outcome for the result at rank in search runID, replacing any
// earlier outcome for that same result. note is redacted before storage. The
// label is anchored to the result's first message, so it keeps counting if the
// chunk is later rebuilt with different bounds.
func Record(db *sql.DB, projectID int64, runID string, rank int, outcome, note string) (Result, error) {
	if !ValidOutcome(outcome) {
		return Result{}, fmt.Errorf("outcome must be one of %v, not %q", Outcomes, outcome)
	}
	if runID == "" || rank <= 0 {
		return Result{}, fmt.Errorf("run_id and a positive rank are required")
	}
	var e governance.Evidence
	err := db.QueryRow(`SELECT rc.evidence_key, rc.session_id, rc.first_seq, rc.last_seq
		FROM retrieval_candidates rc JOIN retrieval_runs rr ON rr.id=rc.run_id
		WHERE rc.run_id=? AND rc.rank=? AND rr.project_id=?`, runID, rank, projectID).Scan(&e.Key, &e.SessionID, &e.FirstSeq, &e.LastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, err
	}
	var storedNote any
	if note != "" {
		storedNote = redact.Redact(note)
	}
	if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,session_id,anchor_seq,run_id,candidate_rank,outcome,note)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id,candidate_rank) WHERE run_id IS NOT NULL DO UPDATE SET
			outcome=excluded.outcome, note=excluded.note,
			created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`, projectID, e.Key, e.SessionID, e.FirstSeq, runID, rank, outcome, storedNote); err != nil {
		return Result{}, err
	}
	out := Result{Recorded: true, Evidence: e}
	out.Assessment, out.AssessErr = governance.Assess(db, projectID, e)
	return out, nil
}

// SearchOutcomes lists the verdicts a whole search may take, and
// SearchReasons the gaps a partial or unhelpful search may name. Each reason
// points at a different fix (cross-project scope, ingest freshness, surfacing
// later work, ranking), so counting them shows which gap matters most.
var (
	SearchOutcomes = []string{"helpful", "partial", "unhelpful"}
	SearchReasons  = []string{"wrong_project", "not_indexed", "stale_state", "ranking"}
)

func contains(list []string, s string) bool {
	for _, v := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ErrRunNotFound means no search with that run ID exists in the project.
var ErrRunNotFound = errors.New("search run not found in this project")

// RecordSearch stores the verdict on search runID as a whole, replacing any
// earlier verdict for that run. It writes only retrieval_run_observations,
// which governance never reads: a search that missed half the story says
// nothing about whether the exchanges it did return were reliable, so it must
// not move any evidence's credibility.
func RecordSearch(db *sql.DB, projectID int64, runID, outcome, reason, note string) error {
	if !contains(SearchOutcomes, outcome) {
		return fmt.Errorf("outcome must be one of %v, not %q", SearchOutcomes, outcome)
	}
	if reason != "" && !contains(SearchReasons, reason) {
		return fmt.Errorf("reason must be one of %v, not %q", SearchReasons, reason)
	}
	if outcome == "helpful" && reason != "" {
		return fmt.Errorf("a helpful search has no gap to name; leave reason empty")
	}
	if runID == "" {
		return fmt.Errorf("run_id is required")
	}
	var found int
	err := db.QueryRow(`SELECT 1 FROM retrieval_runs WHERE id=? AND project_id=?`, runID, projectID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRunNotFound
	}
	if err != nil {
		return err
	}
	var storedReason, storedNote any
	if reason != "" {
		storedReason = reason
	}
	if note != "" {
		storedNote = redact.Redact(note)
	}
	_, err = db.Exec(`INSERT INTO retrieval_run_observations(project_id,run_id,outcome,reason,note)
		VALUES (?,?,?,?,?)
		ON CONFLICT(run_id) WHERE run_id IS NOT NULL DO UPDATE SET
			outcome=excluded.outcome, reason=excluded.reason, note=excluded.note,
			created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`, projectID, runID, outcome, storedReason, storedNote)
	return err
}
