// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// copyFixture puts a mutable copy of an adapter's fixture session into a
// temp file, so tests can append to it to simulate a session continuing
// between two ingest runs without touching the checked-in testdata.
func copyFixture(t *testing.T, srcPath string) string {
	t.Helper()
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(srcPath))
	if err := os.WriteFile(path, src, 0644); err != nil {
		t.Fatalf("write fixture copy: %v", err)
	}
	return path
}

func TestRunIngestsSessionAndChunksOnlySettledTurns(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "claudecode/testdata/session.jsonl")
	adapter := claudecode.New()

	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var msgCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 8 {
		t.Errorf("messages = %d, want 8", msgCount)
	}

	var sessionCount int
	var title, agent string
	if err := db.QueryRow(`SELECT COUNT(*), title, agent FROM sessions GROUP BY id`).Scan(&sessionCount, &title, &agent); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("sessions = %d, want 1", sessionCount)
	}
	if title != "WAL mode question" {
		t.Errorf("title = %q, want the ai-title event's value", title)
	}
	if agent != "claude-code" {
		t.Errorf("agent = %q, want claude-code", agent)
	}

	// The fixture's second turn ends on a tool_result with no closing
	// assistant text yet — it must NOT be chunked (the "settled tail"
	// rule), so only the first, complete turn should be indexed.
	var chunkCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != 1 {
		t.Errorf("chunks = %d, want 1 (second turn is still dangling)", chunkCount)
	}

	var firstSeq, lastSeq int
	var text string
	if err := db.QueryRow(`SELECT first_seq, last_seq, text FROM chunks`).Scan(&firstSeq, &lastSeq, &text); err != nil {
		t.Fatalf("query chunk: %v", err)
	}
	if firstSeq != 0 || lastSeq != 4 {
		t.Errorf("chunk span = [%d,%d], want [0,4]", firstSeq, lastSeq)
	}
	if !strings.Contains(text, "WAL mode") || !strings.Contains(text, "_journal_mode=WAL") {
		t.Errorf("chunk text = %q, want it to contain both the question and the answer", text)
	}
	// Tool noise must be trimmed out of the indexed text.
	if strings.Contains(text, "toolu_1") || strings.Contains(text, "Read") {
		t.Errorf("chunk text = %q, tool call/result leaked into the index", text)
	}
}

func TestRunIsIdempotentAndCompletesDanglingTurnsOnReingest(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "claudecode/testdata/session.jsonl")
	adapter := claudecode.New()

	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Re-running with no new bytes must be a pure no-op.
	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("second Run (no new data): %v", err)
	}
	var msgCount, chunkCount int
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunkCount)
	if msgCount != 8 {
		t.Errorf("after no-op re-run: messages = %d, want 8 (no duplicates)", msgCount)
	}
	if chunkCount != 1 {
		t.Errorf("after no-op re-run: chunks = %d, want 1 (no duplicates)", chunkCount)
	}

	// Now the assistant finishes the second (previously dangling) turn.
	closing := `{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"Use internal/redact.Redact on both content and raw before insert."}]},"sessionId":"sess-1","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:01:03Z"}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open fixture for append: %v", err)
	}
	if _, err := f.WriteString(closing); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("third Run (new data): %v", err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunkCount)
	if msgCount != 9 {
		t.Errorf("after completing turn: messages = %d, want 9", msgCount)
	}
	if chunkCount != 2 {
		t.Errorf("after completing turn: chunks = %d, want 2 (dangling turn now settled)", chunkCount)
	}

	var firstSeq, lastSeq int
	if err := db.QueryRow(`SELECT first_seq, last_seq FROM chunks ORDER BY first_seq DESC LIMIT 1`).Scan(&firstSeq, &lastSeq); err != nil {
		t.Fatalf("query newest chunk: %v", err)
	}
	if firstSeq != 5 || lastSeq != 8 {
		t.Errorf("newest chunk span = [%d,%d], want [5,8]", firstSeq, lastSeq)
	}
}

func TestRunLogsMalformedLinesWithoutAbortingTheFile(t *testing.T) {
	db := openTestDB(t)
	adapter := claudecode.New()

	content := `{"type":"user","message":{"role":"user","content":"first"},"sessionId":"sess-2","cwd":"/x","timestamp":"2026-09-20T10:00:00Z"}
