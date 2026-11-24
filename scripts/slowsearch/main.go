// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command slowsearch is a read-only development tool for issue #44. It takes
// the slowest recorded searches from a copy of a kae database and replays
// each one stage by stage, timing the query embedding, the keyword search,
// the vector search and the fused query separately, so the stage behind the
// slow tail can be named with numbers instead of guessed.
//
//	go run -tags sqlite_fts5 ./scripts/slowsearch -db /path/to/copy-of-kae.db          # Ollama warm
//	go run -tags sqlite_fts5 ./scripts/slowsearch -db /path/to/copy-of-kae.db -cold    # model unloaded before each query
//
// -cold asks Ollama to unload the embedding model before every query, which
// is what a search finds after the model's keep-alive has run out. It affects
// only that one model, and Ollama loads it again on the next request.
//
// The report names searches by run ID, never by their text, so it can be
// posted. The database is opened with mode=ro and its size and modification
// time are checked before and after.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

func main() {
	dbPath := flag.String("db", "", "path to a copy of kae.db (opened read-only)")
	n := flag.Int("n", 20, "how many of the slowest recorded searches to replay")
	ollama := flag.String("ollama", embed.DefaultBaseURL, "Ollama base URL")
	cold := flag.Bool("cold", false, "unload the embedding model before each query")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "usage: go run -tags sqlite_fts5 ./scripts/slowsearch -db PATH [-n 20] [-cold] [-ollama URL]")
		os.Exit(2)
	}
	if err := run(*dbPath, *n, *ollama, *cold, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "slowsearch:", err)
		os.Exit(1)
	}
}

// recorded is one search as retrieval_runs stored it.
type recorded struct {
	id        string
	projectID int64
	query     string
	agent     string
	limit     int
	ms        int64
	createdAt string
}

// timing is one replay of one search, each stage on its own clock.
type timing struct {
	run recorded
	// embed is the round trip to Ollama for the query vector.
	embed time.Duration
	// keyword is the BM25 query alone, vector the nearest-neighbour query
	// alone. Neither is run by a hybrid search as such; they are the two
	// halves of the fused query, measured apart to see which one weighs.
	keyword, vector time.Duration
	// fused is the whole hybrid SQL statement, both halves and the rank
	// fusion, given a ready-made query vector. fusedWindowed is the same
	// statement asked with a date window, which makes it fetch 400 semantic
	// candidates instead of 50 and check each candidate's turn time.
	fused, fusedWindowed time.Duration
	// modelLoaded says whether Ollama had the model in memory just before
	// the embedding call.
	modelLoaded bool
	err         error
}

// total is what the search itself waits for: the embedding, then the fused
// query. (The keyword and vector figures are parts of the fused one.)
func (t timing) total() time.Duration { return t.embed + t.fused }

