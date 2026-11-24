// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/mcpserver"
	"github.com/danPoku/kaectx/internal/store"
)

// otherProjectDir is the second repo in these tests: the "frontend" to the
// fixture's "api". Work on one feature was split across the two.
const otherProjectDir = "/home/dev/frontend"

// crossProject is the fixture project plus a second project holding one
// session whose only exchange also mentions WAL.
type crossProject struct {
	db        *sql.DB
	here      *sdkmcp.ClientSession // server started in the fixture project
	hereID    int64
	otherID   int64
	otherSess string
	otherChnk int64
}

// serverAt connects a kae MCP server started in cwd to db.
func serverAt(t *testing.T, db *sql.DB, cwd string) *sdkmcp.ClientSession {
	t.Helper()
	srv, err := mcpserver.New(db, cwd, brokenEmbedder{})
	if err != nil {
		t.Fatalf("mcpserver.New(%s): %v", cwd, err)
	}
	ctx := context.Background()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func newCrossProject(t *testing.T) crossProject {
	t.Helper()
	here, db := testServer(t)
	hereID, err := store.ResolveProject(db, "/home/dev/proj")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := store.ResolveProject(db, otherProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	if otherID == hereID {
		t.Fatal("setup: the two directories resolved to one project")
	}
	const sess = "codex:frontend-1"
	exec := func(q string, args ...any) sql.Result {
		t.Helper()
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at)
		VALUES (?, 'codex', 'frontend-1', ?, '2026-09-21T09:00:00Z')`, sess, otherID)
	exec(`INSERT INTO messages(session_id, seq, role, content, created_at) VALUES
		(?, 0, 'user', 'Does the frontend need to know about WAL mode?', '2026-09-21T09:00:00Z'),
		(?, 1, 'assistant', 'No: WAL is a storage detail the API hides.', '2026-09-21T09:00:05Z')`, sess, sess)
	res := exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text)
		VALUES (?, 0, 1, 'User: Does the frontend need to know about WAL mode?
Assistant: No: WAL is a storage detail the API hides.')`, sess)
	chunkID, _ := res.LastInsertId()
	return crossProject{db: db, here: here, hereID: hereID, otherID: otherID, otherSess: sess, otherChnk: chunkID}
}

type scopedHit struct {
	Rank        int     `json:"rank"`
	ChunkID     int64   `json:"chunk_id"`
	SessionID   string  `json:"session_id"`
	Project     string  `json:"project"`
	Reliability float64 `json:"reliability"`
	NextStep    string  `json:"next_step"`
}

type scopedSearch struct {
	RunID   string      `json:"run_id"`
	Results []scopedHit `json:"results"`
}

func (s scopedSearch) hitFor(chunkID int64) (scopedHit, bool) {
	for _, h := range s.Results {
		if h.ChunkID == chunkID {
			return h, true
		}
	}
	return scopedHit{}, false
}

func projectName(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var name string
	if err := db.QueryRow(`SELECT name FROM projects WHERE id = ?`, id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// #32: project scope is the default and returns only this project's hits;
// scope=all returns both, each naming its project.
func TestSearchContextScope(t *testing.T) {
	cp := newCrossProject(t)
	hereName, otherName := projectName(t, cp.db, cp.hereID), projectName(t, cp.db, cp.otherID)

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"scope omitted", map[string]any{"query": "WAL"}},
		{"scope project", map[string]any{"query": "WAL", "scope": "project"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out scopedSearch
			callTool(t, cp.here, "search_context", c.args, &out)
			if len(out.Results) != 1 {
				t.Fatalf("results = %+v, want only this project's one hit", out.Results)
			}
			if h := out.Results[0]; h.SessionID == cp.otherSess || h.Project != hereName {
				t.Errorf("hit = %+v, want this project's own (%s)", h, hereName)
			}
		})
	}

	t.Run("scope all", func(t *testing.T) {
		var out scopedSearch
		callTool(t, cp.here, "search_context", map[string]any{"query": "WAL", "scope": "all"}, &out)
		if len(out.Results) != 2 {
			t.Fatalf("results = %+v, want one hit from each project", out.Results)
		}
		got := map[string]string{}
		for _, h := range out.Results {
			got[h.SessionID] = h.Project
		}
		if got[cp.otherSess] != otherName {
			t.Errorf("the other project's hit names project %q, want %q", got[cp.otherSess], otherName)
		}
		delete(got, cp.otherSess)
		for sess, project := range got {
			if project != hereName {
				t.Errorf("this project's hit %s names project %q, want %q", sess, project, hereName)
			}
		}
	})

	t.Run("unknown scope is an error", func(t *testing.T) {
		if msg := toolError(t, cp.here, "search_context", map[string]any{"query": "WAL", "scope": "everything"}); !strings.Contains(msg, "scope must be project or all") {
			t.Errorf("error = %q, want it to name the valid scopes", msg)
		}
	})
}

// #32: another project's chunk and session open only with the run_id and rank
// of the all-projects hit that returned them.
func TestCrossProjectReadNeedsTheHit(t *testing.T) {
	cp := newCrossProject(t)
	var all, own scopedSearch
	callTool(t, cp.here, "search_context", map[string]any{"query": "WAL", "scope": "all"}, &all)
	callTool(t, cp.here, "search_context", map[string]any{"query": "WAL"}, &own)
	hit, ok := all.hitFor(cp.otherChnk)
	if !ok {
		t.Fatalf("setup: scope=all did not return the other project's chunk: %+v", all.Results)
	}
	var ownRank int
	for _, h := range all.Results {
		if h.ChunkID != cp.otherChnk {
			ownRank = h.Rank
		}
	}

	t.Run("get_chunk with the hit's run_id and rank reads it and records the open", func(t *testing.T) {
		var out struct {
			ChunkID      int64  `json:"chunk_id"`
			SessionID    string `json:"session_id"`
			Text         string `json:"text"`
			OpenRecorded bool   `json:"open_recorded"`
			OpenInferred bool   `json:"open_inferred"`
		}
		callTool(t, cp.here, "get_chunk", map[string]any{"chunk_id": cp.otherChnk, "run_id": all.RunID, "rank": hit.Rank}, &out)
		if out.SessionID != cp.otherSess || !strings.Contains(out.Text, "storage detail") {
			t.Fatalf("chunk = %+v, want the other project's exchange", out)
		}
		if !out.OpenRecorded || out.OpenInferred {
			t.Errorf("open_recorded = %v, open_inferred = %v; want an exact recorded open", out.OpenRecorded, out.OpenInferred)
		}
		var opens int
		if err := cp.db.QueryRow(`SELECT open_count FROM retrieval_candidates WHERE run_id = ? AND rank = ?`, all.RunID, hit.Rank).Scan(&opens); err != nil {
			t.Fatal(err)
		}
		if opens != 1 {
			t.Errorf("open_count = %d, want 1", opens)
		}
	})

	refused := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"get_chunk with the chunk_id alone", "get_chunk", map[string]any{"chunk_id": cp.otherChnk}},
		{"get_chunk with another hit's rank from the same run", "get_chunk",
			map[string]any{"chunk_id": cp.otherChnk, "run_id": all.RunID, "rank": ownRank}},
		{"get_chunk with a project-scoped run", "get_chunk",
			map[string]any{"chunk_id": cp.otherChnk, "run_id": own.RunID, "rank": 1}},
		{"get_chunk with a made-up run", "get_chunk",
			map[string]any{"chunk_id": cp.otherChnk, "run_id": "no-such-run", "rank": 1}},
		{"get_session with the session_id alone", "get_session", map[string]any{"session_id": cp.otherSess}},
		{"get_session with another hit's rank from the same run", "get_session",
			map[string]any{"session_id": cp.otherSess, "run_id": all.RunID, "rank": ownRank}},
		{"get_session with a project-scoped run", "get_session",
			map[string]any{"session_id": cp.otherSess, "run_id": own.RunID, "rank": 1}},
	}
	for _, c := range refused {
		t.Run(c.name+" is the cross-project error", func(t *testing.T) {
			msg := toolError(t, cp.here, c.tool, c.args)
			if !strings.Contains(msg, "belongs to another project") {
				t.Errorf("error = %q, want the cross-project error", msg)
			}
			if strings.Contains(msg, "storage detail") {
				t.Errorf("the refusal leaked the other project's text: %q", msg)
			}
		})
	}

	t.Run("get_session with the hit's run_id and rank reads it", func(t *testing.T) {
		var out struct {
			Messages []struct {
				Seq     int    `json:"seq"`
				Content string `json:"content"`
			} `json:"messages"`
			OpenRecorded bool `json:"open_recorded"`
		}
		callTool(t, cp.here, "get_session", map[string]any{
			"session_id": cp.otherSess, "from_seq": 0, "to_seq": 1, "run_id": all.RunID, "rank": hit.Rank}, &out)
		if len(out.Messages) != 2 || !strings.Contains(out.Messages[1].Content, "storage detail") {
			t.Fatalf("messages = %+v, want the other project's two messages", out.Messages)
		}
		if !out.OpenRecorded {
			t.Error("open_recorded = false, want the read recorded as an open of the hit")
		}
	})

	// A run made by one project is no key for a server started in a third.
	t.Run("another project's server cannot use this project's run", func(t *testing.T) {
		if _, err := store.ResolveProject(cp.db, "/home/dev/third"); err != nil {
			t.Fatal(err)
		}
		third := serverAt(t, cp.db, "/home/dev/third")
		msg := toolError(t, third, "get_chunk", map[string]any{"chunk_id": cp.otherChnk, "run_id": all.RunID, "rank": hit.Rank})
		if !strings.Contains(msg, "belongs to another project") {
			t.Errorf("error = %q, want the cross-project error", msg)
		}
	})
}

