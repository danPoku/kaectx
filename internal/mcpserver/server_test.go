// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
		} `json:"results"`
		UsedSemanticSearch bool   `json:"used_semantic_search"`
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
	if out.RetrievalRunID == "" {
		t.Error("RetrievalRunID is empty, want recorded telemetry")
	}
	if !out.TelemetryRecorded {
		t.Error("TelemetryRecorded = false, want true")
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
	var openedAt sql.NullString
	var openCount int
	if err := db.QueryRow(`SELECT opened_at,open_count FROM retrieval_candidates
		WHERE run_id=? AND rank=?`, out.FeedbackTarget.RunID, out.FeedbackTarget.Rank).Scan(&openedAt, &openCount); err != nil {
		t.Fatal(err)
	}
	if !openedAt.Valid || openCount != 1 {
		t.Errorf("open telemetry = %v count %d, want timestamp and count 1", openedAt, openCount)
	}

	t.Run("direct read is untracked", func(t *testing.T) {
		var direct struct {
			Text           string `json:"text"`
			OpenRecorded   bool   `json:"open_recorded"`
			FeedbackTarget *struct {
				RunID string `json:"run_id"`
				Rank  int    `json:"rank"`
			} `json:"feedback_target"`
		}
		callTool(t, cs, "get_chunk", map[string]any{"chunk_id": hit.Results[0].ChunkID}, &direct)
		if direct.Text == "" || direct.OpenRecorded || direct.FeedbackTarget != nil {
			t.Fatalf("direct read = %+v, want content without attribution", direct)
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
