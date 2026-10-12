// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package feedback records explicit outcomes for retrieved evidence. The MCP
// record_retrieval_outcome tool and the `ctx feedback` command both write
// through Record, so validation, redaction and anchoring happen in one place
// whichever way a label arrives.
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
func ValidOutcome(o string) bool {
	for _, v := range Outcomes {
		if o == v {
			return true
		}
	}
	return false
}

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
