// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/store"
)

func startupLine(text, ts string) string {
	return `{"type":"user","message":{"role":"user","content":"` + text + `"},"sessionId":"s1","cwd":"/home/dev/proj","timestamp":"` + ts + `"}` + "\n"
}

// TestRunCatchesWritesRightAfterStartupScan is the regression test for issue
// #27. Run used to scan before adding its watches, so a line appended in
// between raised no event and waited for the fallback tick. The hook appends
// at exactly that moment; the fallback here is an hour, so only a watch
// event can pick the line up in time.
func TestRunCatchesWritesRightAfterStartupScan(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "s1.jsonl")
	if err := os.WriteFile(path, []byte(startupLine("first", "2026-09-20T10:00:00Z")), 0644); err != nil {
		t.Fatal(err)
	}

	appended := make(chan struct{})
	afterInitialScan = func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			f.WriteString(startupLine("second", "2026-09-20T10:00:01Z"))
			f.Close()
		}
		close(appended)
	}
	t.Cleanup(func() { afterInitialScan = func() {} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources := []Source{{Agent: "claude-code", Root: root, NewAdapter: func() ingest.Adapter { return claudecode.New() }}}
	go Run(ctx, db, sources, time.Hour, 20*time.Millisecond)
	<-appended

	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("have %d message(s) 3s after the post-scan append, want 2: the write raised no watch event", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
