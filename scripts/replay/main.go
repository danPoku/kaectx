// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command replay is a read-only development tool for issue #37. It replays
// every recorded search in a kae database twice: once under today's
// governance (a fixed prior of 1.0, neutral below three labels) and once
// under a class-level prior, and reports whether the class prior would have
// changed any result order and whether those changes agree with the labels
// agents left.
//
// It changes nothing kae ships and writes nothing: the database is opened
// with mode=ro, and the tool checks the file's size and modification time
// before and after.
//
//	go run -tags sqlite_fts5 ./scripts/replay -db /path/to/copy-of-kae.db
//
// Run it against a copy, not the live database: a copy is a fixed snapshot,
// so two runs give the same report.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	dbPath := flag.String("db", "", "path to a copy of kae.db (opened read-only)")
	project := flag.Int64("project", 0, "replay one project's searches (default: every project, each on its own labels)")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "usage: go run -tags sqlite_fts5 ./scripts/replay -db PATH [-project ID]")
		os.Exit(2)
	}
	if err := runReplay(*dbPath, *project, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

// openReadOnly opens the database so that SQLite itself refuses any write.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(abs)+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func runReplay(path string, projectID int64, w io.Writer) error {
	before, err := os.Stat(path)
	if err != nil {
		return err
	}
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	runs, err := loadRuns(db, projectID)
	if err != nil {
		db.Close()
		return fmt.Errorf("load searches: %w", err)
	}
	labels, err := loadLabels(db, projectID)
	if err != nil {
		db.Close()
		return fmt.Errorf("load labels: %w", err)
	}
	if err := db.Close(); err != nil {
		return err
	}
	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	unchanged := before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())

	writeReport(w, buildReport(runs, labels))
	fmt.Fprintf(w, "\nDatabase file unchanged by this run (size and modification time): %v\n", unchanged)
	if !unchanged {
		return fmt.Errorf("the database file changed during the replay; if it is the live database, run against a copy")
	}
	return nil
}

// rankStats accumulates the positions of labelled hits under one ordering.
type rankStats struct {
	helpfulSum, helpfulN     int
	unhelpfulSum, unhelpfulN int
}

func (s *rankStats) add(position int, helpful bool) {
	if helpful {
		s.helpfulSum += position
		s.helpfulN++
		return
	}
	s.unhelpfulSum += position
	s.unhelpfulN++
}

func mean(sum, n int) string {
	if n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(sum)/float64(n))
}

// classRow is one class's line in the report.
type classRow struct {
	class         classKey
	labels        tally // every label in the class, as of the end of the data
	used          int   // hits whose prior came from this class with n > 0
	usedEmpty     int   // hits in this class scored while it had no labels yet
	sizesWhenUsed []int
}

type report struct {
	searches, candidates  int
	labels, classable     int
	helpful               int
	riskAware             int // recorded searches that had reranking applied
	riskAwareReproduced   int // of those, baseline replay gives the stored order
	riskAwareMismatches   []string
	baselineChanged       int // searches whose baseline order differs from relevance order
	classChanged          int
	baselineMoved         int // hits at a different position than relevance order
	classMoved            int
	labelledSearches      int
	labelledHits          int
	relevance, base, clsS rankStats
	classes               []classRow
	changedExamples       []string
}

func position(order []int, cand int) int {
	for i, c := range order {
		if c == cand {
			return i + 1
		}
	}
	return 0
}

func moved(a, b []int) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

