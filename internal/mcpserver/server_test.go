// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/mcpserver"
	"github.com/danPoku/kaectx/internal/store"
)

// brokenEmbedder always fails, so these tests are hermetic: they must not
// depend on whether some Ollama instance happens to be reachable at
// embed.DefaultBaseURL on the machine running them. mcpserver.New takes an
// embed.Embedder explicitly for exactly this reason.
type brokenEmbedder struct{}

func (brokenEmbedder) Model() string { return "broken" }
func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("no embedder configured for this test")
}

// testServer wires up a real ctx MCP server against a temp DB seeded with
// the claudecode fixture (cwd "/home/dev/proj"), connected in-process to a
// real MCP client — no subprocess, but a genuine client/server MCP
// round-trip (initialize handshake, JSON schema validation, wire encoding)
// rather than calling handler functions directly.
func testServer(t *testing.T) (*sdkmcp.ClientSession, *sql.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	fixture, err := os.ReadFile("../ingest/claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionPath, fixture, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := ingest.Run(db, claudecode.New(), sessionPath); err != nil {
		t.Fatalf("ingest.Run: %v", err)
	}

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

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { clientSession.Close() })

	return clientSession, db
}

// callTool calls name with args and decodes the tool's auto-populated JSON
// text content into out — the same wire path a real MCP client goes
// through, not a shortcut to the handler function.
func callTool(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) returned a tool error: %+v", name, res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%s): no content in result", name)
	}
	tc, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%s): content[0] is %T, want *TextContent", name, res.Content[0])
	}
	if err := json.Unmarshal([]byte(tc.Text), out); err != nil {
		t.Fatalf("CallTool(%s): decode result: %v\nraw: %s", name, err, tc.Text)
	}
}