not valid json at all
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]},"sessionId":"sess-2","timestamp":"2026-09-20T10:00:01Z"}
`
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var msgCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 2 {
		t.Errorf("messages = %d, want 2 (the malformed line skipped, its neighbors kept)", msgCount)
	}

	var lastErr sql.NullString
	if err := db.QueryRow(`SELECT last_error FROM sources WHERE path = ?`, path).Scan(&lastErr); err != nil {
		t.Fatalf("query source: %v", err)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Errorf("sources.last_error = %v, want the malformed line's error recorded", lastErr)
	}
}

// TestRunCodexSessionChunksOnlySettledTurns is the Codex analog of
// TestRunIngestsSessionAndChunksOnlySettledTurns: same settled/dangling
// chunking rule, exercised through a structurally different adapter (no
// per-line session id, content_item_kinds-driven turn classification) to
// confirm that logic is genuinely agent-agnostic and not accidentally
// tailored to Claude Code's shape.
func TestRunCodexSessionChunksOnlySettledTurns(t *testing.T) {
	db := openTestDB(t)
	path := copyFixture(t, "codex/testdata/session.jsonl")

	// Each Run needs its own Adapter instance — Codex's adapter carries
	// per-file session-id state (see its package doc).
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var msgCount int
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	if msgCount != 9 {
		t.Errorf("messages = %d, want 9", msgCount)
	}

	var agent string
	if err := db.QueryRow(`SELECT agent FROM sessions`).Scan(&agent); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if agent != "codex" {
		t.Errorf("agent = %q, want codex", agent)
	}

	// The fixture's second turn ends on a bare tool_call (a destructive
	// `rm`, deliberately never actually run — it's fixture text) with no
	// tool_result or closing assistant text: must not be chunked.
	var chunkCount int
	db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunkCount)
	if chunkCount != 1 {
		t.Errorf("chunks = %d, want 1 (second turn is still dangling)", chunkCount)
	}

	var text string
	if err := db.QueryRow(`SELECT text FROM chunks`).Scan(&text); err != nil {
		t.Fatalf("query chunk: %v", err)
	}
	if !strings.Contains(text, "How many files") || !strings.Contains(text, "2 files") {
		t.Errorf("chunk text = %q, want the real question and answer", text)
	}
	// The framework-injected context (skills instructions, environment
	// snapshot) that arrived under role=developer/user before the genuine
	// user.text turn must not have been misread as part of the turn.
	if strings.Contains(text, "skills_instructions") || strings.Contains(text, "environment_context") {
		t.Errorf("chunk text = %q, framework-injected context leaked into the index", text)
	}
	// The tool call itself must be trimmed — "main.go" is fine here since
	// it's also genuinely part of the assistant's final answer text.
	if strings.Contains(text, "ls -1") {
		t.Errorf("chunk text = %q, tool call leaked into the index", text)
	}
}

// TestRunRecordsFileTouches proves the driver actually writes file_touches
// rows end to end (project_id resolved, path relativized against the
// session's cwd, action mapped correctly) and that re-ingesting the same
// bytes doesn't duplicate them — the INSERT OR IGNORE + RowsAffected guard
// in processLine is what's under test here, not just the adapter's parsing.
func TestRunRecordsFileTouches(t *testing.T) {
	db := openTestDB(t)

	content := `{"type":"user","message":{"role":"user","content":"read and edit main.go"},"sessionId":"sess-ft","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/home/dev/proj/main.go"}}]},"sessionId":"sess-ft","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:01Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package main"}]},"sessionId":"sess-ft","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:02Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/home/dev/proj/main.go","old_string":"a","new_string":"b"}}]},"sessionId":"sess-ft","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:03Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]},"sessionId":"sess-ft","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:04Z"}
`
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	adapter := claudecode.New()
	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	rows, err := db.Query(`SELECT path, action, project_id FROM file_touches ORDER BY id`)
	if err != nil {
		t.Fatalf("query file_touches: %v", err)
	}
	type touch struct {
		path, action string
		projectID    sql.NullInt64
	}
	var got []touch
	for rows.Next() {
		var tr touch
		if err := rows.Scan(&tr.path, &tr.action, &tr.projectID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, tr)
	}
	rows.Close()

	want := []touch{{path: "main.go", action: "read"}, {path: "main.go", action: "edit"}}
	if len(got) != len(want) {
		t.Fatalf("file_touches = %+v, want %d rows", got, len(want))
	}
	for i, w := range want {
		if got[i].path != w.path || got[i].action != w.action {
			t.Errorf("file_touches[%d] = {path:%q action:%q}, want {path:%q action:%q}", i, got[i].path, got[i].action, w.path, w.action)
		}
		if !got[i].projectID.Valid {
			t.Errorf("file_touches[%d].project_id is NULL, want it resolved from cwd", i)
		}
	}

	// Re-ingesting the same bytes must not duplicate the touches.
	if err := ingest.Run(db, adapter, path); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM file_touches`).Scan(&count); err != nil {
		t.Fatalf("count file_touches: %v", err)
	}
	if count != 2 {
		t.Errorf("file_touches after re-run = %d, want 2 (no duplicates)", count)
	}
}

