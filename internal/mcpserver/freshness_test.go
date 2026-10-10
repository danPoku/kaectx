// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
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

type indexOut struct {
	LastIngestAt string `json:"last_ingest_at"`
	PendingFiles int    `json:"pending_files"`
	CatchingUp   bool   `json:"catching_up"`
	Warning      string `json:"warning"`
}

type searchWithIndex struct {
	Results []struct {
		SessionID string `json:"session_id"`
	} `json:"results"`
	Index *indexOut `json:"index"`
}

// staleServer starts a server over an EMPTY database whose agent log
// directory holds one finished session that was never ingested: the state
// a machine is in when nobody has run `ctx daemon` for a few days.
func staleServer(t *testing.T, lockPath string) *sdkmcp.ClientSession {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}

	fixture, err := os.ReadFile("../ingest/claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "proj", "session.jsonl")
	os.MkdirAll(filepath.Dir(logPath), 0755)
	if err := os.WriteFile(logPath, fixture, 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(logPath, old, old)

	srv, err := mcpserver.NewWithOptions(db, "/home/dev/proj", brokenEmbedder{}, mcpserver.Options{
		Roots:    []ingest.Root{{Agent: "claude-code", Dir: logDir, NewAdapter: func() ingest.Adapter { return claudecode.New() }}},
		LockPath: lockPath,
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

func TestServerCatchesUpOnUnreadLogsAtStartup(t *testing.T) {
	cs := staleServer(t, filepath.Join(t.TempDir(), "ingest.lock"))

	// The catch-up runs in the background, so poll briefly for it.
	var out searchWithIndex
	deadline := time.Now().Add(10 * time.Second)
	for {
		out = searchWithIndex{} // omitted fields must not carry over from the last poll
		callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &out)
		if len(out.Results) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(out.Results) == 0 {
		t.Fatal("search found nothing: the startup catch-up never ingested the unread log")
	}
	if out.Index == nil {
		t.Fatal("index status missing from search_context result")
	}
	if out.Index.PendingFiles != 0 || out.Index.Warning != "" {
		t.Errorf("index after catch-up = %+v, want nothing pending and no warning", *out.Index)
	}
}

func TestServerWarnsWhenIndexIsBehind(t *testing.T) {
	// Another process holds the catch-up lock, so this server can't catch
	// up, and must say so instead of answering as if the index were whole.
	lock := filepath.Join(t.TempDir(), "ingest.lock")
	release, err := ingest.Lock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cs := staleServer(t, lock)

	var out searchWithIndex
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &out)
	if out.Index == nil || out.Index.PendingFiles != 1 {
		t.Fatalf("index = %+v, want 1 pending file", out.Index)
	}
	if !strings.Contains(out.Index.Warning, "hasn't read yet") {
		t.Errorf("warning = %q, want it to say logs are unread", out.Index.Warning)
	}

	var recent struct {
		Index *indexOut `json:"index"`
	}
	callTool(t, cs, "recent_sessions", map[string]any{}, &recent)
	if recent.Index == nil || recent.Index.Warning == "" {
		t.Errorf("recent_sessions index = %+v, want the same warning", recent.Index)
	}
}

// The fixture session ran on 2026-09-20.
func TestToolsHonourSinceAndUntil(t *testing.T) {
	cs, _ := testServer(t)
	cases := []struct {
		name         string
		since, until string
		want         int
	}{
		{name: "window containing the session", since: "2026-09-20", until: "2026-09-20", want: 1},
		{name: "window after it", since: "2026-09-21", want: 0},
		{name: "window before it", until: "2026-09-19", want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := map[string]any{}
			if c.since != "" {
				args["since"] = c.since
			}
			if c.until != "" {
				args["until"] = c.until
			}

			var recent struct {
				Sessions []struct{ ID string } `json:"sessions"`
			}
			callTool(t, cs, "recent_sessions", args, &recent)
			if len(recent.Sessions) != c.want {
				t.Errorf("recent_sessions = %d sessions, want %d", len(recent.Sessions), c.want)
			}

			args["query"] = "WAL"
			var found searchWithIndex
			callTool(t, cs, "search_context", args, &found)
			if len(found.Results) != c.want {
				t.Errorf("search_context = %d results, want %d", len(found.Results), c.want)
			}
		})
	}

	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "search_context", Arguments: map[string]any{"query": "WAL", "since": "last week"},
	})
	if err == nil && !res.IsError {
		t.Error("search_context accepted since=\"last week\"; want an error naming the accepted formats")
	}
}