func TestSearchContext(t *testing.T) {
	cs, _ := testServer(t)

	var out struct {
		Results []struct {
			Rank      int    `json:"rank"`
			ChunkID   int64  `json:"chunk_id"`
			SessionID string `json:"session_id"`
			Agent     string `json:"agent"`
			Snippet   string `json:"snippet"`
			NextStep  string `json:"next_step"`
		} `json:"results"`
		UsedSemanticSearch bool   `json:"used_semantic_search"`
		RunID              string `json:"run_id"`
		RetrievalRunID     string `json:"retrieval_run_id"`
		TelemetryRecorded  bool   `json:"telemetry_recorded"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &out)

	if len(out.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(out.Results))
	}
	if out.Results[0].Agent != "claude-code" {
		t.Errorf("Agent = %q, want claude-code", out.Results[0].Agent)
	}
	if out.Results[0].Rank != 1 {
		t.Errorf("Rank = %d, want 1", out.Results[0].Rank)
	}
	if !strings.Contains(out.Results[0].Snippet, "WAL") {
		t.Errorf("Snippet = %q, want it to contain WAL", out.Results[0].Snippet)
	}
	// testServer wires up brokenEmbedder, so search_context must
	// transparently fall back to keyword-only rather than error — proof
	// search.Best's fallback is actually wired through the MCP layer, not
	// just tested in isolation.
	if out.UsedSemanticSearch {
		t.Error("UsedSemanticSearch = true, want false (brokenEmbedder always fails)")
	}
	if out.RunID == "" {
		t.Error("RunID is empty, want recorded telemetry")
	}
	// retrieval_run_id is the deprecated name, kept for one release.
	if out.RetrievalRunID != out.RunID {
		t.Errorf("retrieval_run_id = %q, want the same value as run_id %q", out.RetrievalRunID, out.RunID)
	}
	if !out.TelemetryRecorded {
		t.Error("TelemetryRecorded = false, want true")
	}
	// The hint must carry all three arguments, or following it records no
	// exact open.
	wantStep := fmt.Sprintf("get_chunk(chunk_id=%d, run_id=%q, rank=1)", out.Results[0].ChunkID, out.RunID)
	if out.Results[0].NextStep != wantStep {
		t.Errorf("NextStep = %q, want %q", out.Results[0].NextStep, wantStep)
	}
}

func TestRecordRetrievalOutcomeUsesRunAndRank(t *testing.T) {
	cs, db := testServer(t)
	var out struct {
		Recorded     bool    `json:"recorded"`
		EvidenceKey  string  `json:"evidence_key"`
		Observations int     `json:"observations"`
		Reliability  float64 `json:"reliability"`
		Neutral      bool    `json:"neutral"`
	}
	fakeSecret := "ghp_" + strings.Repeat("A", 36)
	var runIDs []string
	for i, outcome := range []string{"helpful", "incorrect", "incorrect"} {
		var searchOut struct {
			RetrievalRunID string `json:"retrieval_run_id"`
		}
		callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &searchOut)
		if searchOut.RetrievalRunID == "" {
			t.Fatal("search did not return a retrieval run id")
		}
		runIDs = append(runIDs, searchOut.RetrievalRunID)
		callTool(t, cs, "record_retrieval_outcome", map[string]any{
			"run_id": searchOut.RetrievalRunID, "rank": 1,
			"outcome": outcome, "note": "token " + fakeSecret,
		}, &out)
		if !out.Recorded || out.Observations != i+1 {
			t.Fatalf("outcome %d result = %+v", i, out)
		}
	}
	if out.Neutral || out.Reliability >= 1 || len(out.EvidenceKey) != 64 {
		t.Errorf("final assessment = %+v, want non-neutral bounded reliability", out)
	}
	var note string
	if err := db.QueryRow(`SELECT note FROM memory_observations ORDER BY id LIMIT 1`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(note, fakeSecret) || !strings.Contains(note, "[REDACTED]") {
		t.Errorf("stored note was not redacted: %q", note)
	}
	// The label is anchored to the labelled exchange's first message, so it
	// survives the chunk being rebuilt with other bounds.
	var anchorSession string
	var anchorSeq, candidateFirst int
	if err := db.QueryRow(`SELECT mo.session_id, mo.anchor_seq, rc.first_seq FROM memory_observations mo
		JOIN retrieval_candidates rc ON rc.run_id = mo.run_id AND rc.rank = mo.candidate_rank
		ORDER BY mo.id LIMIT 1`).Scan(&anchorSession, &anchorSeq, &candidateFirst); err != nil {
		t.Fatal(err)
	}
	if anchorSession != "claude-code:sess-1" || anchorSeq != candidateFirst {
		t.Errorf("anchor = %s@%d, want claude-code:sess-1@%d", anchorSession, anchorSeq, candidateFirst)
	}

	// A second opinion from the same retrieval replaces that opinion instead
	// of manufacturing another observation.
	callTool(t, cs, "record_retrieval_outcome", map[string]any{
		"run_id": runIDs[0], "rank": 1, "outcome": "helpful",
	}, &out)
	if out.Observations != 3 {
		t.Errorf("duplicate feedback produced %d observations, want 3", out.Observations)
	}

	// Bounded telemetry may disappear, but the actuarial experience must keep
	// its stable evidence identity and merely lose the optional provenance.
	if _, err := db.Exec(`DELETE FROM retrieval_runs WHERE id=?`, runIDs[0]); err != nil {
		t.Fatal(err)
	}
	var runID sql.NullString
	var rank sql.NullInt64
	var evidence string
	if err := db.QueryRow(`SELECT run_id,candidate_rank,evidence_key FROM memory_observations ORDER BY id LIMIT 1`).Scan(&runID, &rank, &evidence); err != nil {
		t.Fatal(err)
	}
	if runID.Valid || rank.Valid || evidence == "" {
		t.Errorf("pruned observation = run %v rank %v evidence %q", runID, rank, evidence)
	}
}

func TestRecordRetrievalOutcomeRequiresExactTarget(t *testing.T) {
	cs, _ := testServer(t)
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "record_retrieval_outcome",
		Arguments: map[string]any{"chunk_id": 1, "outcome": "helpful"},
	})
	if err == nil && !res.IsError {
		t.Fatal("chunk-only outcome succeeded, want an exact-target error")
	}
}

func TestRecentSessions(t *testing.T) {
	cs, _ := testServer(t)

	var out struct {
		Sessions []struct {
			ID    string `json:"id"`
			Agent string `json:"agent"`
			Title string `json:"title"`
		} `json:"sessions"`
	}
	callTool(t, cs, "recent_sessions", map[string]any{}, &out)

	if len(out.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(out.Sessions))
	}
	if out.Sessions[0].ID != "claude-code:sess-1" {
		t.Errorf("ID = %q, want claude-code:sess-1", out.Sessions[0].ID)
	}
	if out.Sessions[0].Title != "WAL mode question" {
		t.Errorf("Title = %q, want the ai-title event's value", out.Sessions[0].Title)
	}
}

func TestGetSessionPagesAndCaps(t *testing.T) {
	cs, _ := testServer(t)

	var out struct {
		Messages []struct {
			Seq     int    `json:"seq"`
			Role    string `json:"role"`
			Kind    string `json:"kind"`
			Content string `json:"content"`
		} `json:"messages"`
		Truncated   bool `json:"truncated"`
		NextFromSeq *int `json:"next_from_seq"`
	}
	callTool(t, cs, "get_session", map[string]any{"session_id": "claude-code:sess-1"}, &out)

	if len(out.Messages) == 0 {
		t.Fatal("messages is empty, want the fixture's messages")
	}
	for _, m := range out.Messages {
		if m.Kind == "thinking" {
			t.Errorf("message seq=%d kind=thinking leaked through — GetSessionMessages excludes reasoning traces", m.Seq)
		}
	}

	// The fixture has 8 messages total (well under the 200-row cap), so a
	// default call must NOT report truncation.
	if out.Truncated {
		t.Errorf("Truncated = true for a small session, want false")
	}
}

func TestSessionsTouching(t *testing.T) {
	cs, db := testServer(t)

	// The claudecode fixture's tool calls don't touch a file with the
	// "file_path" shape file_touches extraction looks for (it uses Read
	// with a "path" key and Bash), so seed a file_touches row directly to
	// test the tool's own query/response shape rather than re-testing
	// extraction (already covered in internal/ingest's own tests).
	if _, err := db.Exec(`INSERT INTO file_touches(session_id, project_id, path, action, created_at)
	                       VALUES ('claude-code:sess-1', (SELECT project_id FROM sessions WHERE id='claude-code:sess-1'), 'main.go', 'edit', '2026-09-20T10:00:00Z')`); err != nil {
		t.Fatalf("seed file_touches: %v", err)
	}

	var out struct {
		Sessions []struct {
			SessionID string   `json:"session_id"`
			Agent     string   `json:"agent"`
			Actions   []string `json:"actions"`
			Touches   int      `json:"touches"`
		} `json:"sessions"`
	}
	callTool(t, cs, "sessions_touching", map[string]any{"path": "main.go"}, &out)

	if len(out.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(out.Sessions))
	}
	if out.Sessions[0].SessionID != "claude-code:sess-1" {
		t.Errorf("SessionID = %q, want claude-code:sess-1", out.Sessions[0].SessionID)
	}
	if out.Sessions[0].Touches != 1 {
		t.Errorf("Touches = %d, want 1", out.Sessions[0].Touches)
	}
}

func TestExplainFile(t *testing.T) {
	cs, db := testServer(t)

	if _, err := db.Exec(`UPDATE sessions
	                         SET git_branch = 'feature/memory',
	                             starting_commit = '1111111111111111111111111111111111111111',
	                             ending_commit = '2222222222222222222222222222222222222222'
	                       WHERE id = 'claude-code:sess-1'`); err != nil {
		t.Fatalf("seed git span: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO file_touches(session_id, project_id, path, action, created_at)
	                       VALUES ('claude-code:sess-1', (SELECT project_id FROM sessions WHERE id='claude-code:sess-1'), 'main.go', 'edit', '2026-09-20T10:00:00Z')`); err != nil {
		t.Fatalf("seed file_touches: %v", err)
	}

	var out struct {
		Path     string `json:"path"`
		Sessions []struct {
			SessionID      string   `json:"session_id"`
			Agent          string   `json:"agent"`
			GitBranch      string   `json:"git_branch"`
			StartingCommit string   `json:"starting_commit"`
			EndingCommit   string   `json:"ending_commit"`
			Actions        []string `json:"actions"`
			ChunkID        *int64   `json:"chunk_id"`
			Preview        string   `json:"preview"`
		} `json:"sessions"`
	}
	callTool(t, cs, "explain_file", map[string]any{"path": "main.go"}, &out)

	if out.Path != "main.go" {
		t.Errorf("Path = %q, want main.go", out.Path)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(out.Sessions))
	}
	s := out.Sessions[0]
	if s.SessionID != "claude-code:sess-1" || s.Agent != "claude-code" {
		t.Errorf("session/agent = %q/%q", s.SessionID, s.Agent)
	}
	if s.GitBranch != "feature/memory" || s.StartingCommit == "" || s.EndingCommit == "" {
		t.Errorf("git fields = branch %q start %q end %q", s.GitBranch, s.StartingCommit, s.EndingCommit)
	}
	if len(s.Actions) != 1 || s.Actions[0] != "edit" {
		t.Errorf("Actions = %v, want [edit]", s.Actions)
	}
	if s.ChunkID == nil || s.Preview == "" {
		t.Errorf("ChunkID/Preview = %v/%q, want a follow-up chunk and preview", s.ChunkID, s.Preview)
	}
}