// A live daemon can read a Codex file after only its first line (the
// session_meta that carries the session id) has been written, then resume
// later with a brand-new adapter. Lines after the resume point must still be
// stored — previously they came back with no session id and were dropped
// while the byte offset advanced anyway.
func TestRunResumeWithFreshAdapterKeepsCodexSessionID(t *testing.T) {
	db := openTestDB(t)
	full, err := os.ReadFile("codex/testdata/session.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	firstEnd := strings.Index(string(full), "\n") + 1
	path := filepath.Join(t.TempDir(), "rollout.jsonl")

	if err := os.WriteFile(path, full[:firstEnd], 0644); err != nil {
		t.Fatalf("write first line: %v", err)
	}
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("Run (first line): %v", err)
	}

	if err := os.WriteFile(path, full, 0644); err != nil {
		t.Fatalf("write full file: %v", err)
	}
	if err := ingest.Run(db, codex.New(), path); err != nil { // fresh adapter, as the daemon does
		t.Fatalf("Run (resume): %v", err)
	}

	var withResume int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&withResume); err != nil {
		t.Fatalf("count messages: %v", err)
	}

	// Reference: the same file ingested in one go.
	ref := openTestDB(t)
	if err := ingest.Run(ref, codex.New(), copyFixture(t, "codex/testdata/session.jsonl")); err != nil {
		t.Fatalf("Run (reference): %v", err)
	}
	var want int
	if err := ref.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&want); err != nil {
		t.Fatalf("count reference messages: %v", err)
	}
	if want == 0 || withResume != want {
		t.Errorf("messages after resume = %d, want %d (same as a one-shot ingest)", withResume, want)
	}
}

// When a file is rewritten (its already-read prefix changes) or shrinks, Run
// starts over from byte 0. The rows from the old version must be replaced,
// not joined by a second copy of every message.
func TestRunRewrittenFileReplacesRowsInsteadOfDuplicating(t *testing.T) {
	t.Run("truncated then rewritten shorter", func(t *testing.T) {
		db := openTestDB(t)
		path := copyFixture(t, "claudecode/testdata/session.jsonl")
		if err := ingest.Run(db, claudecode.New(), path); err != nil {
			t.Fatalf("first Run: %v", err)
		}
		var before int
		db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before)

		// Replace the file with just its first half: smaller than the stored
		// offset, but still holding real messages for the same session (a
		// shrunk file that no longer mentions a session at all leaves that
		// session's old rows alone — sources doesn't record which sessions a
		// file produced).
		full, _ := os.ReadFile(path)
		lines := strings.SplitAfter(string(full), "\n")
		half := strings.Join(lines[:len(lines)/2], "")
		if err := os.WriteFile(path, []byte(half), 0644); err != nil {
			t.Fatal(err)
		}
		if err := ingest.Run(db, claudecode.New(), path); err != nil {
			t.Fatalf("Run after truncation: %v", err)
		}
		var after int
		db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&after)
		if after >= before {
			t.Errorf("messages after shrink = %d, want fewer than the %d from the old version", after, before)
		}
		var maxSeq, n int
		db.QueryRow(`SELECT COALESCE(MAX(seq),-1), COUNT(*) FROM messages`).Scan(&maxSeq, &n)
		if n > 0 && maxSeq != n-1 {
			t.Errorf("seq not restarted from 0: max=%d count=%d", maxSeq, n)
		}
	})

	t.Run("prefix altered, same length", func(t *testing.T) {
		db := openTestDB(t)
		path := copyFixture(t, "codex/testdata/session.jsonl")
		if err := ingest.Run(db, codex.New(), path); err != nil {
			t.Fatalf("first Run: %v", err)
		}
		var before, chunksBefore int
		db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before)
		db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunksBefore)

		// Change a byte inside the first 4KB we already hashed, keeping the
		// length (and so the offset) the same: the "rewritten" branch, not the "shrunk" one.
		orig, _ := os.ReadFile(path)
		changed := append([]byte(nil), orig...)
		i := strings.Index(string(changed), "codex")
		if i < 0 || i >= 4096 {
			t.Fatalf("fixture has no 'codex' within its first 4KB to alter (index %d)", i)
		}
		changed[i] = 'C'
		if err := os.WriteFile(path, changed, 0644); err != nil {
			t.Fatal(err)
		}
		if err := ingest.Run(db, codex.New(), path); err != nil {
			t.Fatalf("Run after rewrite: %v", err)
		}

		var after, chunksAfter int
		db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&after)
		db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunksAfter)
		if after != before {
			t.Errorf("messages after rewrite = %d, want %d (replaced, not appended)", after, before)
		}
		if chunksAfter != chunksBefore {
			t.Errorf("chunks after rewrite = %d, want %d", chunksAfter, chunksBefore)
		}
	})
}

