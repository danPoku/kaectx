// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/danPoku/kaectx/internal/governance"
)

// candidate is one hit a recorded search returned.
type candidate struct {
	storedRank int // position the search actually returned it in
	baseRank   int // position before any reranking: the relevance order
	key        string
	sessionID  string
	firstSeq   int
	lastSeq    int
	agent      string
	subagent   bool // the hit's session has a parent session
	policy     bool // risk-aware reranking was applied when it was recorded
}

// run is one recorded search.
type run struct {
	id        string
	projectID int64
	createdAt string
	cands     []candidate
}

// label is one recorded per-hit outcome.
type label struct {
	projectID int64
	key       string
	sessionID sql.NullString
	anchorSeq sql.NullInt64
	runID     sql.NullString
	rank      sql.NullInt64
	helpful   bool
	createdAt string
	// class is the class of the hit the label was given to, when the run it
	// came from is still on record. A label whose run was pruned cannot be
	// classed and counts only toward its own evidence.
	class    classKey
	hasClass bool
}

// classKey groups hits that tend to fail or succeed together. One exchange
// rarely collects enough labels to say anything about itself, but "a
// subagent's prompt shown at rank 1" is a kind of hit that collects labels
// across many searches. Like pricing a new driver from their age band and
// car group because they have no claims record of their own yet.
type classKey struct {
	agent    string
	bucket   string // rank bucket: "1", "2-3", "4+"
	subagent bool
}

func (c classKey) String() string {
	kind := "parent"
	if c.subagent {
		kind = "subagent"
	}
	return fmt.Sprintf("%s / rank %s / %s", c.agent, c.bucket, kind)
}

// rankBucket puts a relevance rank into the issue's three bands. Ranks past
// 10 (a search asked for with a larger limit) join the last band.
func rankBucket(rank int) string {
	switch {
	case rank <= 1:
		return "1"
	case rank <= 3:
		return "2-3"
	default:
		return "4+"
	}
}

func classOf(c candidate) classKey {
	return classKey{agent: c.agent, bucket: rankBucket(c.baseRank), subagent: c.subagent}
}

// matches is governance's rule for which labels count toward a piece of
// evidence (see governance.matchObservations), restated over rows already in
// memory: a label anchored inside the exchange's range counts, and a label
// with no anchor counts when its evidence key is the same.
func (l label) matches(c candidate, projectID int64) bool {
	if l.projectID != projectID {
		return false
	}
	if l.anchorSeq.Valid {
		return l.sessionID.Valid && l.sessionID.String == c.sessionID &&
			int(l.anchorSeq.Int64) >= c.firstSeq && int(l.anchorSeq.Int64) <= c.lastSeq
	}
	return l.key == c.key
}

// tally counts labels.
type tally struct{ n, helpful int }

func (t *tally) add(helpful bool) {
	t.n++
	if helpful {
		t.helpful++
	}
}

func (t tally) rate() float64 { return float64(t.helpful) / float64(t.n) }

// baselineReliability is today's governance: the hit's own helpful rate
// blended with a fixed prior of 1.0, and neutral until it has
// MinCredibleObservations labels. It mirrors governance.finalize; the test
// TestBaselineMatchesGovernance holds the two together.
func baselineReliability(own tally) float64 {
	if own.n < governance.MinCredibleObservations {
		return 1
	}
	z := float64(own.n) / (float64(own.n) + governance.CredibilityK)
	return (1 - z) + z*own.rate()
}

// classReliability is the proposal: Z x the hit's own helpful rate plus
// (1 - Z) x its class's helpful rate. There is no minimum label count: with
// no labels of its own a hit takes its class's rate outright. A class with no
// labels yet has nothing to say, so the prior falls back to 1.0 and the hit
// is treated exactly as the baseline treats an unlabelled hit.
func classReliability(own, class tally) float64 {
	prior := 1.0
	if class.n > 0 {
		prior = class.rate()
	}
	if own.n == 0 {
		return prior
	}
	z := float64(own.n) / (float64(own.n) + governance.CredibilityK)
	return z*own.rate() + (1-z)*prior
}

// rerank orders candidates by reliability / (60 + relevance rank), the same
// utility search.applyGovernance uses, and returns the candidates' positions
// in the new order. Ties keep relevance order.
func rerank(cands []candidate, reliability []float64) []int {
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	// Start from relevance order so the stable sort breaks ties by it.
	sort.SliceStable(idx, func(a, b int) bool { return cands[idx[a]].baseRank < cands[idx[b]].baseRank })
	score := func(i int) float64 { return reliability[i] / float64(60+cands[i].baseRank) }
	sort.SliceStable(idx, func(a, b int) bool { return score(idx[a]) > score(idx[b]) })
	return idx
}

// replayed is one search replayed under every method.
type replayed struct {
	run      run
	relevant []int // relevance order: the order with no reranking at all
	baseline []int
	class    []int
	// classUsed records, for each hit, the class tally that fed its prior.
	classUsed []classUse
}