// A file nothing touched must come back as an empty list (not null), which
// strict MCP clients validate against the tool's output schema; the limit is
// capped; and an absolute path is accepted and rebased to project-relative.
func TestExplainFileEmptyLimitAndAbsolutePath(t *testing.T) {
	cs, db := testServer(t)

	var empty struct {
		Path     string `json:"path"`
		Sessions []any  `json:"sessions"`
	}
	callTool(t, cs, "explain_file", map[string]any{"path": "./nothing/here.go", "limit": 100000}, &empty)
	if empty.Path != "nothing/here.go" || empty.Sessions == nil || len(empty.Sessions) != 0 {
		t.Errorf("empty result = %+v, want normalised path and a non-nil empty list", empty)
	}

	if _, err := db.Exec(`INSERT INTO file_touches(session_id, project_id, path, action, created_at)
	                       VALUES ('claude-code:sess-1', (SELECT project_id FROM sessions WHERE id='claude-code:sess-1'), 'main.go', 'edit', '2026-09-20T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Sessions []struct {
			Actions []string `json:"actions"`
			Changes int      `json:"changes"`
		} `json:"sessions"`
	}
	callTool(t, cs, "explain_file", map[string]any{"path": "/home/dev/proj/main.go"}, &out)
	if len(out.Sessions) != 1 || out.Sessions[0].Changes != 1 {
		t.Errorf("absolute path result = %+v, want the one edited session", out.Sessions)
	}
}

func TestSaveNoteAndSearchNotes(t *testing.T) {
	cs, _ := testServer(t)

	var saveOut struct {
		ID int64 `json:"id"`
	}
	callTool(t, cs, "save_note", map[string]any{
		"kind": "decision", "title": "Use WAL mode", "body": "WAL avoids writer/reader blocking.",
		"tags": []string{"sqlite"},
	}, &saveOut)
	if saveOut.ID == 0 {
		t.Fatal("save_note returned id=0, want a real row id")
	}

	var searchOut struct {
		Notes []struct {
			ID    int64    `json:"id"`
			Kind  string   `json:"kind"`
			Title string   `json:"title"`
			Tags  []string `json:"tags"`
			Agent string   `json:"agent"`
		} `json:"notes"`
	}
	callTool(t, cs, "search_notes", map[string]any{"query": "WAL"}, &searchOut)

	if len(searchOut.Notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(searchOut.Notes))
	}
	n := searchOut.Notes[0]
	if n.ID != saveOut.ID {
		t.Errorf("ID = %d, want %d", n.ID, saveOut.ID)
	}
	if n.Kind != "decision" || n.Title != "Use WAL mode" {
		t.Errorf("note = %+v, want kind=decision title=%q", n, "Use WAL mode")
	}
	if len(n.Tags) != 1 || n.Tags[0] != "sqlite" {
		t.Errorf("Tags = %v, want [sqlite]", n.Tags)
	}
	// The test client identifies itself as "test-client" during the MCP
	// handshake — proof save_note actually reads the caller's ClientInfo
	// rather than hardcoding an agent name.
	if n.Agent != "test-client" {
		t.Errorf("Agent = %q, want test-client (from the MCP client's own Implementation.Name)", n.Agent)
	}
}

func TestSaveNoteRejectsInvalidKind(t *testing.T) {
	cs, _ := testServer(t)

	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "save_note",
		Arguments: map[string]any{"kind": "not-a-real-kind", "title": "x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Error("save_note with an invalid kind: IsError = false, want true")
	}
}

// seedMessage inserts one message straight into the fixture session, so a
// test can build a transcript of exactly the shape it needs (a 50k-character
// tool result, a run of long replies) without a giant JSONL fixture.
func seedMessage(t *testing.T, db *sql.DB, seq int, role, kind, content string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO messages(session_id, seq, role, kind, content) VALUES ('claude-code:sess-1', ?, ?, ?, ?)`,
		seq, role, kind, content); err != nil {
		t.Fatalf("seed message %d: %v", seq, err)
	}
}

