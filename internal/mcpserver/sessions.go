// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

const defaultRecentLimit = 20

// getSessionMaxRows caps how many messages get_session can return in one
// call — progressive disclosure: an agent pages through a session
// deliberately instead of one call swallowing a 2,000-message transcript
// into its context window.
const getSessionMaxRows = 200

// Size caps for get_session, in characters (a token is roughly 4 of them).
// The row cap alone was not a real limit: 200 rows of tool results measured
// about 39k tokens a page. So the response is also bounded by total
// characters, and each message is cut to its own limit, with tool output
// held to a much smaller one than conversation text because one tool result
// can run to tens of thousands of characters.
const (
	getSessionDefaultChars = 20000
	getSessionMaxChars     = 60000
	maxTextMessageChars    = 6000
	maxToolMessageChars    = 1500
)

type recentSessionsArgs struct {
	Agent string `json:"agent,omitempty" jsonschema:"restrict to one agent, e.g. claude-code or codex (default: every agent)"`
	Limit int    `json:"limit,omitempty" jsonschema:"max sessions, default 20"`
	Since string `json:"since,omitempty" jsonschema:"only sessions active at or after this time: a UTC date (2026-09-28) or RFC 3339 timestamp"`
	Until string `json:"until,omitempty" jsonschema:"only sessions active before this time; a date means through the end of that day (2026-10-04)"`
}

type sessionOverview struct {
	ID              string `json:"id"`
	Agent           string `json:"agent"`
	GitBranch       string `json:"git_branch,omitempty"`
	Title           string `json:"title,omitempty"`
	Summary         string `json:"summary,omitempty"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	MessageCount    int    `json:"message_count"`
	FilesChanged    int    `json:"files_changed"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
}

type recentSessionsResult struct {
	Sessions []sessionOverview `json:"sessions"`
	// Index: see searchContextResult.Index.
	Index *indexStatus `json:"index,omitempty"`
}

type getSessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"the session id, e.g. as returned by search_context or recent_sessions"`
	FromSeq   int    `json:"from_seq,omitempty" jsonschema:"first message sequence number to return, default 0"`
	ToSeq     int    `json:"to_seq,omitempty" jsonschema:"last message sequence number to return; the range is capped, so a large session needs multiple calls"`
	// IncludeTools defaults to false: only user/assistant text comes back.
	IncludeTools bool `json:"include_tools,omitempty" jsonschema:"also return tool calls and tool results (large; default false = conversation text only)"`
	MaxChars     int  `json:"max_chars,omitempty" jsonschema:"cap on total characters returned, default 20000, max 60000; check truncated/next_from_seq to continue"`
	// RunID and Rank attribute the read to a search hit, as on get_chunk.
	RunID string `json:"run_id,omitempty" jsonschema:"run_id returned by search_context when reading one of its hits; provide with rank to record the open"`
	Rank  int    `json:"rank,omitempty" jsonschema:"1-based rank of the hit being read; provide with run_id"`
}

type sessionMessage struct {
	Seq       int    `json:"seq"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	ToolName  string `json:"tool_name,omitempty"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at,omitempty"`
	// ContentTruncated is set when Content was cut to the per-message
	// limit; FullChars is the original length.
	ContentTruncated bool `json:"content_truncated,omitempty"`
	FullChars        int  `json:"full_chars,omitempty"`
}

type getSessionResult struct {
	Messages []sessionMessage `json:"messages"`
	// Truncated and NextFromSeq tell the caller there's more beyond what
	// was returned, and where to resume — the paging contract that keeps
	// this tool from being used to swallow a whole transcript in one call.
	Truncated   bool `json:"truncated"`
	NextFromSeq *int `json:"next_from_seq,omitempty"`
	// OpenRecorded, FeedbackTarget and NextStep work as on get_chunk, for a
	// call that passed a hit's run_id and rank.
	OpenRecorded   bool            `json:"open_recorded"`
	FeedbackTarget *feedbackTarget `json:"feedback_target,omitempty"`
	NextStep       string          `json:"next_step,omitempty"`
}

type sessionsTouchingArgs struct {
	Path  string `json:"path" jsonschema:"project-relative file path, e.g. internal/store/store.go"`
	Limit int    `json:"limit,omitempty" jsonschema:"max sessions, default 20"`
}

