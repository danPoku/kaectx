// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	checkpointMessageThreshold  = 400
	checkpointDurationThreshold = 4 * time.Hour
	checkpointFilesThreshold    = 25
	urgentMessageThreshold      = 800
	urgentDurationThreshold     = 8 * time.Hour
	urgentFilesThreshold        = 40
)

type checkpointArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"session to assess; omit to assess the most recently active session in this project"`
}

type checkpointMetrics struct {
	Messages     int `json:"messages"`
	FilesChanged int `json:"files_changed"`
	DurationMins int `json:"duration_minutes"`
}

type checkpointResult struct {
	SessionID      string            `json:"session_id"`
	Level          string            `json:"level"`
	ShouldSignal   bool              `json:"should_signal"`
	Recommendation string            `json:"recommendation"`
	Reasons        []string          `json:"reasons"`
	Metrics        checkpointMetrics `json:"metrics"`
	Index          *indexStatus      `json:"index,omitempty"`
}

type checkpointSession struct {
	id           string
	startedAt    string
	endedAt      string
	messages     int
	filesChanged int
}

func (s *server) registerCheckpoint(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "checkpoint_status",
		Description: "Assess whether the current coding session is large enough to merit a handoff. Call at natural milestones and when work becomes long, repetitive, or spans many files. If should_signal is true, explain the reasons to the user and offer to save a checkpoint and continue in a fresh session. This is advisory: never end the session automatically.",
	}, s.checkpointStatus)
}

func (s *server) checkpointStatus(_ context.Context, _ *mcp.CallToolRequest, args checkpointArgs) (*mcp.CallToolResult, checkpointResult, error) {
	// Besides reporting staleness, this starts a background catch-up when the
	// live log has advanced. The caller can retry after index.warning clears;
	// returning immediately keeps a health check from stalling the session.
	index := s.indexStatus()
	sess, err := s.checkpointSession(args.SessionID)
	if err != nil {
		return nil, checkpointResult{}, err
	}
	now := time.Now().UTC()
	started, err := time.Parse(time.RFC3339Nano, sess.startedAt)
	if err != nil {
		return nil, checkpointResult{}, fmt.Errorf("parse session start: %w", err)
	}
	lastActivity := now
	if sess.endedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, sess.endedAt); err == nil {
			lastActivity = parsed
		}
	}
	duration := lastActivity.Sub(started)
	if duration < 0 {
		duration = 0
	}

	out := checkpointResult{
		SessionID: sess.id,
		Level:     "healthy",
		Reasons:   []string{},
		Index:     index,
		Metrics: checkpointMetrics{
			Messages: sess.messages, FilesChanged: sess.filesChanged,
			DurationMins: int(duration.Round(time.Minute) / time.Minute),
		},
		Recommendation: "Continue in this session and check again at the next logical milestone.",
	}

	urgent := false
	if sess.messages >= checkpointMessageThreshold {
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d messages (checkpoint threshold %d)", sess.messages, checkpointMessageThreshold))
		urgent = urgent || sess.messages >= urgentMessageThreshold
	}
	if duration >= checkpointDurationThreshold {
		out.Reasons = append(out.Reasons, fmt.Sprintf("%s session span (checkpoint threshold %s)", duration.Round(time.Minute), checkpointDurationThreshold))
		urgent = urgent || duration >= urgentDurationThreshold
	}
	if sess.filesChanged >= checkpointFilesThreshold {
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d files changed (checkpoint threshold %d)", sess.filesChanged, checkpointFilesThreshold))
		urgent = urgent || sess.filesChanged >= urgentFilesThreshold
	}

	if len(out.Reasons) > 0 {
		out.ShouldSignal = true
		out.Level = "checkpoint_recommended"
		out.Recommendation = "Offer to save the objective, decisions, changed files, verification results, open risks, and exact next action, then continue in a fresh session."
	}
	if urgent || len(out.Reasons) >= 2 {
		out.Level = "checkpoint_strongly_recommended"
		out.Recommendation = "Recommend saving a handoff now and continuing in a fresh session; let the user choose whether to switch, save only, or dismiss until the next milestone."
	}
	if out.ShouldSignal {
		reasons, err := json.Marshal(out.Reasons)
		if err != nil {
			return nil, checkpointResult{}, fmt.Errorf("encode checkpoint reasons: %w", err)
		}
		if _, err := s.db.Exec(`INSERT INTO checkpoint_observations
			(session_id,project_id,level,message_count,duration_minutes,files_changed,reasons)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(session_id) DO UPDATE SET
				level=excluded.level, message_count=excluded.message_count,
				duration_minutes=excluded.duration_minutes, files_changed=excluded.files_changed,
				reasons=excluded.reasons,
				recommended_at=CASE WHEN checkpoint_observations.outcome IS NULL
					THEN checkpoint_observations.recommended_at ELSE excluded.recommended_at END,
				outcome=CASE WHEN checkpoint_observations.outcome IS NULL
					THEN NULL ELSE checkpoint_observations.outcome END,
				resolved_at=CASE WHEN checkpoint_observations.outcome IS NULL
					THEN NULL ELSE checkpoint_observations.resolved_at END`,
			sess.id, s.projectID, out.Level, out.Metrics.Messages, out.Metrics.DurationMins,
			out.Metrics.FilesChanged, string(reasons)); err != nil {
			return nil, checkpointResult{}, fmt.Errorf("record checkpoint recommendation: %w", err)
		}
	}
	return nil, out, nil
}

func (s *server) checkpointSession(sessionID string) (checkpointSession, error) {
	const selected = `
		SELECT s.id, s.started_at, COALESCE(s.ended_at, ''), s.message_count,
		       (SELECT COUNT(DISTINCT ft.path) FROM file_touches ft WHERE ft.session_id = s.id)
		FROM sessions s
		WHERE s.id = ? AND s.project_id = ?`
	const latest = `
		SELECT s.id, s.started_at, COALESCE(s.ended_at, ''), s.message_count,
		       (SELECT COUNT(DISTINCT ft.path) FROM file_touches ft WHERE ft.session_id = s.id)
		FROM sessions s
		WHERE s.project_id = ?
		ORDER BY COALESCE(s.ended_at, s.started_at) DESC
		LIMIT 1`

	var row *sql.Row
	if sessionID == "" {
		row = s.db.QueryRow(latest, s.projectID)
	} else {
		row = s.db.QueryRow(selected, sessionID, s.projectID)
	}
	var out checkpointSession
	if err := row.Scan(&out.id, &out.startedAt, &out.endedAt, &out.messages, &out.filesChanged); err != nil {
		if err == sql.ErrNoRows {
			return out, fmt.Errorf("no session found in this project")
		}
		return out, err
	}
	return out, nil
}