type sessionPage struct {
	Messages []struct {
		Seq              int    `json:"seq"`
		Kind             string `json:"kind"`
		Content          string `json:"content"`
		ContentTruncated bool   `json:"content_truncated"`
		FullChars        int    `json:"full_chars"`
	} `json:"messages"`
	Truncated   bool `json:"truncated"`
	NextFromSeq *int `json:"next_from_seq"`
}

func TestSearchContextReturnsMessageRange(t *testing.T) {
	cs, db := testServer(t)

	var out struct {
		Results []struct {
			ChunkID  int64 `json:"chunk_id"`
			FirstSeq int   `json:"first_seq"`
			LastSeq  int   `json:"last_seq"`
		} `json:"results"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &out)
	if len(out.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(out.Results))
	}

	var wantFirst, wantLast int
	if err := db.QueryRow(`SELECT first_seq, last_seq FROM chunks WHERE id = ?`, out.Results[0].ChunkID).
		Scan(&wantFirst, &wantLast); err != nil {
		t.Fatalf("read chunk row: %v", err)
	}
	if out.Results[0].FirstSeq != wantFirst || out.Results[0].LastSeq != wantLast {
		t.Errorf("range = %d-%d, want %d-%d (the chunk's own first_seq/last_seq)",
			out.Results[0].FirstSeq, out.Results[0].LastSeq, wantFirst, wantLast)
	}
	if out.Results[0].LastSeq <= out.Results[0].FirstSeq {
		t.Errorf("range %d-%d should span the whole exchange, not a single message", out.Results[0].FirstSeq, out.Results[0].LastSeq)
	}
}

// ageRuns moves every recorded search back in time by modifier (an SQLite
// date modifier such as "-10 minutes"), so a test can set up searches that
// happened apart without sleeping.
func ageRuns(t *testing.T, db *sql.DB, modifier string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE retrieval_runs
		SET created_at=strftime('%Y-%m-%dT%H:%M:%fZ', created_at, ?)`, modifier); err != nil {
		t.Fatalf("age runs: %v", err)
	}
}

// inferredRead calls get_chunk with chunk_id alone and reports whether an
// open was recorded and against which run.
func inferredRead(t *testing.T, cs *sdkmcp.ClientSession, chunkID int64) (recorded bool, runID string) {
	t.Helper()
	var out struct {
		OpenRecorded   bool `json:"open_recorded"`
		FeedbackTarget *struct {
			RunID string `json:"run_id"`
		} `json:"feedback_target"`
	}
	callTool(t, cs, "get_chunk", map[string]any{"chunk_id": chunkID}, &out)
	if out.FeedbackTarget != nil {
		runID = out.FeedbackTarget.RunID
	}
	return out.OpenRecorded, runID
}

// TestGetChunkKeepsEveryOpen is the regression test for issue #20: each open
// used to overwrite the candidate's opened_at, so only the last survived.
func TestGetChunkKeepsEveryOpen(t *testing.T) {
	cs, db := testServer(t)
	var hit struct {
		RunID   string `json:"run_id"`
		Results []struct {
			ChunkID int64 `json:"chunk_id"`
		} `json:"results"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &hit)
	exact := map[string]any{"chunk_id": hit.Results[0].ChunkID, "run_id": hit.RunID, "rank": 1}
	// No pauses between calls: opens landing in the same millisecond must
	// still get distinct, ordered times.
	var out struct{}
	callTool(t, cs, "get_chunk", exact, &out)
	callTool(t, cs, "get_chunk", exact, &out)
	callTool(t, cs, "get_chunk", map[string]any{"chunk_id": hit.Results[0].ChunkID}, &out) // inferred

	rows, err := db.Query(`SELECT opened_at, inferred, backfilled FROM retrieval_open_events
		WHERE run_id = ? AND rank = 1 ORDER BY id`, hit.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type event struct {
		at                   string
		inferred, backfilled bool
	}
	var got []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.at, &e.inferred, &e.backfilled); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) != 3 {
		t.Fatalf("open events = %+v, want 3", got)
	}
	if !(got[0].at < got[1].at && got[1].at < got[2].at) {
		t.Errorf("open times = %s, %s, %s; want distinct and increasing", got[0].at, got[1].at, got[2].at)
	}
	var last string
	if err := db.QueryRow(`SELECT opened_at FROM retrieval_candidates WHERE run_id = ? AND rank = 1`, hit.RunID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != got[2].at {
		t.Errorf("candidate opened_at = %s, want the last event's %s", last, got[2].at)
	}
	if got[0].inferred || got[1].inferred || !got[2].inferred || got[0].backfilled {
		t.Errorf("events = %+v, want exact, exact, inferred and none backfilled", got)
	}

	// Events go with their run when retention prunes it.
	if _, err := db.Exec(`DELETE FROM retrieval_runs WHERE id = ?`, hit.RunID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM retrieval_open_events WHERE run_id = ?`, hit.RunID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d open events outlived their run, want 0", left)
	}
}