type fileTouchSession struct {
	SessionID string   `json:"session_id"`
	Agent     string   `json:"agent"`
	Title     string   `json:"title,omitempty"`
	StartedAt string   `json:"started_at"`
	Actions   []string `json:"actions"`
	Touches   int      `json:"touches"`
}

type sessionsTouchingResult struct {
	Sessions []fileTouchSession `json:"sessions"`
}

func (s *server) registerSessions(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recent_sessions",
		Description: "List the most recent coding-agent sessions in this project, newest first. A summary view — use get_session to read one in full. " +
			"Set since/until to list the sessions active in a period, e.g. one week. If index.warning is set, some recent sessions haven't been read yet.",
	}, s.recentSessions)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_session",
		Description: "Read a session's messages in order, paged by sequence number. Returns conversation text only unless include_tools is set. Capped by total characters (max_chars) and per-message length; check `truncated` and use `next_from_seq` to continue. " +
			"To read a search hit, pass its first_seq/last_seq as from_seq/to_seq rather than paging from 0, and the search's run_id and the hit's rank to record the open; the result's next_step is then the call that labels it.",
	}, s.getSession)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sessions_touching",
		Description: "Find past sessions that touched a given file, most recent first — useful before editing a file to see who's been here and what they did. Path must be project-relative.",
	}, s.sessionsTouching)
}

func (s *server) recentSessions(_ context.Context, _ *mcp.CallToolRequest, args recentSessionsArgs) (*mcp.CallToolResult, recentSessionsResult, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "RecentSessions")
	if err != nil {
		return nil, recentSessionsResult{}, err
	}

	var agentArg any
	if args.Agent != "" {
		agentArg = args.Agent
	}
	w, err := search.ParseWindow(args.Since, args.Until)
	if err != nil {
		return nil, recentSessionsResult{}, err
	}
	rows, err := s.db.Query(sqlText, append([]any{
		sql.Named("project_id", s.projectID), sql.Named("agent", agentArg), sql.Named("limit", limit),
	}, w.Args()...)...)
	if err != nil {
		return nil, recentSessionsResult{}, err
	}
	defer rows.Close()

	out := recentSessionsResult{Index: s.indexStatus()}
	for rows.Next() {
		// v_session_overview: id, agent, project_id, project, git_branch,
		// title, summary, started_at, ended_at, message_count,
		// files_changed, parent_session_id
		var o sessionOverview
		var projectID sql.NullInt64
		var project, gitBranch, title, summary, endedAt, parentSessionID sql.NullString
		if err := rows.Scan(&o.ID, &o.Agent, &projectID, &project, &gitBranch, &title, &summary,
			&o.StartedAt, &endedAt, &o.MessageCount, &o.FilesChanged, &parentSessionID); err != nil {
			return nil, recentSessionsResult{}, err
		}
		o.GitBranch, o.Title, o.Summary = gitBranch.String, title.String, summary.String
		o.EndedAt, o.ParentSessionID = endedAt.String, parentSessionID.String
		out.Sessions = append(out.Sessions, o)
	}
	return nil, out, rows.Err()
}