type classUse struct {
	class classKey
	n     int
}

// replay rebuilds one search's order under both methods using only the labels
// created strictly before the search ran. That is the no-look-ahead rule: a
// label left after a search could not have influenced it, and letting it
// would flatter whichever method leans hardest on labels.
func replay(r run, labels []label) replayed {
	var known []label
	for _, l := range labels {
		if l.createdAt < r.createdAt {
			known = append(known, l)
		}
	}
	classes := map[classKey]*tally{}
	for _, l := range known {
		if l.projectID != r.projectID || !l.hasClass {
			continue
		}
		t := classes[l.class]
		if t == nil {
			t = &tally{}
			classes[l.class] = t
		}
		t.add(l.helpful)
	}

	base := make([]float64, len(r.cands))
	cls := make([]float64, len(r.cands))
	neutral := make([]float64, len(r.cands))
	out := replayed{run: r, classUsed: make([]classUse, len(r.cands))}
	for i, c := range r.cands {
		var own tally
		for _, l := range known {
			if l.matches(c, r.projectID) {
				own.add(l.helpful)
			}
		}
		var class tally
		if t := classes[classOf(c)]; t != nil {
			class = *t
		}
		neutral[i] = 1
		base[i] = baselineReliability(own)
		cls[i] = classReliability(own, class)
		out.classUsed[i] = classUse{class: classOf(c), n: class.n}
	}
	out.relevant = rerank(r.cands, neutral)
	out.baseline = rerank(r.cands, base)
	out.class = rerank(r.cands, cls)
	return out
}

// sameOrder reports whether two orders are identical.
func sameOrder(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// storedOrder is the order the search really returned.
func storedOrder(r run) []int {
	idx := make([]int, len(r.cands))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return r.cands[idx[a]].storedRank < r.cands[idx[b]].storedRank })
	return idx
}

// loadRuns reads every recorded search with its candidates, oldest first.
// projectID 0 means every project.
func loadRuns(db *sql.DB, projectID int64) ([]run, error) {
	rows, err := db.Query(`SELECT rr.id, rr.project_id, rr.created_at,
			rc.rank, COALESCE(rc.original_rank, rc.rank), rc.evidence_key, rc.session_id,
			rc.first_seq, rc.last_seq, COALESCE(s.agent, 'unknown'),
			s.parent_session_id IS NOT NULL, rc.policy_applied
		FROM retrieval_runs rr
		JOIN retrieval_candidates rc ON rc.run_id = rr.id
		LEFT JOIN sessions s ON s.id = rc.session_id
		WHERE (? = 0 OR rr.project_id = ?)
		ORDER BY rr.created_at, rr.rowid, rc.rank`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []run
	for rows.Next() {
		var id, createdAt string
		var project int64
		var c candidate
		if err := rows.Scan(&id, &project, &createdAt, &c.storedRank, &c.baseRank, &c.key, &c.sessionID,
			&c.firstSeq, &c.lastSeq, &c.agent, &c.subagent, &c.policy); err != nil {
			return nil, err
		}
		if len(runs) == 0 || runs[len(runs)-1].id != id {
			runs = append(runs, run{id: id, projectID: project, createdAt: createdAt})
		}
		last := &runs[len(runs)-1]
		last.cands = append(last.cands, c)
	}
	return runs, rows.Err()
}

// loadLabels reads every per-hit outcome and classes it by the hit it was
// given to. projectID 0 means every project.
func loadLabels(db *sql.DB, projectID int64) ([]label, error) {
	rows, err := db.Query(`SELECT mo.project_id, mo.evidence_key, mo.session_id, mo.anchor_seq,
			mo.run_id, mo.candidate_rank, mo.outcome = 'helpful', mo.created_at,
			rc.run_id IS NOT NULL, COALESCE(s.agent, 'unknown'),
			COALESCE(rc.original_rank, rc.rank, 0), s.parent_session_id IS NOT NULL
		FROM memory_observations mo
		LEFT JOIN retrieval_candidates rc ON rc.run_id = mo.run_id AND rc.rank = mo.candidate_rank
		LEFT JOIN sessions s ON s.id = rc.session_id
		WHERE (? = 0 OR mo.project_id = ?)
		ORDER BY mo.created_at, mo.id`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var labels []label
	for rows.Next() {
		var l label
		var agent string
		var baseRank int
		var subagent bool
		if err := rows.Scan(&l.projectID, &l.key, &l.sessionID, &l.anchorSeq, &l.runID, &l.rank,
			&l.helpful, &l.createdAt, &l.hasClass, &agent, &baseRank, &subagent); err != nil {
			return nil, err
		}
		if l.hasClass {
			l.class = classKey{agent: agent, bucket: rankBucket(baseRank), subagent: subagent}
		}
		labels = append(labels, l)
	}
	return labels, rows.Err()
}
