// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command evalset is a read-only development tool for issue #43. It runs a
// fixed set of real queries with known right answers against a copy of a ctx
// database and reports recall@10 and mean reciprocal rank, for normal and for
// risk-aware search, so a change to ranking or governance can be shown to
// help or hurt before it ships.
//
//	go run -tags sqlite_fts5 ./scripts/evalset -db /path/to/copy-of-ctx.db -cases cases.json
//
// The case file holds real queries and session IDs from the machine it was
// built on, so it is private to that machine: keep it out of the repository.
// testdata/example-cases.json shows the format with made-up content. Use
// -public to print a report with case IDs but no query text, fit to post.
//
// The database is opened with mode=ro, searches run through the untracked
// search paths (no telemetry row is written), and the file's size and
// modification time are checked before and after.
//
// Semantic search needs Ollama running with the embedding model the database
// was built with. Without it every query falls back to keyword search, and
// the report says how many did: numbers from a keyword-only run are not
// comparable with numbers from a hybrid one.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/danPoku/kaectx/internal/embed"
	// Imported for its init, which registers sqlite-vec on every connection;
	// without it the fused query cannot read chunk_vectors.
	_ "github.com/danPoku/kaectx/internal/store"
)

func main() {
	dbPath := flag.String("db", "", "path to a copy of ctx.db (opened read-only)")
	casesPath := flag.String("cases", "", "path to the case file (JSON)")
	ollama := flag.String("ollama", embed.DefaultBaseURL, "Ollama base URL for the query embedding")
	keywordOnly := flag.Bool("keyword-only", false, "skip the semantic half; do not compare with a hybrid run")
	public := flag.Bool("public", false, "leave query text out of the report, listing failing cases by ID only")
	flag.Parse()
	if *dbPath == "" || *casesPath == "" {
		fmt.Fprintln(os.Stderr, "usage: go run -tags sqlite_fts5 ./scripts/evalset -db PATH -cases FILE [-ollama URL] [-keyword-only] [-public]")
		os.Exit(2)
	}
	var client embed.Embedder
	if !*keywordOnly {
		client = embed.NewOllamaClient(*ollama, embed.DefaultModel)
	}
	if err := runEval(*dbPath, *casesPath, client, *public, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "evalset:", err)
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

func runEval(dbPath, casesPath string, client embed.Embedder, public bool, w io.Writer) error {
	cases, err := loadCases(casesPath)
	if err != nil {
		return err
	}
	before, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return err
	}
	outcomes := evaluate(context.Background(), db, client, cases)
	if err := db.Close(); err != nil {
		return err
	}
	after, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	unchanged := before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())

	writeReport(w, outcomes, public)
	fmt.Fprintf(w, "\nDatabase file unchanged by this run (size and modification time): %v\n", unchanged)
	if !unchanged {
		return fmt.Errorf("the database file changed during the run; if it is the live database, run against a copy")
	}
	return nil
}

func writeReport(w io.Writer, outcomes []outcome, public bool) {
	all := summarise(outcomes)
	fmt.Fprintln(w, "## Search evaluation")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- Cases: %d with a known answer, %d expecting no result, %d skipped\n", all.cases, all.emptyCases, all.skipped)
	if run := all.cases + all.emptyCases; all.keywordOnly == run {
		fmt.Fprintln(w, "- Search mode: keyword only (the semantic half did not run for any case)")
	} else if all.keywordOnly > 0 {
		fmt.Fprintf(w, "- Search mode: hybrid, but %d of %d cases fell back to keyword only\n", all.keywordOnly, run)
	} else {
		fmt.Fprintln(w, "- Search mode: hybrid (keyword + semantic) for every case")
	}
	fmt.Fprintf(w, "- Cut-off: top %d results\n", topK)
	fmt.Fprintf(w, "- Cases replayed with a date window: %d\n", all.windowed)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "| Cases | n | Recall@10 normal | Recall@10 risk-aware | MRR normal | MRR risk-aware | Found normal | Found risk-aware |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|")
	row := func(name string, s summary) {
		if s.cases == 0 {
			return
		}
		fmt.Fprintf(w, "| %s | %d | %.3f | %.3f | %.3f | %.3f | %d | %d |\n", name, s.cases,
			s.recallNormal, s.recallRisk, s.mrrNormal, s.mrrRisk, s.foundNormal, s.foundRisk)
	}
	row("All", all)
	names, groups := bySource(outcomes)
	for _, n := range names {
		row(n, summarise(groups[n]))
	}
	if all.emptyCases > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Queries with no right answer in the corpus: %d of %d correctly returned nothing.\n", all.emptyCorrect, all.emptyCases)
	}

	describe := func(o outcome) string {
		if public {
			return fmt.Sprintf("`%s` (%s)", o.c.ID, o.c.Source)
		}
		return fmt.Sprintf("`%s` (%s): %s", o.c.ID, o.c.Source, o.c.Query)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "### Failing cases, normal search (%d)\n\n", len(all.failingNormal))
	for _, o := range all.failingNormal {
		where := fmt.Sprintf("not in the top %d", searchDepth)
		if o.normal.deepRank > 0 {
			where = fmt.Sprintf("first right answer at rank %d", o.normal.deepRank)
		}
		fmt.Fprintf(w, "- %s; %s\n", describe(o), where)
	}
	// Risk-aware failures that normal search also fails add nothing; list
	// only the ones reranking itself lost.
	var lost []outcome
	for _, o := range all.failingRsk {
		if o.normal.firstRank > 0 {
			lost = append(lost, o)
		}
	}
	fmt.Fprintf(w, "\n### Cases risk-aware search loses that normal search finds (%d)\n\n", len(lost))
	for _, o := range lost {
		fmt.Fprintf(w, "- %s; was at rank %d\n", describe(o), o.normal.firstRank)
	}
	var skipped []outcome
	for _, o := range outcomes {
		if o.skipped != "" {
			skipped = append(skipped, o)
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "\n### Skipped (%d)\n\n", len(skipped))
		for _, o := range skipped {
			fmt.Fprintf(w, "- `%s`: %s\n", o.c.ID, o.skipped)
		}
	}
}