func (s *server) getSession(_ context.Context, _ *mcp.CallToolRequest, args getSessionArgs) (*mcp.CallToolResult, getSessionResult, error) {
	if (args.RunID != "") != (args.Rank > 0) {
		return nil, getSessionResult{}, fmt.Errorf("pass run_id and rank together, or neither")
	}
	fromSeq := args.FromSeq
	toSeq := args.ToSeq
	// explicitRange: the caller named a range that fits under the row cap.
	// Anything else (no to_seq, or one too wide) is a window WE chose, so
	// there may be more beyond it that the caller didn't ask for yet.
	explicitRange := toSeq > 0 && toSeq >= fromSeq && toSeq-fromSeq+1 <= getSessionMaxRows
	if !explicitRange {
		toSeq = fromSeq + getSessionMaxRows - 1
	}

	maxChars := args.MaxChars
	if maxChars <= 0 {
		maxChars = getSessionDefaultChars
	}
	if maxChars > getSessionMaxChars {
		maxChars = getSessionMaxChars
	}
	includeTools := 0
	if args.IncludeTools {
		includeTools = 1
	}

	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "GetSessionMessages")
	if err != nil {
		return nil, getSessionResult{}, err
	}
	rows, err := s.db.Query(sqlText,
		sql.Named("session_id", args.SessionID), sql.Named("project_id", s.projectID),
		sql.Named("from_seq", fromSeq), sql.Named("to_seq", toSeq),
		sql.Named("include_tools", includeTools))
	if err != nil {
		return nil, getSessionResult{}, err
	}
	defer rows.Close()

	out := getSessionResult{Messages: []sessionMessage{}}
	used := 0
	var resumeAt *int
	for rows.Next() {
		var m sessionMessage
		var toolName, createdAt sql.NullString
		if err := rows.Scan(&m.Seq, &m.Role, &m.Kind, &toolName, &m.Content, &createdAt); err != nil {
			return nil, getSessionResult{}, err
		}
		m.ToolName, m.CreatedAt = toolName.String, createdAt.String

		limit := maxTextMessageChars
		if m.Kind != "text" {
			limit = maxToolMessageChars
		}
		if cut, full, ok := truncateChars(m.Content, limit); ok {
			m.Content, m.ContentTruncated, m.FullChars = cut, true, full
		}

		// Stop before the message that would blow the budget, but always
		// return at least one so a caller can never get stuck.
		if len(out.Messages) > 0 && used+len(m.Content) > maxChars {
			seq := m.Seq
			resumeAt = &seq
			break
		}
		used += len(m.Content)
		out.Messages = append(out.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, getSessionResult{}, err
	}

	if resumeAt == nil && !explicitRange {
		// We chose the window; report whether messages remain past it.
		var next sql.NullInt64
		q, err := store.LoadQuery(queries.FS, "retrieval.sql", "FirstMessageSeqAfter")
		if err != nil {
			return nil, getSessionResult{}, err
		}
		if err := s.db.QueryRow(q, sql.Named("session_id", args.SessionID), sql.Named("project_id", s.projectID),
			sql.Named("after_seq", toSeq), sql.Named("include_tools", includeTools)).Scan(&next); err != nil {
			return nil, getSessionResult{}, err
		}
		if next.Valid {
			n := int(next.Int64)
			resumeAt = &n
		}
	}
	if resumeAt != nil {
		out.Truncated = true
		out.NextFromSeq = resumeAt
	}
	// The open is judged against the messages actually returned, not the
	// requested range: a page cut short by max_chars, or a range that misses
	// the hit, is not a read of it. Nothing returned, no open.
	if args.RunID != "" && len(out.Messages) > 0 {
		first, last := out.Messages[0].Seq, out.Messages[len(out.Messages)-1].Seq
		if s.recordOpen(args.RunID, args.Rank, false,
			`session_id=? AND first_seq<=? AND last_seq>=?`, args.SessionID, last, first) {
			out.FeedbackTarget = &feedbackTarget{RunID: args.RunID, Rank: args.Rank}
			out.OpenRecorded = true
			out.NextStep = labelStep(*out.FeedbackTarget)
		}
	}
	return nil, out, nil
}

// truncateChars cuts s to at most n runes (never mid-character), appending a
// marker, and reports the original length in runes.
func truncateChars(s string, n int) (cut string, fullChars int, truncated bool) {
	if len(s) <= n { // bytes >= runes, so this is a cheap early out
		return s, 0, false
	}
	r := []rune(s)
	if len(r) <= n {
		return s, 0, false
	}
	return string(r[:n]) + fmt.Sprintf(" … [truncated, %d of %d chars shown]", n, len(r)), len(r), true
}

func (s *server) sessionsTouching(_ context.Context, _ *mcp.CallToolRequest, args sessionsTouchingArgs) (*mcp.CallToolResult, sessionsTouchingResult, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "SessionsTouchingFile")
	if err != nil {
		return nil, sessionsTouchingResult{}, err
	}
	rows, err := s.db.Query(sqlText,
		sql.Named("project_id", s.projectID), sql.Named("path", args.Path), sql.Named("limit", limit))
	if err != nil {
		return nil, sessionsTouchingResult{}, err
	}
	defer rows.Close()

	var out sessionsTouchingResult
	for rows.Next() {
		var f fileTouchSession
		var title, actions sql.NullString
		if err := rows.Scan(&f.SessionID, &f.Agent, &title, &f.StartedAt, &actions, &f.Touches); err != nil {
			return nil, sessionsTouchingResult{}, err
		}
		f.Title = title.String
		if actions.Valid && actions.String != "" {
			f.Actions = strings.Split(actions.String, ",")
		}
		out.Sessions = append(out.Sessions, f)
	}
	return nil, out, rows.Err()
}
