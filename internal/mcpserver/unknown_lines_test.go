// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/mcpserver"
	"github.com/danPoku/kaectx/internal/store"
)

// serverWithUnknownLines ingests a Claude Code log whose lines are stamped
// at `when`, one of them of a type kae does not know, and returns a server
// for the log's project. The log is already ingested and old enough on disk
// that the index is otherwise up to date, so any warning is about the
// unknown line alone.
func serverWithUnknownLines(t *testing.T, when time.Time) *sdkmcp.ClientSession {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}

	ts := when.UTC().Format(time.RFC3339)
	log := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"How do I rotate the signing key?"},"sessionId":"sess-w","cwd":"/home/dev/proj","version":"9.9.0","timestamp":%q}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Generate a new key, then update the verifier."}]},"sessionId":"sess-w","cwd":"/home/dev/proj","version":"9.9.0","timestamp":%q}
{"type":"conversation-digest","digest":"key rotation","sessionId":"sess-w","version":"9.9.0","timestamp":%q}
`, ts, ts, ts)
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "proj", "sess-w.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(log), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(logPath, old, old)
	if err := ingest.Run(db, claudecode.New(), logPath); err != nil {
		t.Fatal(err)
	}

	srv, err := mcpserver.NewWithOptions(db, "/home/dev/proj", brokenEmbedder{}, mcpserver.Options{
		Roots:    []ingest.Root{{Agent: "claude-code", Dir: logDir, NewAdapter: func() ingest.Adapter { return claudecode.New() }}},
		LockPath: filepath.Join(t.TempDir(), "ingest.lock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// An agent searching a project whose recent logs held an unrecognised line
// type is told so, with the type, the agent and its version, even though
// every log file has been read.
func TestIndexWarningNamesUnrecognisedLogTypes(t *testing.T) {
	cs := serverWithUnknownLines(t, time.Now().Add(-2*time.Hour))

	for _, tool := range []string{"search_context", "recent_sessions"} {
		var out searchWithIndex
		args := map[string]any{}
		if tool == "search_context" {
			args["query"] = "signing key"
		}
		callTool(t, cs, tool, args, &out)
		if out.Index == nil {
			t.Fatalf("%s: index status missing", tool)
		}
		if out.Index.PendingFiles != 0 {
			t.Fatalf("%s: the log was ingested, yet %d file(s) are pending", tool, out.Index.PendingFiles)
		}
		for _, want := range []string{"conversation-digest", "claude-code 9.9.0", "1 line(s)", "kae doctor"} {
			if !strings.Contains(out.Index.Warning, want) {
				t.Errorf("%s: warning lacks %q: %q", tool, want, out.Index.Warning)
			}
		}
	}
}

// A type last seen long ago no longer warns: either an adapter update dealt
// with it or the agent stopped writing it.
func TestIndexWarningDropsOldUnrecognisedTypes(t *testing.T) {
	cs := serverWithUnknownLines(t, time.Now().Add(-30*24*time.Hour))
	var out searchWithIndex
	callTool(t, cs, "search_context", map[string]any{"query": "signing key"}, &out)
	if out.Index == nil {
		t.Fatal("index status missing")
	}
	if out.Index.Warning != "" {
		t.Errorf("a month-old unknown type should not warn: %q", out.Index.Warning)
	}
}