func buildReport(runs []run, labels []label) report {
	var rep report
	rep.searches = len(runs)
	rep.labels = len(labels)
	// Labels by the hit they were given to, to find a search's own labels.
	type hitKey struct {
		run  string
		rank int
	}
	own := map[hitKey]label{}
	classTotals := map[classKey]*classRow{}
	rowFor := func(c classKey) *classRow {
		r := classTotals[c]
		if r == nil {
			r = &classRow{class: c}
			classTotals[c] = r
		}
		return r
	}
	for _, l := range labels {
		if l.helpful {
			rep.helpful++
		}
		if l.hasClass {
			rep.classable++
			rowFor(l.class).labels.add(l.helpful)
		}
		if l.runID.Valid && l.rank.Valid {
			own[hitKey{l.runID.String, int(l.rank.Int64)}] = l
		}
	}

	for _, r := range runs {
		rep.candidates += len(r.cands)
		got := replay(r, labels)

		policy := false
		for _, c := range r.cands {
			policy = policy || c.policy
		}
		if policy {
			rep.riskAware++
			if sameOrder(got.baseline, storedOrder(r)) {
				rep.riskAwareReproduced++
			} else {
				rep.riskAwareMismatches = append(rep.riskAwareMismatches, r.id)
			}
		}
		if !sameOrder(got.baseline, got.relevant) {
			rep.baselineChanged++
			rep.baselineMoved += moved(got.baseline, got.relevant)
		}
		if !sameOrder(got.class, got.relevant) {
			rep.classChanged++
			rep.classMoved += moved(got.class, got.relevant)
			if len(rep.changedExamples) < 10 {
				rep.changedExamples = append(rep.changedExamples,
					fmt.Sprintf("%s (%s): relevance order %s became %s", r.id[:minInt(8, len(r.id))], r.createdAt[:10],
						ranksOf(r, got.relevant), ranksOf(r, got.class)))
			}
		}
		for _, use := range got.classUsed {
			row := rowFor(use.class)
			if use.n == 0 {
				row.usedEmpty++
			} else {
				row.used++
				row.sizesWhenUsed = append(row.sizesWhenUsed, use.n)
			}
		}

		labelled := false
		for i, c := range r.cands {
			l, ok := own[hitKey{r.id, c.storedRank}]
			if !ok {
				continue
			}
			labelled = true
			rep.labelledHits++
			rep.relevance.add(position(got.relevant, i), l.helpful)
			rep.base.add(position(got.baseline, i), l.helpful)
			rep.clsS.add(position(got.class, i), l.helpful)
		}
		if labelled {
			rep.labelledSearches++
		}
	}
	for _, row := range classTotals {
		rep.classes = append(rep.classes, *row)
	}
	sort.Slice(rep.classes, func(i, j int) bool { return rep.classes[i].class.String() < rep.classes[j].class.String() })
	return rep
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ranksOf prints an order as the relevance ranks of its hits, e.g. "2,1,3".
func ranksOf(r run, order []int) string {
	parts := make([]string, len(order))
	for i, c := range order {
		parts[i] = fmt.Sprint(r.cands[c].baseRank)
	}
	return strings.Join(parts, ",")
}

func sizeRange(sizes []int) string {
	if len(sizes) == 0 {
		return "never"
	}
	s := append([]int(nil), sizes...)
	sort.Ints(s)
	return fmt.Sprintf("%d / %d / %d", s[0], s[len(s)/2], s[len(s)-1])
}

func writeReport(w io.Writer, r report) {
	fmt.Fprintln(w, "## Replay of recorded searches: baseline vs class-level prior")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- Searches replayed: %d (%d hits)\n", r.searches, r.candidates)
	fmt.Fprintf(w, "- Per-hit labels: %d (%d helpful, %d not), of which %d can be put in a class\n",
		r.labels, r.helpful, r.labels-r.helpful, r.classable)
	fmt.Fprintln(w, "- No look-ahead: each search is replayed with only the labels created before it ran.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "### Does the baseline replay match what was stored?")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Recorded risk-aware searches: %d. Baseline replay reproduces the stored order for %d of them.\n",
		r.riskAware, r.riskAwareReproduced)
	for _, id := range r.riskAwareMismatches {
		fmt.Fprintf(w, "- not reproduced: run %s\n", id)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "### Changes in order")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Method | Searches reordered | Hits at a new position |")
	fmt.Fprintln(w, "|---|---|---|")
	fmt.Fprintf(w, "| Baseline (fixed 1.0 prior) | %d of %d | %d |\n", r.baselineChanged, r.searches, r.baselineMoved)
	fmt.Fprintf(w, "| Class prior | %d of %d | %d |\n", r.classChanged, r.searches, r.classMoved)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "### Do the changes agree with the labels?")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Labelled searches: %d (%d labelled hits). Mean position of the labelled hits under each order; a good method moves helpful hits up (smaller) and the others down (larger).\n",
		r.labelledSearches, r.labelledHits)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Order | Helpful hits (n) | Mean position | Not-helpful hits (n) | Mean position |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, row := range []struct {
		name string
		s    rankStats
	}{{"Relevance only", r.relevance}, {"Baseline", r.base}, {"Class prior", r.clsS}} {
		fmt.Fprintf(w, "| %s | %d | %s | %d | %s |\n", row.name, row.s.helpfulN, mean(row.s.helpfulSum, row.s.helpfulN),
			row.s.unhelpfulN, mean(row.s.unhelpfulSum, row.s.unhelpfulN))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "### Classes")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Class = agent / relevance-rank band / parent or subagent session. \"Size when used\" is the number of labels the class held at the moment it supplied a prior (min / median / max).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Class | Labels now | Helpful rate now | Hits it priced | Size when used | Hits scored before it had labels |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|")
	for _, c := range r.classes {
		rate := "n/a"
		if c.labels.n > 0 {
			rate = fmt.Sprintf("%.2f", c.labels.rate())
		}
		fmt.Fprintf(w, "| %s | %d | %s | %d | %s | %d |\n", c.class, c.labels.n, rate, c.used, sizeRange(c.sizesWhenUsed), c.usedEmpty)
	}
	if len(r.changedExamples) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "### First reorderings under the class prior")
		fmt.Fprintln(w)
		for _, e := range r.changedExamples {
			fmt.Fprintln(w, "- "+e)
		}
	}
}
