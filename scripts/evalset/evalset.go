// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/search"
)

// topK is the cut-off both measures use: the default page of results an agent
// sees. A right answer at rank 11 is, for the agent, a miss.
const topK = 10

// searchDepth is how far down each search is read. Scoring uses only the top
// K; the rest is there so a failing case can say whether the right answer was
// just out of sight (rank 12) or nowhere (not in the top 50). Those call for
// different fixes. The fused query ranks at most 50 keyword and 50 semantic
// candidates, so asking for more would add nothing.
const searchDepth = 50

// caseFile is the on-disk evaluation set.
type caseFile struct {
	Version int        `json:"version"`
	Cases   []evalCase `json:"cases"`
}

// evalCase is one query with a known right answer. Like a past exam paper
// with its marking scheme: the question is a query someone really asked, and
// the expected ranges are the exchanges that really answered it.
type evalCase struct {
	ID string `json:"id"`
	// Source says where the case came from, so results can be read per kind:
	// helpful_label (a hit an agent used and labelled helpful), search_outcome
	// (a whole-search verdict), miss (the right session was found some other
	// way), empty (the search returned nothing), stale (the hit was right
	// once and has been overtaken).
	Source string `json:"source"`
	// Checked says who vouches for the expected answer: "hand" when a person
	// read the transcript, "label" when it rests on the agent's own label.
	Checked string `json:"checked"`
	// Project names the project to search: its git remote when it has one
	// (stable across machines), otherwise its name.
	Project       string `json:"project,omitempty"`
	ProjectRemote string `json:"project_remote,omitempty"`
	Query         string `json:"query"`
	Agent         string `json:"agent,omitempty"`
	// Since and Until are the date window the query was asked with, in the
	// forms search_context accepts. About half of real searches carry one,
	// and a windowed search replayed without its window is a different
	// search: the right answer can sit at rank 5 inside the window and rank
	// 35 outside it.
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Expected lists the exchanges that answer the query. A result counts as
	// one of them when it is from the same session and its message range
	// overlaps, so a chunk rebuilt with slightly different bounds still counts.
	Expected []expectedRange `json:"expected,omitempty"`
	// ExpectNone marks a query with no right answer in the corpus: the correct
	// result is an empty one. It is scored apart from the recall figures.
	ExpectNone bool   `json:"expect_none,omitempty"`
	Note       string `json:"note,omitempty"`
}

type expectedRange struct {
	SessionID string `json:"session_id"`
	FirstSeq  int    `json:"first_seq"`
	LastSeq   int    `json:"last_seq"`
}

func (e expectedRange) covers(r search.Result) bool {
	return r.SessionID == e.SessionID && r.FirstSeq <= e.LastSeq && r.LastSeq >= e.FirstSeq
}

func loadCases(path string) ([]evalCase, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f caseFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, c := range f.Cases {
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("%s: case %d has no id", path, i)
		case seen[c.ID]:
			return nil, fmt.Errorf("%s: duplicate case id %q", path, c.ID)
		case c.Query == "":
			return nil, fmt.Errorf("%s: case %s has no query", path, c.ID)
		case windowError(c) != nil:
			return nil, fmt.Errorf("%s: case %s: %w", path, c.ID, windowError(c))
		case c.ExpectNone == (len(c.Expected) > 0):
			return nil, fmt.Errorf("%s: case %s must have expected ranges or expect_none, not both and not neither", path, c.ID)
		}
		seen[c.ID] = true
	}
	return f.Cases, nil
}

func windowError(c evalCase) error {
	_, err := search.ParseWindow(c.Since, c.Until)
	return err
}

// score is one case's result under one search mode.
type score struct {
	// recall is the share of the expected exchanges found in the top K.
	recall float64
	// reciprocalRank is 1/rank of the first expected exchange in the top K,
	// or 0 when none is there.
	reciprocalRank float64
	firstRank      int // 0 when not found
	returned       int
	// deepRank is the rank of the first expected exchange anywhere in the
	// searchDepth results read, or 0 when it is not among them.
	deepRank int
}

func scoreResults(c evalCase, results []search.Result) score {
	deepRank := 0
	for i, r := range results {
		for _, e := range c.Expected {
			if e.covers(r) {
				deepRank = i + 1
				break
			}
		}
		if deepRank > 0 {
			break
		}
	}
	if len(results) > topK {
		results = results[:topK]
	}
	s := score{returned: len(results), deepRank: deepRank}
	found := 0
	for _, e := range c.Expected {
		for _, r := range results {
			if e.covers(r) {
				found++
				break
			}
		}
	}
	if len(c.Expected) > 0 {
		s.recall = float64(found) / float64(len(c.Expected))
	}
	for i, r := range results {
		matched := false
		for _, e := range c.Expected {
			if e.covers(r) {
				matched = true
				break
			}
		}
		if matched {
			s.firstRank = i + 1
			s.reciprocalRank = 1 / float64(i+1)
			break
		}
	}
	return s
}

