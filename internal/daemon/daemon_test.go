// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package daemon_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/daemon"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/antigravity"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
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

// waitForSessionCount polls until sessions reaches want, or fails the test
// after timeout — avoids a flat sleep long enough for the slowest possible
// debounce+ingest cycle on every assertion.
func waitForSessionCount(t *testing.T, db *sql.DB, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&got); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if got >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got int
	db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&got)
	t.Fatalf("timed out waiting for %d session(s), have %d after %v", want, got, timeout)
}

func waitForMessageCount(t *testing.T, db *sql.DB, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&got); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if got >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got int
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&got)
	t.Fatalf("timed out waiting for %d message(s), have %d after %v", want, got, timeout)
}

func sessionLine(sessionID, text, ts string) string {
	return `{"type":"user","message":{"role":"user","content":"` + text + `"},"sessionId":"` + sessionID + `","cwd":"/home/dev/proj","timestamp":"` + ts + `"}` + "\n"
}

// TestRunIngestsExistingFilesOnStartup proves the initial catch-up pass:
// a file that already existed before Run was ever called still gets
// ingested, not just files that change afterward.
func TestRunIngestsExistingFilesOnStartup(t *testing.T) {
	db := openTestDB(t)
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "s1.jsonl"), []byte(sessionLine("s1", "hello", "2026-09-20T10:00:00Z")), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources := []daemon.Source{{Agent: "claude-code", Root: root, NewAdapter: func() ingest.Adapter { return claudecode.New() }}}

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, db, sources, time.Hour, 20*time.Millisecond) }()

	waitForSessionCount(t, db, 1, 2*time.Second)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestRunPicksUpNewFileAndNewDirectoryLive proves the fsnotify path: a
// brand new session file appearing in a brand new subdirectory (the shape
// Claude Code creates — one directory per project) gets watched and
// ingested without any fallback poll, well within a short fallback
// interval that hasn't fired yet.
func TestRunPicksUpNewFileAndNewDirectoryLive(t *testing.T) {
	db := openTestDB(t)
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources := []daemon.Source{{Agent: "claude-code", Root: root, NewAdapter: func() ingest.Adapter { return claudecode.New() }}}

	go daemon.Run(ctx, db, sources, time.Hour, 20*time.Millisecond)

	// Give the watcher a moment to establish itself before creating things
	// — a real race in any fsnotify-based tool, not just this test.
	time.Sleep(100 * time.Millisecond)

	newDir := filepath.Join(root, "new-project")
	if err := os.MkdirAll(newDir, 0755); err != nil {
		t.Fatalf("mkdir new project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "s1.jsonl"), []byte(sessionLine("s1", "live create", "2026-09-20T10:00:00Z")), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	waitForSessionCount(t, db, 1, 3*time.Second)
}

// TestRunReIngestsAppendedContent proves a growing file (the normal case —
// an agent appending to its own session log) gets re-ingested as new lines
// land, debounced rather than re-run on every single write.
func TestRunReIngestsAppendedContent(t *testing.T) {
	db := openTestDB(t)
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(root, "s1.jsonl")
	if err := os.WriteFile(path, []byte(sessionLine("s1", "first", "2026-09-20T10:00:00Z")), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources := []daemon.Source{{Agent: "claude-code", Root: root, NewAdapter: func() ingest.Adapter { return claudecode.New() }}}

	go daemon.Run(ctx, db, sources, time.Hour, 20*time.Millisecond)
	waitForMessageCount(t, db, 1, 2*time.Second)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(sessionLine("s1", "second", "2026-09-20T10:00:01Z")); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	waitForMessageCount(t, db, 2, 2*time.Second)
}

// An Antigravity conversation appears as a new folder several levels deep,
// with other .jsonl files beside the transcript. The daemon must ingest the
// transcript live and nothing else: the shortened copy would otherwise be
// stored as a second reading of the same conversation.
func TestRunIngestsAntigravityTranscriptAndIgnoresItsNeighbours(t *testing.T) {
	db := openTestDB(t)
	root := filepath.Join(t.TempDir(), "antigravity-cli", "brain")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources := []daemon.Source{{Agent: "antigravity", Root: root, NewAdapter: func() ingest.Adapter { return antigravity.New() }}}
	go daemon.Run(ctx, db, sources, time.Hour, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	logs := filepath.Join(root, "conv-1", ".system_generated", "logs")
	chunks := filepath.Join(logs, "chunks", "transcript_full")
	if err := os.MkdirAll(chunks, 0755); err != nil {
		t.Fatalf("mkdir conversation: %v", err)
	}
	line := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-24T08:08:09Z","content":"<USER_REQUEST>\nhello\n</USER_REQUEST>"}` + "\n"
	for _, path := range []string{
		filepath.Join(chunks, "00000000.jsonl"),
		filepath.Join(logs, "transcript.jsonl"),
		filepath.Join(logs, antigravity.TranscriptName),
	} {
		if err := os.WriteFile(path, []byte(line), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	waitForSessionCount(t, db, 1, 3*time.Second)
	// Long enough for a wrongly scheduled ingest of the other two files to
	// have run (the debounce is 20ms).
	time.Sleep(300 * time.Millisecond)
	var messages, files int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM sources)`).Scan(&messages, &files); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || files != 1 {
		t.Errorf("stored %d message(s) from %d file(s); want 1 from the full transcript only", messages, files)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM sessions`).Scan(&id); err != nil || id != "antigravity:conv-1" {
		t.Errorf("session id = %q, %v; want antigravity:conv-1", id, err)
	}
}
