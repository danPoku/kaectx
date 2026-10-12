// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package governance_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/store"
)

func anchorTestDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO projects(name) VALUES ('p')`)
	if err != nil {
		t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	return db, projectID
}

// label stores n outcomes for the exchange that, when it was labelled,
// spanned session s from first to last.
func label(t *testing.T, db *sql.DB, projectID int64, s string, first, last int, outcome string, n int) {
	t.Helper()
	key := store.EvidenceKey(s, first, last)
	for i := 0; i < n; i++ {
		if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,session_id,anchor_seq,outcome)
			VALUES (?,?,?,?,?)`, projectID, key, s, first, outcome); err != nil {
			t.Fatal(err)
		}
	}
}

func ev(s string, first, last int) governance.Evidence {
	return governance.Evidence{Key: store.EvidenceKey(s, first, last), SessionID: s, FirstSeq: first, LastSeq: last}
}

// TestAssessFollowsLabelsAcrossRechunking is the regression test for issue
// #19: labels were matched by a hash of the exact chunk range, so a rebuilt
// chunk with new bounds lost every label it had.
func TestAssessFollowsLabelsAcrossRechunking(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, db *sql.DB, p int64)
		now   governance.Evidence
		want  int
	}{
		{
			// A settled tail was chunked on its own; a rebuild folds it into
			// the turn before, so that turn's last_seq grows.
			name:  "range grows",
			setup: func(t *testing.T, db *sql.DB, p int64) { label(t, db, p, "s", 5, 8, "incorrect", 3) },
			now:   ev("s", 5, 12),
			want:  3,
		},
		{
			// repair reclassified the turn at 9 as injected text, so the two
			// chunks merge; the merged chunk carries both sets of labels.
			name: "two chunks merge",
			setup: func(t *testing.T, db *sql.DB, p int64) {
				label(t, db, p, "s", 5, 8, "incorrect", 2)
				label(t, db, p, "s", 9, 12, "incorrect", 1)
			},
			now:  ev("s", 5, 12),
			want: 3,
		},
		{
			// The reverse reclassification splits a chunk. The labels stay
			// with the part holding the original question.
			name:  "split keeps labels with the first part",
			setup: func(t *testing.T, db *sql.DB, p int64) { label(t, db, p, "s", 5, 12, "incorrect", 3) },
			now:   ev("s", 5, 8),
			want:  3,
		},
		{
			name:  "split leaves the second part unlabelled",
			setup: func(t *testing.T, db *sql.DB, p int64) { label(t, db, p, "s", 5, 12, "incorrect", 3) },
			now:   ev("s", 9, 12),
			want:  0,
		},
		{
			name:  "another session's labels never match",
			setup: func(t *testing.T, db *sql.DB, p int64) { label(t, db, p, "other", 5, 8, "incorrect", 3) },
			now:   ev("s", 5, 8),
			want:  0,
		},
		{
			// Labels whose anchor could not be recovered still match their
			// exact original range.
			name: "unanchored legacy label matches its exact key",
			setup: func(t *testing.T, db *sql.DB, p int64) {
				for i := 0; i < 3; i++ {
					if _, err := db.Exec(`INSERT INTO memory_observations(project_id,evidence_key,outcome) VALUES (?,?,?)`,
						p, store.EvidenceKey("s", 5, 8), "incorrect"); err != nil {
						t.Fatal(err)
					}
				}
			},
			now:  ev("s", 5, 8),
			want: 3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, p := anchorTestDB(t)
			c.setup(t, db, p)
			one, err := governance.Assess(db, p, c.now)
			if err != nil {
				t.Fatal(err)
			}
			many, err := governance.AssessMany(context.Background(), db, p, []governance.Evidence{c.now, c.now})
			if err != nil {
				t.Fatal(err)
			}
			if one.Observations != c.want || many[c.now.Key].Observations != c.want {
				t.Errorf("observations: Assess %d, AssessMany %d; want %d", one.Observations, many[c.now.Key].Observations, c.want)
			}
		})
	}
}