// outcome is everything measured for one case.
type outcome struct {
	c         evalCase
	normal    score
	riskAware score
	hybrid    bool   // the fused keyword+semantic query ran
	skipped   string // why the case could not be run, if it could not
}

// resolveProject finds the case's project in this database: by git remote
// first, because project names repeat (two checkouts of one repo) and a
// remote does not.
func resolveProject(db *sql.DB, c evalCase) (int64, error) {
	var id int64
	if c.ProjectRemote != "" {
		err := db.QueryRow(`SELECT id FROM projects WHERE git_remote = ?`, c.ProjectRemote).Scan(&id)
		if err == nil {
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	if c.Project == "" {
		return 0, fmt.Errorf("no project with remote %q", c.ProjectRemote)
	}
	// Among projects sharing a name, the one with the most sessions is the
	// real one; the rest are leftovers of a split.
	err := db.QueryRow(`SELECT p.id FROM projects p WHERE p.name = ?
		ORDER BY (SELECT COUNT(*) FROM sessions s WHERE s.project_id = p.id) DESC, p.id LIMIT 1`, c.Project).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no project named %q", c.Project)
	}
	return id, err
}

// evaluate runs every case in both modes. Searches go through search.BestIn
// and search.Rerank, the untracked paths, so the database is never written.
func evaluate(ctx context.Context, db *sql.DB, client embed.Embedder, cases []evalCase) []outcome {
	out := make([]outcome, 0, len(cases))
	for _, c := range cases {
		o := outcome{c: c}
		projectID, err := resolveProject(db, c)
		if err != nil {
			o.skipped = err.Error()
			out = append(out, o)
			continue
		}
		// loadCases has already checked that the window parses.
		window, _ := search.ParseWindow(c.Since, c.Until)
		got, err := search.BestIn(ctx, db, client, c.Query, projectID, c.Agent, window, searchDepth)
		if err != nil {
			o.skipped = "search failed: " + err.Error()
			out = append(out, o)
			continue
		}
		o.hybrid = got.UsedHybrid
		o.normal = scoreResults(c, got.Results)

		reranked := append([]search.Result(nil), got.Results...)
		if err := search.Rerank(ctx, db, projectID, reranked); err != nil {
			o.skipped = "risk-aware rerank failed: " + err.Error()
			out = append(out, o)
			continue
		}
		o.riskAware = scoreResults(c, reranked)
		out = append(out, o)
	}
	return out
}

// summary is the headline numbers for a set of outcomes.
type summary struct {
	cases                     int
	recallNormal, recallRisk  float64
	mrrNormal, mrrRisk        float64
	foundNormal, foundRisk    int // cases with at least one expected exchange in the top K
	emptyCases                int
	emptyCorrect              int // expect_none cases that returned nothing
	windowed                  int // cases replayed with a date window
	skipped                   int
	keywordOnly               int // cases where the semantic half did not run
	failingNormal, failingRsk []outcome
}

func summarise(outcomes []outcome) summary {
	var s summary
	for _, o := range outcomes {
		if o.skipped != "" {
			s.skipped++
			continue
		}
		if !o.hybrid {
			s.keywordOnly++
		}
		if o.c.Since != "" || o.c.Until != "" {
			s.windowed++
		}
		if o.c.ExpectNone {
			s.emptyCases++
			if o.normal.returned == 0 {
				s.emptyCorrect++
			}
			continue
		}
		s.cases++
		s.recallNormal += o.normal.recall
		s.recallRisk += o.riskAware.recall
		s.mrrNormal += o.normal.reciprocalRank
		s.mrrRisk += o.riskAware.reciprocalRank
		if o.normal.firstRank > 0 {
			s.foundNormal++
		} else {
			s.failingNormal = append(s.failingNormal, o)
		}
		if o.riskAware.firstRank > 0 {
			s.foundRisk++
		} else {
			s.failingRsk = append(s.failingRsk, o)
		}
	}
	if s.cases > 0 {
		n := float64(s.cases)
		s.recallNormal /= n
		s.recallRisk /= n
		s.mrrNormal /= n
		s.mrrRisk /= n
	}
	return s
}

// bySource splits outcomes by case source, in a fixed order.
func bySource(outcomes []outcome) ([]string, map[string][]outcome) {
	groups := map[string][]outcome{}
	for _, o := range outcomes {
		groups[o.c.Source] = append(groups[o.c.Source], o)
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, groups
}
