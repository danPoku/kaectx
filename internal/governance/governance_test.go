// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package governance_test

import (
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/store"
)

func TestAssessStaysNeutralUntilEvidenceIsCredible(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO projects(name) VALUES ('p')`)
	if err != nil {
		t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	empty, err := governance.Assess(db, projectID, "never-observed")
	if err != nil {
		t.Fatalf("empty assessment: %v", err)
	}
	if !empty.Neutral || empty.Observations != 0 || empty.Reliability != 1 {
		t.Fatalf("empty assessment = %+v, want neutral", empty)
	}
	for _, outcome := range []string{"incorrect", "helpful"} {
		if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,outcome) VALUES (?,?,?)`, projectID, "e", outcome); err != nil {
			t.Fatal(err)
		}
	}
	a, err := governance.Assess(db, projectID, "e")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Neutral || a.Reliability != 1 || a.Observations != 2 {
		t.Fatalf("assessment = %+v, want neutral reliability 1 with two observations", a)
	}
	if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,outcome) VALUES (?,?,?)`, projectID, "e", "incorrect"); err != nil {
		t.Fatal(err)
	}
	a, err = governance.Assess(db, projectID, "e")
	if err != nil {
		t.Fatal(err)
	}
	if a.Neutral || a.Reliability >= 1 || a.Reliability <= 0 {
		t.Fatalf("assessment = %+v, want a bounded non-neutral penalty", a)
	}
}