func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(abs)+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	// One connection, so every stage of every query shares one page cache,
	// as the stages of a real search do.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func slowest(db *sql.DB, n int) ([]recorded, error) {
	rows, err := db.Query(`SELECT id, project_id, query, COALESCE(agent_filter, ''), requested_limit, duration_ms, created_at
		FROM retrieval_runs WHERE mode = 'hybrid' ORDER BY duration_ms DESC, created_at LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recorded
	for rows.Next() {
		var r recorded
		if err := rows.Scan(&r.id, &r.projectID, &r.query, &r.agent, &r.limit, &r.ms, &r.createdAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// drain runs a query and reads every row, so the time covers the work
// SQLite does lazily while stepping through results.
func drain(db *sql.DB, sqlText string, args ...any) (time.Duration, error) {
	start := time.Now()
	rows, err := db.Query(sqlText, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	sink := make([]any, len(cols))
	for i := range sink {
		sink[i] = new(sql.RawBytes)
	}
	for rows.Next() {
		if err := rows.Scan(sink...); err != nil {
			return 0, err
		}
	}
	return time.Since(start), rows.Err()
}

// replay times one search's stages. windowUntil bounds the windowed variant
// of the fused query; it is the time the search was recorded, so that variant
// reads the corpus the original search could see.
func replay(ctx context.Context, db *sql.DB, client embed.Embedder, r recorded) timing {
	t := timing{run: r}

	start := time.Now()
	vec, err := client.Embed(ctx, r.query)
	t.embed = time.Since(start)
	if err != nil {
		t.err = fmt.Errorf("embed: %w", err)
		return t
	}
	vecJSON, err := json.Marshal(vec)
	if err != nil {
		t.err = err
		return t
	}

	// The fused statement runs first, on whatever the page cache holds, as it
	// does in a real search. The two halves are timed after it; they then
	// find their pages already read, so they show computation, not disk.
	name := "SearchHybrid"
	fusedArgs := func(w search.Window, semK int) []any {
		args := []any{
			sql.Named("query", search.SanitizeFTS5Query(r.query)),
			sql.Named("query_embedding", string(vecJSON)),
			sql.Named("project_id", r.projectID),
			// Recorded searches are replayed in the scope they ran in: the
			// project's own. Telemetry does not say which ran with scope=all.
			sql.Named("all_projects", 0),
			sql.Named("max_distance", search.MaxSemanticDistance),
			sql.Named("limit", r.limit),
			sql.Named("sem_k", semK),
		}
		args = append(args, w.Args()...)
		if r.agent != "" {
			args = append(args, sql.Named("agent", r.agent))
		}
		return args
	}
	if r.agent != "" {
		name = "SearchHybridByAgent"
	}
	fusedSQL, err := store.LoadQuery(queries.FS, "retrieval.sql", name)
	if err != nil {
		t.err = err
		return t
	}
	if t.fused, err = drain(db, fusedSQL, fusedArgs(search.Window{}, 50)...); err != nil {
		t.err = fmt.Errorf("fused query: %w", err)
		return t
	}
	window, err := search.ParseWindow("", r.createdAt)
	if err != nil {
		t.err = err
		return t
	}
	if t.fusedWindowed, err = drain(db, fusedSQL, fusedArgs(window, 400)...); err != nil {
		t.err = fmt.Errorf("windowed fused query: %w", err)
		return t
	}

	start = time.Now()
	if _, err := search.KeywordIn(db, r.query, r.projectID, r.agent, search.Window{}, r.limit); err != nil {
		t.err = fmt.Errorf("keyword query: %w", err)
		return t
	}
	t.keyword = time.Since(start)

	knn := `SELECT chunk_id, distance FROM chunk_vectors WHERE embedding MATCH ? AND k = 50 AND project_id = ?`
	knnArgs := []any{string(vecJSON), r.projectID}
	if r.agent != "" {
		knn += ` AND agent = ?`
		knnArgs = append(knnArgs, r.agent)
	}
	if t.vector, err = drain(db, knn, knnArgs...); err != nil {
		t.err = fmt.Errorf("vector query: %w", err)
	}
	return t
}

// ollamaControl unloads the embedding model and reports whether it is loaded.
type ollamaControl struct {
	base, model string
	http        *http.Client
}

// unload asks Ollama to drop the model. An embed request with keep_alive 0
// does it: the model answers this one request and is released straight after.
func (o ollamaControl) unload(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{"model": o.model, "input": "unload", "keep_alive": 0})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload: Ollama answered %s", resp.Status)
	}
	// The release is asynchronous; wait until Ollama stops listing the model.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		loaded, err := o.loaded(ctx)
		if err != nil {
			return err
		}
		if !loaded {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("unload: %s still loaded after 15s (another process may be using it)", o.model)
}

func (o ollamaControl) loaded(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/ps", nil)
	if err != nil {
		return false, err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return false, err
	}
	for _, m := range ps.Models {
		if m.Name == o.model || strings.HasPrefix(m.Name, o.model+":") {
			return true, nil
		}
	}
	return false, nil
}

func run(dbPath string, n int, ollamaURL string, cold bool, w io.Writer) error {
	before, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return err
	}
	runs, err := slowest(db, n)
	if err != nil {
		db.Close()
		return fmt.Errorf("load slowest searches: %w", err)
	}
	ctx := context.Background()
	client := embed.NewOllamaClient(ollamaURL, embed.DefaultModel)
	control := ollamaControl{base: ollamaURL, model: embed.DefaultModel, http: &http.Client{Timeout: 60 * time.Second}}

	if !cold {
		// One throwaway call so the first query in a warm pass is not a cold one.
		if _, err := client.Embed(ctx, "warm up"); err != nil {
			db.Close()
			return fmt.Errorf("Ollama is not answering at %s: %w", ollamaURL, err)
		}
	}
	timings := make([]timing, 0, len(runs))
	for _, r := range runs {
		if cold {
			if err := control.unload(ctx); err != nil {
				db.Close()
				return err
			}
		}
		loaded, _ := control.loaded(ctx)
		t := replay(ctx, db, client, r)
		t.modelLoaded = loaded
		timings = append(timings, t)
	}
	if err := db.Close(); err != nil {
		return err
	}
	after, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	unchanged := before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())

	writeReport(w, timings, cold)
	fmt.Fprintf(w, "\nDatabase file unchanged by this run (size and modification time): %v\n", unchanged)
	if !unchanged {
		return fmt.Errorf("the database file changed during the run; if it is the live database, run against a copy")
	}
	return nil
}

func ms(d time.Duration) string { return fmt.Sprintf("%d", d.Milliseconds()) }

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func writeReport(w io.Writer, timings []timing, cold bool) {
	state := "warm (model loaded before each query)"
	if cold {
		state = "cold (model unloaded before each query)"
	}
	fmt.Fprintf(w, "## Slowest recorded searches, replayed stage by stage: Ollama %s\n\n", state)
	fmt.Fprintln(w, "All times in milliseconds. \"Recorded\" is what the search took when it really ran. \"Total\" is embedding plus the fused query, which is what a hybrid search waits for; the keyword and vector columns are the fused query's two halves measured on their own.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Run | Recorded | Model loaded | Embedding | Keyword | Vector | Fused query | Fused, windowed | Total | Embedding share |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|")
	var embeds, fuseds, windoweds, keywords, vectors, totals []time.Duration
	var sumEmbed, sumTotal time.Duration
	failed := 0
	for _, t := range timings {
		id := t.run.id
		if len(id) > 8 {
			id = id[:8]
		}
		if t.err != nil {
			failed++
			fmt.Fprintf(w, "| %s | %d | %v | failed: %v |||||||\n", id, t.run.ms, t.modelLoaded, t.err)
			continue
		}
		share := 0.0
		if t.total() > 0 {
			share = 100 * float64(t.embed) / float64(t.total())
		}
		fmt.Fprintf(w, "| %s | %d | %v | %s | %s | %s | %s | %s | %s | %.0f%% |\n", id, t.run.ms, t.modelLoaded,
			ms(t.embed), ms(t.keyword), ms(t.vector), ms(t.fused), ms(t.fusedWindowed), ms(t.total()), share)
		embeds = append(embeds, t.embed)
		fuseds = append(fuseds, t.fused)
		windoweds = append(windoweds, t.fusedWindowed)
		keywords = append(keywords, t.keyword)
		vectors = append(vectors, t.vector)
		totals = append(totals, t.total())
		sumEmbed += t.embed
		sumTotal += t.total()
	}
	if len(totals) == 0 {
		return
	}
	for _, s := range [][]time.Duration{embeds, fuseds, windoweds, keywords, vectors, totals} {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Stage | Median | 90th pct | Slowest |")
	fmt.Fprintln(w, "|---|---|---|---|")
	for _, row := range []struct {
		name string
		s    []time.Duration
	}{{"Embedding", embeds}, {"Keyword", keywords}, {"Vector", vectors}, {"Fused query", fuseds}, {"Fused query, windowed", windoweds}, {"Total", totals}} {
		fmt.Fprintf(w, "| %s | %s | %s | %s |\n", row.name, ms(percentile(row.s, 0.5)), ms(percentile(row.s, 0.9)), ms(row.s[len(row.s)-1]))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Embedding is %.0f%% of the total time across these %d searches.", 100*float64(sumEmbed)/float64(sumTotal), len(totals))
	if failed > 0 {
		fmt.Fprintf(w, " %d could not be replayed.", failed)
	}
	fmt.Fprintln(w)
}