// TestGetChunkInferenceAbstains covers the cases where a chunk_id-only read
// must record nothing rather than guess which search it came from.
func TestGetChunkInferenceAbstains(t *testing.T) {
	search := func(t *testing.T, cs *sdkmcp.ClientSession) (chunkID int64, runID string) {
		t.Helper()
		var out struct {
			RunID   string `json:"run_id"`
			Results []struct {
				ChunkID int64 `json:"chunk_id"`
			} `json:"results"`
		}
		callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &out)
		if len(out.Results) != 1 {
			t.Fatalf("search results = %d, want 1", len(out.Results))
		}
		return out.Results[0].ChunkID, out.RunID
	}

	t.Run("two searches within the gap", func(t *testing.T) {
		cs, db := testServer(t)
		chunkID, _ := search(t, cs)
		ageRuns(t, db, "-2 minutes")
		search(t, cs)
		if recorded, runID := inferredRead(t, cs, chunkID); recorded {
			t.Errorf("open recorded against %q, want none: either search could be the source", runID)
		}
	})

	t.Run("search older than the window", func(t *testing.T) {
		cs, db := testServer(t)
		chunkID, _ := search(t, cs)
		ageRuns(t, db, "-25 hours")
		if recorded, runID := inferredRead(t, cs, chunkID); recorded {
			t.Errorf("open recorded against %q, want none: the only search is over a day old", runID)
		}
	})

	for _, surface := range []string{"cli", "internal"} {
		t.Run(surface+" searches are not matched", func(t *testing.T) {
			cs, db := testServer(t)
			chunkID, runID := search(t, cs)
			if _, err := db.Exec(`UPDATE retrieval_runs SET surface=? WHERE id=?`, surface, runID); err != nil {
				t.Fatal(err)
			}
			if recorded, got := inferredRead(t, cs, chunkID); recorded {
				t.Errorf("open recorded against %s search %q, want none", surface, got)
			}
		})
	}
}

