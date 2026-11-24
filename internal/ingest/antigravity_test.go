// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/antigravity"
	"github.com/danPoku/kaectx/internal/ingest/codex"
)

// The two conversations in antigravity/testdata, by their (replaced) ids:
// an interactive one in a repository and a headless one in a scratch folder.
const (
	agyRepoConv    = "0a1b2c3d-1111-4aaa-8bbb-000000000001"
	agyScratchConv = "0a1b2c3d-2222-4aaa-8bbb-000000000002"
)

func newAntigravity() ingest.Adapter { return antigravity.New() }

// copyAntigravityData copies the fixture data folder to a temp directory and
// returns its path: the stand-in for ~/.gemini/antigravity-cli.
func copyAntigravityData(t *testing.T) string {
	t.Helper()
	src := filepath.Join("antigravity", "testdata", "antigravity-cli")
	dst := filepath.Join(t.TempDir(), "antigravity-cli")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func agyTranscript(data, id string) string {
	return filepath.Join(data, "brain", id, ".system_generated", "logs", antigravity.TranscriptName)
}

// fileURI spells a local folder the way Antigravity stores it in
// workspace_uris: file:///C:/Users/dev/proj or file:///home/dev/proj.
func fileURI(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// writeAgySummaries creates Antigravity's conversation_summaries.db in data
// with one row per (conversation id, folder URI, title). Only the columns
// the adapter reads, plus the two the real table requires.
func writeAgySummaries(t *testing.T, data string, rows ...[3]string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(data, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS conversation_summaries (
	        conversation_id text PRIMARY KEY, title text NOT NULL DEFAULT "",
	        workspace_uris text NOT NULL, last_modified_time datetime NOT NULL,
	        last_user_input_time datetime NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		uris, _ := json.Marshal([]string{r[1]})
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?,?,?,'2026-11-24 08:08:09+00:00','2026-11-24 08:08:09+00:00')`,
			r[0], r[2], string(uris)); err != nil {
			t.Fatal(err)
		}
	}
}

// The whole path for a real data folder: only the full transcripts are read,
// one line becomes several messages, results get their tool's name, file
// touches are project-relative, and a second run adds nothing.
func TestRunAllAntigravityIngestsConversations(t *testing.T) {
	db := openTestDB(t)
	data := copyAntigravityData(t)
	writeAgySummaries(t, data,
		[3]string{agyRepoConv, "file:///C:/Users/dev/proj", "Explore Repository Without Code Edits"},
		[3]string{agyScratchConv, "file:///C:/Users/dev/scratch", ""},
	)

	// Everything else Antigravity keeps beside a transcript, all .jsonl and
	// none of it a session log. The shortened copy and the 102400-byte
	// pieces repeat the transcript; reading them would store it three times.
	full, err := os.ReadFile(agyTranscript(data, agyRepoConv))
	if err != nil {
		t.Fatal(err)
	}
	conv := filepath.Join(data, "brain", agyRepoConv)
	decoys := map[string][]byte{
		filepath.Join(conv, ".system_generated", "logs", "transcript.jsonl"):                            full,
		filepath.Join(conv, ".system_generated", "logs", "chunks", "transcript_full", "00000000.jsonl"): full[:len(full)/2],
		filepath.Join(conv, ".system_generated", "logs", "chunks", "transcript", "00000000.jsonl"):      full[:len(full)/2],
		filepath.Join(conv, ".system_generated", "steps", "2", "notes.jsonl"):                           []byte(`{"type":"USER_INPUT","content":"not a prompt"}` + "\n"),
		filepath.Join(conv, "scratch", "2", "transcript_full.jsonl"):                                    full,
		filepath.Join(conv, ".user_uploaded", "data.jsonl"):                                             []byte(`{"a":1}` + "\n"),
	}
	for path, content := range decoys {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	root := filepath.Join(data, "brain")
	results, err := ingest.RunAll(db, newAntigravity, root)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("read %d files, want the 2 full transcripts: %+v", len(results), results)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Path, r.Err)
		}
	}

	repoSession, scratchSession := "antigravity:"+agyRepoConv, "antigravity:"+agyScratchConv

	// Session fields: the folder and title come from Antigravity's
	// database, the model from the transcript.
	var agent, cwd, title, model string
	if err := db.QueryRow(`SELECT agent, cwd, title, model FROM sessions WHERE id = ?`, repoSession).Scan(&agent, &cwd, &title, &model); err != nil {
		t.Fatalf("repo session: %v", err)
	}
	if agent != "antigravity" || cwd != `C:\Users\dev\proj` || title != "Explore Repository Without Code Edits" || model != "gemini-3.8-flash-high" {
		t.Errorf("repo session = agent %q cwd %q title %q model %q", agent, cwd, title, model)
	}
	// With no title in the database the first prompt stands in, without
	// the wrapper Antigravity puts round it.
	if err := db.QueryRow(`SELECT title FROM sessions WHERE id = ?`, scratchSession).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(title, "In notes.txt change the word alpha to beta") {
		t.Errorf("scratch session title = %q, want the first prompt", title)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE project_id IS NULL`); n != 0 {
		t.Errorf("%d sessions have no project", n)
	}
	if n := count(t, db, `SELECT count(DISTINCT project_id) FROM sessions`); n != 2 {
		t.Errorf("two folders gave %d projects, want 2", n)
	}

	// 16 lines, 21 messages: five of the model steps hold more than one.
	if n := count(t, db, `SELECT count(*) FROM messages WHERE session_id = ?`, repoSession); n != 21 {
		t.Errorf("repo session has %d messages, want 21", n)
	}
	// seq runs 0..n-1 with no gap, so paging by seq works.
	if n := count(t, db, `SELECT max(seq) FROM messages WHERE session_id = ?`, repoSession); n != 20 {
		t.Errorf("max seq = %d, want 20", n)
	}
	// Each result is named after the call it answers, paired by order.
	var names string
	if err := db.QueryRow(`SELECT group_concat(tool_name, ',') FROM (SELECT coalesce(tool_name, '?') AS tool_name FROM messages WHERE session_id = ? AND kind = 'tool_result' ORDER BY seq)`, repoSession).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if want := "run_command,read_file,read_file,run_command,write_file,run_command"; names != want {
		t.Errorf("tool results are named %q, want %q", names, want)
	}
	// The prompt is stored without Antigravity's appended context.
	var prompt string
	if err := db.QueryRow(`SELECT content FROM messages WHERE session_id = ? AND role = 'user' ORDER BY seq LIMIT 1`, repoSession).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if prompt != "explore this repo. No code/edits" {
		t.Errorf("prompt stored as %q", prompt)
	}

	// File touches, relative to the conversation's folder.
	var touches string
	if err := db.QueryRow(`SELECT group_concat(t, ',') FROM (SELECT session_id || '=' || path || ':' || action AS t FROM file_touches ORDER BY session_id, id)`).Scan(&touches); err != nil {
		t.Fatal(err)
	}
	want := repoSession + "=go.mod:read," + repoSession + "=CLAUDE.md:read," + repoSession + "=scratch.txt:edit," + scratchSession + "=notes.txt:edit"
	if touches != want {
		t.Errorf("file touches:\n got %s\nwant %s", touches, want)
	}

	// Both turns of the repo conversation ended with a written answer, so
	// both are searchable; the appended context is not in the index.
	if n := count(t, db, `SELECT count(*) FROM chunks WHERE session_id = ?`, repoSession); n != 2 {
		t.Errorf("repo session has %d chunks, want 2", n)
	}
	if n := count(t, db, `SELECT count(*) FROM chunks WHERE text LIKE '%ADDITIONAL_METADATA%' OR text LIKE '%USER_REQUEST%'`); n != 0 {
		t.Errorf("%d chunks contain Antigravity's prompt wrapper", n)
	}
	if n := count(t, db, `SELECT count(*) FROM unknown_line_types`); n != 0 {
		t.Errorf("%d line types were reported as unknown", n)
	}

	// Re-ingest adds no rows.
	before := [4]int{
		count(t, db, `SELECT count(*) FROM messages`), count(t, db, `SELECT count(*) FROM chunks`),
		count(t, db, `SELECT count(*) FROM file_touches`), count(t, db, `SELECT count(*) FROM sources`),
	}
	if _, err := ingest.RunAll(db, newAntigravity, root); err != nil {
		t.Fatalf("second RunAll: %v", err)
	}
	after := [4]int{
		count(t, db, `SELECT count(*) FROM messages`), count(t, db, `SELECT count(*) FROM chunks`),
		count(t, db, `SELECT count(*) FROM file_touches`), count(t, db, `SELECT count(*) FROM sources`),
	}
	if before != after || after[3] != 2 {
		t.Errorf("re-ingest changed row counts (messages, chunks, touches, sources): %v -> %v", before, after)
	}

	// The freshness check agrees about which files count: with both
	// transcripts read, the decoys must not be reported as unread sessions.
	f, err := ingest.CheckFreshness(db, []ingest.Root{{Agent: "antigravity", Dir: root, NewAdapter: newAntigravity}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if f.PendingFiles != 0 {
		t.Errorf("freshness reports %d unread files; the other .jsonl files are not sessions", f.PendingFiles)
	}
}

// The issue's first criterion: an Antigravity exchange and a Codex exchange
// from the same repository are found by one search, as one project.
func TestAntigravityAndCodexSessionsShareAProject(t *testing.T) {
	db := openTestDB(t)
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")

	data := copyAntigravityData(t)
	writeAgySummaries(t, data, [3]string{agyRepoConv, fileURI(repo), ""})
	if err := ingest.Run(db, antigravity.New(), agyTranscript(data, agyRepoConv)); err != nil {
		t.Fatalf("antigravity Run: %v", err)
	}

	jl := func(typ, ts string, payload map[string]any) string {
		b, _ := json.Marshal(map[string]any{"timestamp": ts, "type": typ, "payload": payload})
		return string(b) + "\n"
	}
	rollout := jl("session_meta", "2026-11-24T09:00:00Z", map[string]any{"session_id": "cx-same-repo", "cwd": repo, "cli_version": "0"}) +
		jl("response_item", "2026-11-24T09:00:01Z", map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "explore the repo layout"}},
			"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"user.text"}},
		}) +
		jl("response_item", "2026-11-24T09:00:02Z", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "The repo has two packages."}},
		})
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("codex Run: %v", err)
	}

	if n := count(t, db, `SELECT count(DISTINCT project_id) FROM sessions WHERE project_id IS NOT NULL`); n != 1 {
		t.Fatalf("the two agents' sessions are in %d projects, want 1", n)
	}
	// One keyword search over the project's chunks returns both agents.
	var agents string
	err := db.QueryRow(`SELECT group_concat(agent, ',') FROM (
	        SELECT DISTINCT s.agent AS agent
	          FROM chunks_fts f
	          JOIN chunks c ON c.id = f.rowid
	          JOIN sessions s ON s.id = c.session_id
	         WHERE chunks_fts MATCH 'repo'
	         ORDER BY s.agent)`).Scan(&agents)
	if err != nil {
		t.Fatal(err)
	}
	if agents != "antigravity,codex" {
		t.Errorf("a search for 'repo' found agents %q, want antigravity,codex", agents)
	}
}