func observationCount(t *testing.T, db *sql.DB, projectID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// #32: a label on another project's hit is filed under that project. The
// calling project's observation count does not move.
func TestCrossProjectLabelFollowsTheHit(t *testing.T) {
	cp := newCrossProject(t)
	var all scopedSearch
	callTool(t, cp.here, "search_context", map[string]any{"query": "WAL", "scope": "all"}, &all)
	hit, ok := all.hitFor(cp.otherChnk)
	if !ok {
		t.Fatalf("setup: no hit for the other project's chunk: %+v", all.Results)
	}
	hereBefore, otherBefore := observationCount(t, cp.db, cp.hereID), observationCount(t, cp.db, cp.otherID)

	var recorded struct {
		Recorded     bool `json:"recorded"`
		Observations int  `json:"observations"`
	}
	callTool(t, cp.here, "record_retrieval_outcome", map[string]any{"run_id": all.RunID, "rank": hit.Rank, "outcome": "stale"}, &recorded)
	if !recorded.Recorded || recorded.Observations != 1 {
		t.Fatalf("result = %+v, want recorded with 1 observation counted in the hit's project", recorded)
	}
	if got := observationCount(t, cp.db, cp.hereID); got != hereBefore {
		t.Errorf("calling project's observations went from %d to %d, want unchanged", hereBefore, got)
	}
	if got := observationCount(t, cp.db, cp.otherID); got != otherBefore+1 {
		t.Errorf("hit project's observations went from %d to %d, want one more", otherBefore, got)
	}
	var stored int64
	if err := cp.db.QueryRow(`SELECT project_id FROM memory_observations WHERE run_id = ? AND candidate_rank = ?`, all.RunID, hit.Rank).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != cp.otherID {
		t.Errorf("memory_observations.project_id = %d, want the hit's project %d", stored, cp.otherID)
	}

	// The candidate itself records where the hit came from; this project's
	// own hit in the same run records this project.
	rows, err := cp.db.Query(`SELECT chunk_id, project_id FROM retrieval_candidates WHERE run_id = ?`, all.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var chunkID int64
		var project sql.NullInt64
		if err := rows.Scan(&chunkID, &project); err != nil {
			t.Fatal(err)
		}
		want := cp.hereID
		if chunkID == cp.otherChnk {
			want = cp.otherID
		}
		if !project.Valid || project.Int64 != want {
			t.Errorf("candidate for chunk %d has project_id %+v, want %d", chunkID, project, want)
		}
	}
}

// #32: under risk_aware, a hit is assessed against its own project. Its
// reliability from a scope=all search elsewhere equals its reliability from a
// project-scoped search at home, and labels in the searching project that
// happen to share nothing with it leave it alone.
func TestCrossProjectReliabilityIsTheHitsOwn(t *testing.T) {
	cp := newCrossProject(t)
	there := serverAt(t, cp.db, otherProjectDir)

	// Three stale labels at home make the frontend exchange credibly bad.
	for i := 0; i < 3; i++ {
		var s scopedSearch
		callTool(t, there, "search_context", map[string]any{"query": "WAL"}, &s)
		if len(s.Results) != 1 || s.Results[0].ChunkID != cp.otherChnk {
			t.Fatalf("setup: home search = %+v, want the one frontend hit", s.Results)
		}
		var recorded struct{}
		callTool(t, there, "record_retrieval_outcome", map[string]any{"run_id": s.RunID, "rank": 1, "outcome": "stale"}, &recorded)
	}
	// Three helpful labels on this project's own hit: were observations
	// pooled across projects, they would dilute the frontend hit's record.
	for i := 0; i < 3; i++ {
		var s scopedSearch
		callTool(t, cp.here, "search_context", map[string]any{"query": "WAL"}, &s)
		var recorded struct{}
		callTool(t, cp.here, "record_retrieval_outcome", map[string]any{"run_id": s.RunID, "rank": 1, "outcome": "helpful"}, &recorded)
	}

	var home, all scopedSearch
	callTool(t, there, "search_context", map[string]any{"query": "WAL", "risk_aware": true}, &home)
	callTool(t, cp.here, "search_context", map[string]any{"query": "WAL", "scope": "all", "risk_aware": true}, &all)

	atHome, ok := home.hitFor(cp.otherChnk)
	if !ok {
		t.Fatalf("home search lost its hit: %+v", home.Results)
	}
	away, ok := all.hitFor(cp.otherChnk)
	if !ok {
		t.Fatalf("scope=all lost the frontend hit: %+v", all.Results)
	}
	if atHome.Reliability >= 1 || atHome.Reliability <= 0 {
		t.Fatalf("setup: home reliability = %v, want a discount from three stale labels", atHome.Reliability)
	}
	if away.Reliability != atHome.Reliability {
		t.Errorf("reliability under scope=all = %v, want the home project's %v", away.Reliability, atHome.Reliability)
	}
	// And this project's own hit keeps its own clean record in the same run.
	for _, h := range all.Results {
		if h.ChunkID != cp.otherChnk && h.Reliability != 1 {
			t.Errorf("this project's hit has reliability %v, want 1 (three helpful labels, none of the frontend's)", h.Reliability)
		}
	}
	// The credibly stale hit is ranked below the clean one.
	if all.Results[len(all.Results)-1].ChunkID != cp.otherChnk {
		t.Errorf("results = %+v, want the discounted frontend hit last", all.Results)
	}
}
