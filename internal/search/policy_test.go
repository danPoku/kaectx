// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/store"
)

func openPolicyTestDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO projects(name) VALUES (?)`, "policy-test")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return db, projectID
}

func addOutcome(t *testing.T, db *sql.DB, projectID int64, evidence, outcome string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		runID := fmt.Sprintf("run-%s-%d", evidence[:8], i)
		if _, err := db.Exec(`INSERT INTO retrieval_runs(id,project_id,surface,query,requested_limit,mode,duration_ms,result_count) VALUES (?,?, 'internal','q',10,'keyword',0,1)`, runID, projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO retrieval_candidates(run_id,rank,evidence_key,session_id,first_seq,last_seq,score,score_kind) VALUES (?,1,?,'seed',0,0,0.0,'bm25')`, runID, evidence); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,run_id,candidate_rank,outcome) VALUES (?,?,?,?,?)`, projectID, evidence, runID, 1, outcome); err != nil {
			t.Fatal(err)
		}
	}
}

func TestApplyGovernanceReranksCrediblyNegativeEvidence(t *testing.T) {
	db, projectID := openPolicyTestDB(t)
	results := []Result{
		{SessionID: "top", FirstSeq: 1, LastSeq: 2},
		{SessionID: "second", FirstSeq: 3, LastSeq: 4},
	}
	addOutcome(t, db, projectID, evidenceKey(results[0]), "incorrect", 3)

	if err := applyGovernance(context.Background(), db, projectID, results); err != nil {
		t.Fatal(err)
	}
	if results[0].SessionID != "second" || results[1].SessionID != "top" {
		t.Fatalf("order = %s, %s; want second, top", results[0].SessionID, results[1].SessionID)
	}
	if results[1].GovernanceNeutral || results[1].Reliability >= 1 {
		t.Errorf("negative evidence = neutral %v reliability %.3f", results[1].GovernanceNeutral, results[1].Reliability)
	}
	if results[0].OriginalRank != 2 || results[1].OriginalRank != 1 {
		t.Errorf("original ranks = %d, %d; want 2, 1", results[0].OriginalRank, results[1].OriginalRank)
	}
}

// TestApplyGovernanceNeverDemotesConsistentlyHelpfulEvidence is the
// regression test for issue #18: helpful labels used to lower reliability
// below an unlabelled result's 1.0, so the top result fell behind the second.
func TestApplyGovernanceNeverDemotesConsistentlyHelpfulEvidence(t *testing.T) {
	for _, n := range []int{3, 10, 50} {
		t.Run(fmt.Sprintf("%d of %d helpful", n, n), func(t *testing.T) {
			db, projectID := openPolicyTestDB(t)
			results := []Result{
				{SessionID: "top", FirstSeq: 1, LastSeq: 2},
				{SessionID: "second", FirstSeq: 3, LastSeq: 4},
			}
			addOutcome(t, db, projectID, evidenceKey(results[0]), "helpful", n)

			if err := applyGovernance(context.Background(), db, projectID, results); err != nil {
				t.Fatal(err)
			}
			if results[0].SessionID != "top" {
				t.Fatalf("order = %s, %s; want the helpful result to keep rank 1", results[0].SessionID, results[1].SessionID)
			}
			if results[0].GovernanceNeutral || results[0].Reliability != 1 {
				t.Errorf("helpful evidence = neutral %v reliability %.3f; want credible and 1.0", results[0].GovernanceNeutral, results[0].Reliability)
			}
		})
	}
}

func TestApplyGovernanceKeepsSparseEvidenceExactlyNeutral(t *testing.T) {
	db, projectID := openPolicyTestDB(t)
	results := []Result{
		{SessionID: "top", FirstSeq: 1, LastSeq: 2},
		{SessionID: "second", FirstSeq: 3, LastSeq: 4},
	}
	addOutcome(t, db, projectID, evidenceKey(results[0]), "incorrect", 1)

	if err := applyGovernance(context.Background(), db, projectID, results); err != nil {
		t.Fatal(err)
	}
	if results[0].SessionID != "top" || results[0].Reliability != 1 || !results[0].GovernanceNeutral {
		t.Errorf("sparse result = %+v; want original top result with neutral reliability", results[0])
	}
}

func TestApplyGovernanceFailureLeavesResultsUntouched(t *testing.T) {
	db, projectID := openPolicyTestDB(t)
	results := []Result{{SessionID: "top", FirstSeq: 1, LastSeq: 2}}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := applyGovernance(context.Background(), db, projectID, results); err == nil {
		t.Fatal("applyGovernance succeeded on a closed database")
	}
	if results[0].GovernanceEvaluated || results[0].OriginalRank != 0 || results[0].PolicyScore != 0 {
		t.Errorf("failed policy mutated result: %+v", results[0])
	}
}

func TestRecordRetrievalPersistsGovernanceDecision(t *testing.T) {
	db, projectID := openPolicyTestDB(t)
	got := BestResult{Results: []Result{{SessionID: "s", FirstSeq: 1, LastSeq: 2, ScoreKind: "bm25", Reliability: .7, PolicyScore: .01, GovernanceEvaluated: true, OriginalRank: 2}}}
	runID, err := recordRetrieval(context.Background(), db, "q", projectID, "", 10, "internal", 0, got)
	if err != nil {
		t.Fatal(err)
	}
	var reliability, policyScore float64
	var neutral, applied bool
	var originalRank int
	if err := db.QueryRow(`SELECT reliability,policy_score,governance_neutral,policy_applied,original_rank FROM retrieval_candidates WHERE run_id=?`, runID).Scan(&reliability, &policyScore, &neutral, &applied, &originalRank); err != nil {
		t.Fatal(err)
	}
	if reliability != .7 || policyScore != .01 || neutral || !applied || originalRank != 2 {
		t.Errorf("persisted = %.2f, %.2f, neutral=%v applied=%v original=%d", reliability, policyScore, neutral, applied, originalRank)
	}
}

func TestRecordRetrievalMarksDefaultSearchAsUnevaluated(t *testing.T) {
	db, projectID := openPolicyTestDB(t)
	got := BestResult{Results: []Result{{SessionID: "s", FirstSeq: 1, LastSeq: 2, ScoreKind: "bm25"}}}
	runID, err := recordRetrieval(context.Background(), db, "q", projectID, "", 10, "internal", 0, got)
	if err != nil {
		t.Fatal(err)
	}
	var applied bool
	var originalRank, neutral sql.NullInt64
	var reliability, policyScore sql.NullFloat64
	if err := db.QueryRow(`SELECT policy_applied,original_rank,reliability,policy_score,governance_neutral FROM retrieval_candidates WHERE run_id=?`, runID).Scan(&applied, &originalRank, &reliability, &policyScore, &neutral); err != nil {
		t.Fatal(err)
	}
	if applied || originalRank.Valid || reliability.Valid || policyScore.Valid || neutral.Valid {
		t.Errorf("default telemetry unexpectedly contains policy data: applied=%v original=%v reliability=%v score=%v neutral=%v", applied, originalRank, reliability, policyScore, neutral)
	}
}