func TestGetChunk(t *testing.T) {
	cs, db := testServer(t)

	var hit struct {
		RetrievalRunID string `json:"retrieval_run_id"`
		Results        []struct {
			ChunkID int64 `json:"chunk_id"`
			Rank    int   `json:"rank"`
		} `json:"results"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &hit)
	if len(hit.Results) != 1 {
		t.Fatalf("search results = %d, want 1", len(hit.Results))
	}

	var out struct {
		ChunkID        int64  `json:"chunk_id"`
		SessionID      string `json:"session_id"`
		Agent          string `json:"agent"`
		FirstSeq       int    `json:"first_seq"`
		LastSeq        int    `json:"last_seq"`
		Text           string `json:"text"`
		OpenRecorded   bool   `json:"open_recorded"`
		OpenInferred   bool   `json:"open_inferred"`
		NextStep       string `json:"next_step"`
		FeedbackTarget *struct {
			RunID string `json:"run_id"`
			Rank  int    `json:"rank"`
		} `json:"feedback_target"`
	}
	callTool(t, cs, "get_chunk", map[string]any{
		"chunk_id": hit.Results[0].ChunkID,
		"run_id":   hit.RetrievalRunID,
		"rank":     hit.Results[0].Rank,
	}, &out)
	if out.SessionID != "claude-code:sess-1" || out.Agent != "claude-code" {
		t.Errorf("session/agent = %q/%q", out.SessionID, out.Agent)
	}
	if !strings.Contains(out.Text, "User: How do I open the sqlite database with WAL mode?") ||
		!strings.Contains(out.Text, "Assistant: Open it with a DSN") {
		t.Errorf("Text = %q, want the user turn AND the assistant reply", out.Text)
	}
	if !out.OpenRecorded || out.FeedbackTarget == nil || out.FeedbackTarget.RunID != hit.RetrievalRunID || out.FeedbackTarget.Rank != 1 {
		t.Fatalf("feedback target = %+v, want the search run at rank 1", out.FeedbackTarget)
	}
	if out.OpenInferred {
		t.Error("OpenInferred = true for a call that passed run_id and rank, want an exact open")
	}
	wantStep := fmt.Sprintf("record_retrieval_outcome(run_id=%q, rank=1,", hit.RetrievalRunID)
	if !strings.Contains(out.NextStep, wantStep) {
		t.Errorf("NextStep = %q, want it to contain %q", out.NextStep, wantStep)
	}
	var openedAt sql.NullString
	var openCount, inferredCount int
	if err := db.QueryRow(`SELECT opened_at,open_count,inferred_open_count FROM retrieval_candidates
		WHERE run_id=? AND rank=?`, out.FeedbackTarget.RunID, out.FeedbackTarget.Rank).Scan(&openedAt, &openCount, &inferredCount); err != nil {
		t.Fatal(err)
	}
	if !openedAt.Valid || openCount != 1 || inferredCount != 0 {
		t.Errorf("open telemetry = %v count %d inferred %d, want timestamp, count 1, inferred 0", openedAt, openCount, inferredCount)
	}

	t.Run("chunk_id alone is attributed to the latest search", func(t *testing.T) {
		// A second search returns the same chunk; the read belongs to it,
		// not to the older one above. The older one is pushed back past
		// the ambiguity gap so the two are not near-simultaneous.
		ageRuns(t, db, "-10 minutes")
		var latest struct {
			RunID string `json:"run_id"`
		}
		callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &latest)
		var direct struct {
			Text           string `json:"text"`
			OpenRecorded   bool   `json:"open_recorded"`
			OpenInferred   bool   `json:"open_inferred"`
			NextStep       string `json:"next_step"`
			FeedbackTarget *struct {
				RunID string `json:"run_id"`
				Rank  int    `json:"rank"`
			} `json:"feedback_target"`
		}
		callTool(t, cs, "get_chunk", map[string]any{"chunk_id": hit.Results[0].ChunkID}, &direct)
		if direct.Text == "" || !direct.OpenRecorded || !direct.OpenInferred {
			t.Fatalf("direct read = %+v, want content with an inferred open", direct)
		}
		if direct.FeedbackTarget == nil || direct.FeedbackTarget.RunID != latest.RunID || direct.FeedbackTarget.Rank != 1 {
			t.Fatalf("feedback target = %+v, want the latest search %q at rank 1", direct.FeedbackTarget, latest.RunID)
		}
		if !strings.Contains(direct.NextStep, latest.RunID) {
			t.Errorf("NextStep = %q, want the inferred run id", direct.NextStep)
		}
		var openCount, inferredCount int
		if err := db.QueryRow(`SELECT open_count,inferred_open_count FROM retrieval_candidates
			WHERE run_id=? AND rank=1`, latest.RunID).Scan(&openCount, &inferredCount); err != nil {
			t.Fatal(err)
		}
		if openCount != 1 || inferredCount != 1 {
			t.Errorf("open_count/inferred_open_count = %d/%d, want 1/1", openCount, inferredCount)
		}

		// The inferred open is now something the observer can ask about.
		// Its nudge must not claim the confidence of an exact open.
		var status struct {
			Nudges []struct {
				Kind       string `json:"kind"`
				Source     string `json:"source"`
				Confidence string `json:"confidence"`
				RunID      string `json:"run_id"`
				Rank       int    `json:"rank"`
			} `json:"nudges"`
		}
		callTool(t, cs, "feedback_status", map[string]any{}, &status)
		found := false
		for _, n := range status.Nudges {
			if n.Kind == "retrieval_outcome_missing" && n.RunID == latest.RunID && n.Rank == 1 {
				found = true
				if n.Source != "inferred_open" || n.Confidence != "medium" {
					t.Errorf("inferred nudge source/confidence = %q/%q, want inferred_open/medium", n.Source, n.Confidence)
				}
			}
			if n.RunID == hit.RetrievalRunID && n.Confidence != "high" {
				t.Errorf("exact-open nudge confidence = %q, want high", n.Confidence)
			}
		}
		if !found {
			t.Errorf("feedback_status nudges = %+v, want one for the inferred open", status.Nudges)
		}
	})

	t.Run("half an exact target is an error", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"chunk_id": hit.Results[0].ChunkID, "run_id": hit.RetrievalRunID},
			{"chunk_id": hit.Results[0].ChunkID, "rank": 1},
		} {
			res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "get_chunk", Arguments: args})
			if err == nil && !res.IsError {
				t.Errorf("get_chunk(%v) succeeded, want an error naming both arguments", args)
			}
		}
	})

	t.Run("a chunk no search returned stays untracked", func(t *testing.T) {
		res, err := db.Exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES ('claude-code:sess-1', 900, 901, 'never searched for')`)
		if err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
		id, _ := res.LastInsertId()
		var direct struct {
			Text           string          `json:"text"`
			OpenRecorded   bool            `json:"open_recorded"`
			NextStep       string          `json:"next_step"`
			FeedbackTarget json.RawMessage `json:"feedback_target"`
		}
		callTool(t, cs, "get_chunk", map[string]any{"chunk_id": id}, &direct)
		if direct.Text == "" || direct.OpenRecorded || direct.FeedbackTarget != nil || direct.NextStep != "" {
			t.Fatalf("read = %+v, want content with no attribution and no labelling hint", direct)
		}
	})

	t.Run("telemetry failure does not hide the chunk", func(t *testing.T) {
		if _, err := db.Exec(`CREATE TRIGGER fail_open_telemetry BEFORE UPDATE OF open_count
			ON retrieval_candidates BEGIN SELECT RAISE(ABORT, 'telemetry unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		defer db.Exec(`DROP TRIGGER fail_open_telemetry`)
		var got struct {
			Text         string `json:"text"`
			OpenRecorded bool   `json:"open_recorded"`
		}
		callTool(t, cs, "get_chunk", map[string]any{
			"chunk_id": hit.Results[0].ChunkID,
			"run_id":   hit.RetrievalRunID,
			"rank":     hit.Results[0].Rank,
		}, &got)
		if got.Text == "" || got.OpenRecorded {
			t.Fatalf("telemetry failure result = %+v, want readable unrecorded chunk", got)
		}
	})

	t.Run("unknown id is a tool error", func(t *testing.T) {
		res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "get_chunk", Arguments: map[string]any{"chunk_id": 999999}})
		if err == nil && !res.IsError {
			t.Error("get_chunk for a missing id succeeded, want an error")
		}
	})

	t.Run("a chunk from another project reads as absent", func(t *testing.T) {
		otherProject, err := store.ResolveProject(db, "/home/other/proj")
		if err != nil {
			t.Fatalf("ResolveProject: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES ('codex:other', 'codex', 'other', ?, '2026-09-20T10:00:00Z')`, otherProject); err != nil {
			t.Fatalf("seed session: %v", err)
		}
		res, err := db.Exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES ('codex:other', 0, 1, 'secret from elsewhere')`)
		if err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
		id, _ := res.LastInsertId()
		call, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "get_chunk", Arguments: map[string]any{"chunk_id": id}})
		if err == nil && !call.IsError {
			t.Errorf("get_chunk read a chunk from another project: %+v", call.Content)
		}
	})

	t.Run("oversized chunk text is cut", func(t *testing.T) {
		res, err := db.Exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES ('claude-code:sess-1', 500, 501, ?)`, strings.Repeat("x", 50000))
		if err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
		id, _ := res.LastInsertId()
		var big struct {
			Text          string `json:"text"`
			TextTruncated bool   `json:"text_truncated"`
		}
		callTool(t, cs, "get_chunk", map[string]any{"chunk_id": id}, &big)
		if !big.TextTruncated || len(big.Text) > 13000 {
			t.Errorf("TextTruncated = %v, len = %d, want truncated to about 12000", big.TextTruncated, len(big.Text))
		}
	})
}

