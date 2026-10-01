// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package doctor_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/store"
)

func TestCollectReportsCorpusAndRetrievalHealth(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	project, err := db.Exec(`INSERT INTO projects(name) VALUES ('current')`)
	if err != nil {
		t.Fatal(err)
	}
	projectID, _ := project.LastInsertId()
	source, err := db.Exec(`INSERT INTO sources(agent,path,last_ingested_at,last_error) VALUES ('codex','source','2026-09-30T12:00:00Z','parse warning')`)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	if _, err := db.Exec(`INSERT INTO sources(agent,path,last_error) VALUES ('claude-code','failed-source','cannot parse')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,agent,native_id,project_id,source_id,started_at) VALUES ('s','codex','s',?,?, '2026-09-30T00:00:00Z')`, projectID, sourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(session_id,seq,role,content) VALUES ('s',0,'user','question'),('s',1,'assistant','answer')`); err != nil {
		t.Fatal(err)
	}
	chunk, err := db.Exec(`INSERT INTO chunks(session_id,first_seq,last_seq,text) VALUES ('s',0,1,'question answer')`)
	if err != nil {
		t.Fatal(err)
	}
	chunkID, _ := chunk.LastInsertId()
	if _, err := db.Exec(`INSERT INTO sessions(id,agent,native_id,started_at) VALUES ('orphan','codex','orphan','2026-09-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chunks(session_id,first_seq,last_seq,text) VALUES ('orphan',0,0,'unassigned')`); err != nil {
		t.Fatal(err)
	}
	for i, mode := range []string{"hybrid", "keyword", "keyword"} {
		runID := []string{"r1", "r2", "r3"}[i]
		resultCount := 1
		if i == 2 {
			resultCount = 0
		}
		if _, err := db.Exec(`INSERT INTO retrieval_runs(id,project_id,surface,query,requested_limit,mode,duration_ms,result_count) VALUES (?,?,'cli','q',10,?,1,?)`, runID, projectID, mode, resultCount); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO retrieval_candidates(run_id,rank,evidence_key,chunk_id,session_id,first_seq,last_seq,score,score_kind,policy_applied) VALUES (?,1,'e',?,'s',0,1,1.0,?,?)`, runID, chunkID, map[bool]string{true: "rrf", false: "bm25"}[mode == "hybrid"], i == 0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,run_id,candidate_rank,outcome) VALUES (?,'e',?,1,'helpful')`, projectID, runID); err != nil {
			t.Fatal(err)
		}
	}

	r, err := doctor.Collect(context.Background(), db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if r.ProjectName != "current" || r.Sessions != 1 || r.Messages != 2 || r.Chunks != 1 || r.PendingEmbeddings != 1 {
		t.Errorf("corpus report = %+v", r)
	}
	if r.Sources != 2 || r.SourcesWithErrors != 2 || !r.LastIngestedAt.Valid {
		t.Errorf("ingestion report = %+v", r)
	}
	if r.UnassignedChunks != 1 || r.RecentRetrievals != 3 || r.RecentZeroResultRuns != 1 || r.RecentHybridRuns != 1 || r.RecentPolicyRuns != 1 {
		t.Errorf("health report = %+v", r)
	}
	if r.Outcomes != 3 || r.RetrievedEvidence != 1 || r.ObservedEvidence != 1 || r.CredibleEvidence != 1 || r.OutcomeCoverage() != 100 {
		t.Errorf("outcome report = %+v", r)
	}
}

func TestCollectEmptyProjectHasZeroCoverage(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO projects(name) VALUES ('empty')`)
	if err != nil {
		t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	r, err := doctor.Collect(context.Background(), db, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if r.OutcomeCoverage() != 0 || r.NeutralObservedEvidence() != 0 || r.LastIngestedAt.Valid {
		t.Errorf("empty report = %+v", r)
	}
}
