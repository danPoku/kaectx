// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/mcpserver"
)

// secondServer connects another ctx MCP server to the same database, standing
// in for a second `ctx mcp` process: a different agent session working in the
// same project.
func secondServer(t *testing.T, db *sql.DB) *sdkmcp.ClientSession {
	t.Helper()
	srv, err := mcpserver.New(db, "/home/dev/proj", brokenEmbedder{})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	ctx := context.Background()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "second-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type pendingSearchRow struct {
	RunID    string `json:"run_id"`
	Query    string `json:"query"`
	NextStep string `json:"next_step"`
}

// pendingSearchesFrom reads pending_searches from either tool that carries it.
func pendingSearchesFrom(t *testing.T, cs *sdkmcp.ClientSession, tool string, args map[string]any) []pendingSearchRow {
	t.Helper()
	var out struct {
		PendingSearches []pendingSearchRow `json:"pending_searches"`
	}
	callTool(t, cs, tool, args, &out)
	if out.PendingSearches == nil {
		t.Fatalf("%s: pending_searches is null, want an array (empty when nothing is pending)", tool)
	}
	return out.PendingSearches
}

func searchRun(t *testing.T, cs *sdkmcp.ClientSession, query string) (runID, outcomeNextStep string) {
	t.Helper()
	var out struct {
		RunID           string `json:"run_id"`
		OutcomeNextStep string `json:"outcome_next_step"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": query}, &out)
	if out.RunID == "" {
		t.Fatalf("search_context(%q) returned no run_id", query)
	}
	return out.RunID, out.OutcomeNextStep
}

// #40: a search result must carry the call that records its own verdict.
func TestSearchContextOutcomeNextStepNamesItsRun(t *testing.T) {
	cs, _ := testServer(t)
	first, firstStep := searchRun(t, cs, "WAL")
	second, secondStep := searchRun(t, cs, "WAL")
	if first == second {
		t.Fatal("setup: two searches shared a run_id")
	}
	for _, c := range []struct{ runID, step string }{{first, firstStep}, {second, secondStep}} {
		if !strings.HasPrefix(c.step, "record_search_outcome(") || !strings.Contains(c.step, fmt.Sprintf("run_id=%q", c.runID)) {
			t.Errorf("outcome_next_step = %q, want a record_search_outcome call naming run %s", c.step, c.runID)
		}
	}
}

// #40: pending_searches is the calling session's own list. Both tools that
// carry it are checked for each rule, so they cannot drift apart.
func TestPendingSearchesBelongToTheServingProcess(t *testing.T) {
	tools := []struct {
		name string
		args map[string]any
	}{
		{"feedback_status", map[string]any{}},
		{"checkpoint_status", map[string]any{}},
	}
	for _, tool := range tools {
		t.Run(tool.name, func(t *testing.T) {
			first, db := testServer(t)
			second := secondServer(t, db)

			if got := pendingSearchesFrom(t, first, tool.name, tool.args); len(got) != 0 {
				t.Fatalf("before any search: pending_searches = %+v, want none", got)
			}

			older, _ := searchRun(t, first, "WAL")
			newer, _ := searchRun(t, first, "sqlite")

			got := pendingSearchesFrom(t, first, tool.name, tool.args)
			if len(got) != 2 || got[0].RunID != newer || got[1].RunID != older {
				t.Fatalf("pending_searches = %+v, want [%s %s] newest first", got, newer, older)
			}
			if got[0].Query != "sqlite" || !strings.Contains(got[0].NextStep, fmt.Sprintf("run_id=%q", newer)) {
				t.Errorf("pending search = %+v, want its query and a next_step naming its run", got[0])
			}

			// The other process ran neither search, so it is asked about neither.
			if other := pendingSearchesFrom(t, second, tool.name, tool.args); len(other) != 0 {
				t.Errorf("second process: pending_searches = %+v, want none", other)
			}

			// A recorded outcome removes the run, whichever process recorded it.
			var recorded struct{}
			callTool(t, second, "record_search_outcome", map[string]any{"run_id": older, "outcome": "helpful"}, &recorded)
			got = pendingSearchesFrom(t, first, tool.name, tool.args)
			if len(got) != 1 || got[0].RunID != newer {
				t.Fatalf("after recording %s: pending_searches = %+v, want only %s", older, got, newer)
			}
			callTool(t, first, "record_search_outcome", map[string]any{
				"run_id": newer, "outcome": "unhelpful", "reason": "ranking"}, &recorded)
			if got := pendingSearchesFrom(t, first, tool.name, tool.args); len(got) != 0 {
				t.Errorf("after recording both: pending_searches = %+v, want none", got)
			}
		})
	}
}

// #40: the list is capped like pending_feedback, however many searches ran.
func TestPendingSearchesAreCapped(t *testing.T) {
	cs, _ := testServer(t)
	const searches = 30
	var newest string
	for range searches {
		newest, _ = searchRun(t, cs, "WAL")
	}
	for _, c := range []struct {
		name string
		tool string
		args map[string]any
		want int
	}{
		{"checkpoint_status caps at 10", "checkpoint_status", map[string]any{}, 10},
		{"feedback_status defaults to 10", "feedback_status", map[string]any{}, 10},
		{"feedback_status honours a smaller limit", "feedback_status", map[string]any{"limit": 3}, 3},
		{"feedback_status caps limit at 25", "feedback_status", map[string]any{"limit": 500}, 25},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := pendingSearchesFrom(t, cs, c.tool, c.args)
			if len(got) != c.want {
				t.Fatalf("pending_searches has %d entries, want %d", len(got), c.want)
			}
			if got[0].RunID != newest {
				t.Errorf("first pending search = %s, want the newest run %s", got[0].RunID, newest)
			}
		})
	}
}

// A run pruned from telemetry can no longer take an outcome, so it must stop
// being listed rather than nag about a call that would fail.
func TestPendingSearchesDropPrunedRuns(t *testing.T) {
	cs, db := testServer(t)
	runID, _ := searchRun(t, cs, "WAL")
	if _, err := db.Exec(`DELETE FROM retrieval_runs WHERE id = ?`, runID); err != nil {
		t.Fatal(err)
	}
	if got := pendingSearchesFrom(t, cs, "feedback_status", map[string]any{}); len(got) != 0 {
		t.Errorf("pending_searches = %+v, want none after the run was pruned", got)
	}
}