// A conversation still being written is read in several runs, each with a
// fresh adapter. The conversation id comes from the path, so a later run
// must file its lines under the same session and continue the numbering.
func TestRunAntigravityResumesMidFile(t *testing.T) {
	db := openTestDB(t)
	data := copyAntigravityData(t)
	writeAgySummaries(t, data, [3]string{agyRepoConv, "file:///C:/Users/dev/proj", ""})
	path := agyTranscript(data, agyRepoConv)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(full), "\n")

	// First run: the prompt and the model step that calls three tools, with
	// only the first of its results written so far.
	if err := os.WriteFile(path, []byte(strings.Join(lines[:5], "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, antigravity.New(), path); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM chunks`); n != 0 {
		t.Errorf("an unfinished turn was indexed (%d chunks)", n)
	}

	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, antigravity.New(), path); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM messages`); n != 21 {
		t.Errorf("%d messages after resuming, want 21", n)
	}
	if n := count(t, db, `SELECT count(*) FROM (SELECT seq FROM messages GROUP BY seq HAVING count(*) > 1)`); n != 0 {
		t.Errorf("%d seq values are used twice", n)
	}
}

// Antigravity writes the transcript before its database has the
// conversation's row. The session is stored at once without a project and
// gains one on the next run, with its file touches moved along.
func TestRunAntigravityProjectArrivesAfterTheFirstLines(t *testing.T) {
	db := openTestDB(t)
	data := copyAntigravityData(t)
	path := agyTranscript(data, agyRepoConv)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(full), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:7], "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, antigravity.New(), path); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	session := "antigravity:" + agyRepoConv
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE id = ? AND project_id IS NULL`, session); n != 1 {
		t.Fatalf("want the session stored with no project while its folder is unknown")
	}
	if n := count(t, db, `SELECT count(*) FROM file_touches`); n != 2 {
		t.Fatalf("%d file touches from the first lines, want 2", n)
	}

	writeAgySummaries(t, data, [3]string{agyRepoConv, "file:///C:/Users/dev/proj", "Explore"})
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, antigravity.New(), path); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions WHERE id = ? AND project_id IS NOT NULL AND title = 'Explore'`, session); n != 1 {
		t.Errorf("the session did not gain its project and title on the second run")
	}
	if n := count(t, db, `SELECT count(*) FROM file_touches WHERE project_id IS NULL`); n != 0 {
		t.Errorf("%d file touches were left without the project", n)
	}
}

// `kae ingest --path` on a transcript copied somewhere else: there is no
// conversation id to file it under. Nothing is stored and the reason is
// recorded against the file.
func TestRunAntigravityTranscriptOutsideItsFolder(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, filepath.Join("antigravity", "testdata", "antigravity-cli", "brain", agyRepoConv, ".system_generated", "logs", antigravity.TranscriptName))
	if err := ingest.Run(db, antigravity.New(), path); err != nil {
		t.Fatalf("Run must not fail the file: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM sessions`); n != 0 {
		t.Errorf("%d sessions stored from a transcript with no conversation id", n)
	}
	var lastErr sql.NullString
	if err := db.QueryRow(`SELECT last_error FROM sources WHERE path = ?`, path).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastErr.String, "which conversation") {
		t.Errorf("last_error = %q, want the reason", lastErr.String)
	}
}