// TestGetSessionRecordsOpens is the regression test for issue #15: reading a
// hit through get_session left no open, so it could never be labelled.
func TestGetSessionRecordsOpens(t *testing.T) {
	cs, db := testServer(t)
	var hit struct {
		RunID   string `json:"run_id"`
		Results []struct {
			SessionID string `json:"session_id"`
			FirstSeq  int    `json:"first_seq"`
			LastSeq   int    `json:"last_seq"`
		} `json:"results"`
	}
	callTool(t, cs, "search_context", map[string]any{"query": "WAL"}, &hit)
	h := hit.Results[0]
	type result struct {
		Messages       []json.RawMessage `json:"messages"`
		OpenRecorded   bool              `json:"open_recorded"`
		NextStep       string            `json:"next_step"`
		FeedbackTarget *struct {
			RunID string `json:"run_id"`
			Rank  int    `json:"rank"`
		} `json:"feedback_target"`
	}
	opens := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM retrieval_open_events WHERE run_id = ? AND rank = 1`, hit.RunID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("hit range with run_id and rank records an exact open", func(t *testing.T) {
		var got result
		callTool(t, cs, "get_session", map[string]any{
			"session_id": h.SessionID, "from_seq": h.FirstSeq, "to_seq": h.LastSeq,
			"run_id": hit.RunID, "rank": 1,
		}, &got)
		if len(got.Messages) == 0 || !got.OpenRecorded || got.FeedbackTarget == nil || got.FeedbackTarget.RunID != hit.RunID {
			t.Fatalf("result = %+v, want messages and an open on the search's rank 1", got)
		}
		if !strings.Contains(got.NextStep, "record_retrieval_outcome(run_id=\""+hit.RunID+"\", rank=1,") {
			t.Errorf("NextStep = %q, want the labelling call", got.NextStep)
		}
		if opens() != 1 {
			t.Errorf("open events = %d, want 1", opens())
		}
	})

	t.Run("a range that misses the hit records nothing", func(t *testing.T) {
		before := opens()
		var got result
		callTool(t, cs, "get_session", map[string]any{
			"session_id": h.SessionID, "from_seq": h.LastSeq + 1, "to_seq": h.LastSeq + 50,
			"run_id": hit.RunID, "rank": 1,
		}, &got)
		if got.OpenRecorded || opens() != before {
			t.Errorf("open recorded for messages after the hit: %+v", got)
		}
	})

	t.Run("without run_id and rank nothing is recorded", func(t *testing.T) {
		before := opens()
		var got result
		callTool(t, cs, "get_session", map[string]any{"session_id": h.SessionID, "from_seq": h.FirstSeq, "to_seq": h.LastSeq}, &got)
		if len(got.Messages) == 0 || got.OpenRecorded || got.NextStep != "" || opens() != before {
			t.Errorf("plain read = %+v, want messages and no open", got)
		}
	})

	t.Run("half an exact target is an error", func(t *testing.T) {
		for _, extra := range []map[string]any{{"run_id": hit.RunID}, {"rank": 1}} {
			args := map[string]any{"session_id": h.SessionID}
			for k, v := range extra {
				args[k] = v
			}
			res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "get_session", Arguments: args})
			if err == nil && !res.IsError {
				t.Errorf("get_session(%v) succeeded, want an error", args)
			}
		}
	})
}

// TestGetSessionIsProjectScoped: get_chunk reads a chunk from another project
// as absent, and get_session must do the same for a whole transcript.
func TestGetSessionIsProjectScoped(t *testing.T) {
	cs, db := testServer(t)
	other, err := store.ResolveProject(db, "/home/other/proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES ('codex:elsewhere', 'codex', 'elsewhere', ?, '2026-09-20T10:00:00Z');
		INSERT INTO messages(session_id, seq, role, kind, content) VALUES ('codex:elsewhere', 0, 'user', 'text', 'secret from another project'),
			('codex:elsewhere', 1, 'assistant', 'text', 'reply');`, other); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages    []json.RawMessage `json:"messages"`
		NextFromSeq *int              `json:"next_from_seq"`
	}
	callTool(t, cs, "get_session", map[string]any{"session_id": "codex:elsewhere"}, &got)
	if len(got.Messages) != 0 || got.NextFromSeq != nil {
		t.Errorf("another project's session returned %d messages (next %v), want none", len(got.Messages), got.NextFromSeq)
	}
}

