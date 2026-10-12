// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package governance_test

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/store"
)

// TestAssessReliability pins the formula. A perfect record must equal the
// unlabelled 1.0 (issue #18), and anything worse must fall below it.
func TestAssessReliability(t *testing.T) {
	cases := []struct {
		helpful, incorrect int
		want               float64
	}{
		{3, 0, 1},         // perfect record: no demotion
		{50, 0, 1},        // still exactly the prior, never above it
		{2, 1, 0.875},     // Z=3/8, rate 2/3: 0.625 + 0.375*2/3
		{0, 3, 0.625},     // Z=3/8, rate 0
		{9, 1, 14.0 / 15}, // Z=2/3, rate 0.9: 1/3 + 2/3*0.9
		{0, 10, 1.0 / 3},  // Z=2/3, rate 0
	}
	for _, c := range cases {
		db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Migrate(db); err != nil {
			t.Fatal(err)
		}
		res, err := db.Exec(`INSERT INTO projects(name) VALUES ('p')`)
		if err != nil {
			t.Fatal(err)
		}
		projectID, _ := res.LastInsertId()
		for outcome, n := range map[string]int{"helpful": c.helpful, "incorrect": c.incorrect} {
			for i := 0; i < n; i++ {
				if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,outcome) VALUES (?,?,?)`, projectID, "e", outcome); err != nil {
					t.Fatal(err)
				}
			}
		}
		a, err := governance.Assess(db, projectID, governance.Evidence{Key: "e"})
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if a.Neutral || math.Abs(a.Reliability-c.want) > 1e-9 {
			t.Errorf("%d helpful, %d incorrect: reliability %.4f neutral %v, want %.4f", c.helpful, c.incorrect, a.Reliability, a.Neutral, c.want)
		}
	}
}

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
	empty, err := governance.Assess(db, projectID, governance.Evidence{Key: "never-observed"})
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
	a, err := governance.Assess(db, projectID, governance.Evidence{Key: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Neutral || a.Reliability != 1 || a.Observations != 2 {
		t.Fatalf("assessment = %+v, want neutral reliability 1 with two observations", a)
	}
	if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,outcome) VALUES (?,?,?)`, projectID, "e", "incorrect"); err != nil {
		t.Fatal(err)
	}
	a, err = governance.Assess(db, projectID, governance.Evidence{Key: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Neutral || a.Reliability >= 1 || a.Reliability <= 0 {
		t.Fatalf("assessment = %+v, want a bounded non-neutral penalty", a)
	}
}
