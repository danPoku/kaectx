// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"fmt"
	"strings"
	"sync"
)

// servedRunsKept bounds the per-process memory of searches. A session that
// runs more searches than this without ever recording an outcome has stopped
// answering nudges; remembering further back would not change that.
const servedRunsKept = 200

// servedRun is one search this process answered.
type servedRun struct {
	id    string
	query string
}

// servedRuns remembers the searches this `ctx mcp` process served, oldest
// first. It is deliberately in memory, not a query over retrieval_runs: each
// agent session spawns its own server process, so "what this process served"
// is the closest thing ctx has to "what this agent searched". Asking agent A
// for a verdict on agent B's search would get a guess, and a guessed verdict
// is worse than none. Like a waiter who asks about the dishes they carried
// to the table, not every dish the kitchen sent out that night.
type servedRuns struct {
	mu   sync.Mutex
	runs []servedRun
}

func (r *servedRuns) add(id, query string) {
	if id == "" {
		return // telemetry failed: there is no run to attach an outcome to
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, servedRun{id: id, query: query})
	if over := len(r.runs) - servedRunsKept; over > 0 {
		r.runs = append([]servedRun(nil), r.runs[over:]...)
	}
}

func (r *servedRuns) snapshot() []servedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]servedRun(nil), r.runs...)
}

// forget drops runs that no longer need asking about, so later calls stop
// re-checking them.
func (r *servedRuns) forget(done map[string]bool) {
	if len(done) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]servedRun, 0, len(r.runs))
	for _, run := range r.runs {
		if !done[run.id] {
			kept = append(kept, run)
		}
	}
	r.runs = kept
}

// pendingSearch is one search still waiting for a whole-search outcome.
type pendingSearch struct {
	RunID string `json:"run_id"`
	Query string `json:"query"`
	// NextStep is the record_search_outcome call for this run, for the same
	// reason search hits carry a next_step: a call copied from a result is
	// made far more reliably than one assembled from a tool description.
	NextStep string `json:"next_step"`
}

// searchOutcomeCall writes the record_search_outcome call for a run. The
// outcome is the caller's judgement, so the call lists the choices rather
// than pre-filling one.
func searchOutcomeCall(runID string) string {
	if runID == "" {
		return ""
	}
	return fmt.Sprintf(`record_search_outcome(run_id=%q, outcome="helpful" | "partial" | "unhelpful")`, runID)
}

// pendingSearches lists the searches this process served that have no
// whole-search outcome yet, newest first, up to limit.
func (s *server) pendingSearches(limit int) ([]pendingSearch, error) {
	out := []pendingSearch{}
	runs := s.served.snapshot()
	if len(runs) == 0 || limit <= 0 {
		return out, nil
	}

	// One query for the whole set: which of these runs still exist, and which
	// already have a verdict. A pruned run can no longer take an outcome
	// (record_search_outcome would reject it), so it is dropped, not nagged.
	marks := strings.TrimSuffix(strings.Repeat("?,", len(runs)), ",")
	args := make([]any, len(runs))
	for i, run := range runs {
		args[i] = run.id
	}
	rows, err := s.db.Query(`SELECT rr.id, EXISTS (
			SELECT 1 FROM retrieval_run_observations o WHERE o.run_id = rr.id)
		FROM retrieval_runs rr WHERE rr.id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exists := map[string]bool{}
	answered := map[string]bool{}
	for rows.Next() {
		var id string
		var has bool
		if err := rows.Scan(&id, &has); err != nil {
			return nil, err
		}
		exists[id] = true
		answered[id] = has
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	done := map[string]bool{}
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		if !exists[run.id] || answered[run.id] {
			done[run.id] = true
			continue
		}
		if len(out) < limit {
			out = append(out, pendingSearch{RunID: run.id, Query: run.query, NextStep: searchOutcomeCall(run.id)})
		}
	}
	s.served.forget(done)
	return out, nil
}