// The VS Code Claude Code extension writes an empty system event after each
// assistant reply. A finished final turn followed only by such bookkeeping
// rows must still be chunked (and so searchable); one that ends in a tool
// call must not.
func TestRunChunksFinalTurnFollowedByBookkeepingEvent(t *testing.T) {
	line := func(typ, body string) string {
		return `{"type":"` + typ + `",` + body + `"sessionId":"sess-tail","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:00Z"}` + "\n"
	}
	user := line("user", `"message":{"role":"user","content":"Remember ZEPHYRQUOKKA-TAIL"},`)
	assistant := line("assistant", `"message":{"role":"assistant","model":"m","content":[{"type":"text","text":"Noted ZEPHYRQUOKKA-TAIL."}]},`)
	toolCall := line("assistant", `"message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},`)
	// A bookkeeping row the adapter stores as system/event with no content.
	event := line("system", `"subtype":"turn_duration",`)

	cases := []struct {
		name       string
		content    string
		wantChunks int
	}{
		{"reply then bookkeeping event", user + assistant + event, 1},
		{"reply then two events", user + assistant + event + event, 1},
		{"reply only (unchanged behaviour)", user + assistant, 1},
		{"ends mid tool call", user + toolCall, 0},
		{"tool call then event", user + toolCall + event, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}
			if err := ingest.Run(db, claudecode.New(), path); err != nil {
				t.Fatalf("Run: %v", err)
			}
			var n int
			db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&n)
			if n != tc.wantChunks {
				var rows []string
				r, _ := db.Query(`SELECT seq||':'||role||'/'||kind FROM messages ORDER BY seq`)
				for r.Next() {
					var s string
					r.Scan(&s)
					rows = append(rows, s)
				}
				r.Close()
				t.Errorf("chunks = %d, want %d (messages: %v)", n, tc.wantChunks, rows)
			}
		})
	}
}