func TestGetSessionDefaultsToConversationText(t *testing.T) {
	cs, _ := testServer(t)

	var def sessionPage
	callTool(t, cs, "get_session", map[string]any{"session_id": "claude-code:sess-1"}, &def)
	if len(def.Messages) == 0 {
		t.Fatal("no messages returned")
	}
	for _, m := range def.Messages {
		if m.Kind != "text" {
			t.Errorf("seq %d has kind %q, want only text by default", m.Seq, m.Kind)
		}
	}

	var withTools sessionPage
	callTool(t, cs, "get_session", map[string]any{"session_id": "claude-code:sess-1", "include_tools": true}, &withTools)
	kinds := map[string]bool{}
	for _, m := range withTools.Messages {
		kinds[m.Kind] = true
	}
	if !kinds["tool_call"] || !kinds["tool_result"] {
		t.Errorf("include_tools kinds = %v, want tool_call and tool_result present", kinds)
	}
	if len(withTools.Messages) <= len(def.Messages) {
		t.Errorf("include_tools returned %d messages, default %d; want more", len(withTools.Messages), len(def.Messages))
	}
}

func TestGetSessionTruncatesHugeToolResult(t *testing.T) {
	cs, db := testServer(t)
	seedMessage(t, db, 100, "tool", "tool_result", strings.Repeat("r", 80000))

	var out sessionPage
	callTool(t, cs, "get_session", map[string]any{
		"session_id": "claude-code:sess-1", "from_seq": 100, "to_seq": 100, "include_tools": true,
	}, &out)
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	m := out.Messages[0]
	if !m.ContentTruncated || m.FullChars != 80000 {
		t.Errorf("ContentTruncated = %v, FullChars = %d, want true / 80000", m.ContentTruncated, m.FullChars)
	}
	if len(m.Content) > 2000 {
		t.Errorf("content is %d chars, want the tool result cut to about 1500", len(m.Content))
	}
}

func TestGetSessionCapsTotalCharsAndPages(t *testing.T) {
	cs, db := testServer(t)
	// Ten 5000-character assistant replies, well under the row cap but far
	// over a 12000-character budget.
	for i := 0; i < 10; i++ {
		seedMessage(t, db, 100+i, "assistant", "text", strings.Repeat("a", 5000))
	}

	var first sessionPage
	callTool(t, cs, "get_session", map[string]any{
		"session_id": "claude-code:sess-1", "from_seq": 100, "to_seq": 109, "max_chars": 12000,
	}, &first)
	if len(first.Messages) != 2 {
		t.Fatalf("first page = %d messages, want 2 (2 x 5000 fits in 12000, a third does not)", len(first.Messages))
	}
	if !first.Truncated || first.NextFromSeq == nil || *first.NextFromSeq != 102 {
		t.Fatalf("Truncated = %v, NextFromSeq = %v, want true / 102", first.Truncated, first.NextFromSeq)
	}

	// Paging on must reach every message exactly once.
	seen := len(first.Messages)
	next := *first.NextFromSeq
	for pages := 0; pages < 10; pages++ {
		var page sessionPage
		callTool(t, cs, "get_session", map[string]any{
			"session_id": "claude-code:sess-1", "from_seq": next, "to_seq": 109, "max_chars": 12000,
		}, &page)
		seen += len(page.Messages)
		if page.NextFromSeq == nil {
			break
		}
		next = *page.NextFromSeq
	}
	if seen != 10 {
		t.Errorf("paged through %d messages, want 10", seen)
	}

	t.Run("a single message over the budget is still returned", func(t *testing.T) {
		var one sessionPage
		callTool(t, cs, "get_session", map[string]any{
			"session_id": "claude-code:sess-1", "from_seq": 100, "to_seq": 109, "max_chars": 100,
		}, &one)
		if len(one.Messages) != 1 {
			t.Errorf("messages = %d, want 1 so the caller can always make progress", len(one.Messages))
		}
	})

	t.Run("default window reports messages beyond it", func(t *testing.T) {
		seedMessage(t, db, 400, "user", "text", "far ahead")
		var page sessionPage
		callTool(t, cs, "get_session", map[string]any{"session_id": "claude-code:sess-1", "from_seq": 50, "max_chars": 60000}, &page)
		// 50..249 holds seq 100-109 (50k chars, inside the raised budget);
		// seq 400 lies past the 200-row window.
		if !page.Truncated || page.NextFromSeq == nil || *page.NextFromSeq != 400 {
			t.Errorf("Truncated = %v, NextFromSeq = %v, want true / 400", page.Truncated, page.NextFromSeq)
		}
	})
}