// A Claude Code subagent transcript (<session>/subagents/agent-*.jsonl)
// reuses its parent's sessionId, and the subagent is often sent to a parent
// folder that is a project of its own. Whichever file is read first, the
// session must end up in the project its main transcript starts in, with
// that transcript's first timestamp, and stay there when the cwd drifts.
func TestRunSubagentTranscriptDoesNotSetSessionProject(t *testing.T) {
	const (
		repoDir   = "/home/dev/work/repo"
		parentDir = "/home/dev/work"
		sessionID = "claude-code:sess-sub"
		started   = "2026-09-20T10:00:00.100Z"
	)
	main, err := os.ReadFile("claudecode/testdata/subagent/sess-sub.jsonl")
	if err != nil {
		t.Fatalf("read main fixture: %v", err)
	}
	sub, err := os.ReadFile("claudecode/testdata/subagent/sess-sub/subagents/agent-a1.jsonl")
	if err != nil {
		t.Fatalf("read subagent fixture: %v", err)
	}
	mainLines := strings.SplitAfter(string(main), "\n")

	// layout writes the fixture tree into root as Claude Code lays it out,
	// with the main transcript cut after its first n lines.
	layout := func(t *testing.T, root string, n int) (mainPath, subPath string) {
		t.Helper()
		mainPath = filepath.Join(root, "sess-sub.jsonl")
		subPath = filepath.Join(root, "sess-sub", "subagents", "agent-a1.jsonl")
		if err := os.MkdirAll(filepath.Dir(subPath), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(subPath, sub, 0644); err != nil {
			t.Fatalf("write subagent: %v", err)
		}
		if err := os.WriteFile(mainPath, []byte(strings.Join(mainLines[:n], "")), 0644); err != nil {
			t.Fatalf("write main: %v", err)
		}
		return mainPath, subPath
	}

	tests := []struct {
		name   string
		ingest func(t *testing.T, db *sql.DB, root string)
	}{
		{
			// WalkDir visits the "sess-sub/" directory before
			// "sess-sub.jsonl", so the subagent creates the row.
			name: "one walk of the whole tree",
			ingest: func(t *testing.T, db *sql.DB, root string) {
				layout(t, root, len(mainLines))
				if _, err := ingest.RunAll(db, func() ingest.Adapter { return claudecode.New() }, root); err != nil {
					t.Fatalf("RunAll: %v", err)
				}
			},
		},
		{
			// The subagent is read alone, then the main transcript grows
			// across passes, ending in the parent folder.
			name: "subagent first, then incremental passes",
			ingest: func(t *testing.T, db *sql.DB, root string) {
				mainPath, subPath := layout(t, root, 0)
				if err := ingest.Run(db, claudecode.New(), subPath); err != nil {
					t.Fatalf("Run subagent: %v", err)
				}
				for n := 2; n <= len(mainLines); n++ {
					if err := os.WriteFile(mainPath, []byte(strings.Join(mainLines[:n], "")), 0644); err != nil {
						t.Fatalf("grow main: %v", err)
					}
					if err := ingest.Run(db, claudecode.New(), mainPath); err != nil {
						t.Fatalf("Run main, %d lines: %v", n, err)
					}
				}
				if err := ingest.Run(db, claudecode.New(), subPath); err != nil {
					t.Fatalf("re-Run subagent: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			// Both folders are already projects of their own, because
			// earlier sessions started in each. The repo is registered
			// directly: with no git remote here, resolving it would find
			// the parent by path prefix.
			parentID, err := store.ResolveProject(db, parentDir)
			if err != nil {
				t.Fatalf("ResolveProject(parent): %v", err)
			}
			res, err := db.Exec(`INSERT INTO projects(name) VALUES ('repo')`)
			if err != nil {
				t.Fatalf("insert repo project: %v", err)
			}
			repoID, _ := res.LastInsertId()
			if _, err := db.Exec(`INSERT INTO project_paths(path, project_id) VALUES (?, ?)`, filepath.Clean(repoDir), repoID); err != nil {
				t.Fatalf("insert repo path: %v", err)
			}

			tt.ingest(t, db, t.TempDir())

			var projectID int64
			var startedAt, meta string
			if err := db.QueryRow(`SELECT project_id, started_at, meta FROM sessions WHERE id = ?`, sessionID).
				Scan(&projectID, &startedAt, &meta); err != nil {
				t.Fatalf("query session: %v", err)
			}
			if projectID != repoID {
				t.Errorf("project_id = %d, want the repo's %d (parent folder is %d)", projectID, repoID, parentID)
			}
			if startedAt != started {
				t.Errorf("started_at = %q, want the main transcript's first line %q", startedAt, started)
			}
			if strings.Contains(meta, "provisional_project") {
				t.Errorf("meta = %s, want the provisional marker cleared", meta)
			}
		})
	}
}

// A session whose row is created by a line with no cwd and no timestamp (an
// ai-title read first) must still get its project and started_at from the
// first line that carries them, instead of keeping NULL and "" for good.
func TestRunFillsProjectAndStartFromFirstLineThatHasThem(t *testing.T) {
	db := openTestDB(t)
	content := `{"type":"ai-title","aiTitle":"Untimed","sessionId":"sess-late"}
{"type":"user","message":{"role":"user","content":"hello"},"sessionId":"sess-late","cwd":"/home/dev/proj","timestamp":"2026-09-20T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"sessionId":"sess-late","cwd":"/home/dev/other","timestamp":"2026-09-20T10:00:01Z"}
`
	path := filepath.Join(t.TempDir(), "sess-late.jsonl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want, ok, err := store.LookupProject(db, "/home/dev/proj")
	if err != nil || !ok {
		t.Fatalf("LookupProject = %d, %v, %v; want the first cwd registered", want, ok, err)
	}
	var projectID sql.NullInt64
	var startedAt string
	if err := db.QueryRow(`SELECT project_id, started_at FROM sessions WHERE id = 'claude-code:sess-late'`).Scan(&projectID, &startedAt); err != nil {
		t.Fatalf("query session: %v", err)
	}
	if !projectID.Valid || projectID.Int64 != want {
		t.Errorf("project_id = %v, want %d from the first line with a cwd", projectID, want)
	}
	if startedAt != "2026-09-20T10:00:00Z" {
		t.Errorf("started_at = %q, want the first line with a timestamp", startedAt)
	}
}
