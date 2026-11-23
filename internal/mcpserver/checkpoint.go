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

	// activeGapLimit is the longest pause between two messages that still
	// counts as working time. A session resumed after a weekend is not a
	// 60-hour session, any more than a meeting adjourned until Monday ran all
	// weekend: only the time people were in the room counts.
	activeGapLimit = 30 * time.Minute
)

const (
	levelHealthy     = "healthy"
	levelRecommended = "checkpoint_recommended"
	levelStrong      = "checkpoint_strongly_recommended"
)

type checkpointArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"session to assess; omit to assess the most recently active session in this project"`
}

type checkpointMetrics struct {
	Messages     int `json:"messages"`
	FilesChanged int `json:"files_changed"`
	// DurationMins is active minutes: the gaps between consecutive messages,
	// leaving out any gap over activeGapLimit. It keeps its old JSON name so
	// clients that read it keep working.
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
	// PendingFeedback lists opened results still waiting for an outcome. A
	// checkpoint is when an agent is most likely to know whether they helped,
	// and the one moment it reliably checks in. Retrieval nudges only: the
	// checkpoint decision this call may have just recorded is the user's to
	// make, not something to ask back immediately.
	PendingFeedback []feedbackNudge `json:"pending_feedback"`
}

// checkpointPendingLimit caps pending_feedback so a long-neglected backlog
// cannot crowd out the checkpoint advice itself.
const checkpointPendingLimit = 10

type checkpointSession struct {
	id           string
	messages     int
	filesChanged int
}

func (s *server) registerCheckpoint(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "checkpoint_status",
		Description: "Assess whether the current coding session is large enough to merit a handoff. Call at natural milestones and when work becomes long, repetitive, or spans many files. If should_signal is true, explain the reasons to the user and offer to save a checkpoint and continue in a fresh session. duration_minutes is active time: pauses over 30 minutes between messages are not counted, so a session resumed days later is not treated as days long. This is advisory: never end the session automatically. A healthy check is recorded too; when the user carries on after one, record_checkpoint_outcome with continued. pending_feedback lists opened results still waiting for an outcome; label the ones whose usefulness you now know with record_retrieval_outcome.",
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
	duration, err := s.sessionActiveTime(sess.id)
	if err != nil {
		return nil, checkpointResult{}, fmt.Errorf("measure active time: %w", err)
	}

	out := checkpointResult{
		SessionID: sess.id,
		Level:     levelHealthy,
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
		out.Reasons = append(out.Reasons, fmt.Sprintf("%s active time (checkpoint threshold %s)", duration.Round(time.Minute), checkpointDurationThreshold))
		urgent = urgent || duration >= urgentDurationThreshold
	}
	if sess.filesChanged >= checkpointFilesThreshold {
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d files changed (checkpoint threshold %d)", sess.filesChanged, checkpointFilesThreshold))
		urgent = urgent || sess.filesChanged >= urgentFilesThreshold
	}

	if len(out.Reasons) > 0 {
		out.ShouldSignal = true
		out.Level = levelRecommended
		out.Recommendation = "Offer to save the objective, decisions, changed files, verification results, open risks, and exact next action, then continue in a fresh session."
	}
	// Only an urgent threshold escalates. Counting reasons did not work: a
	// long session crosses the message and time thresholds together, so two
	// reasons were the norm and the lower level never showed.
	if urgent {
		out.Level = levelStrong
		out.Recommendation = "Recommend saving a handoff now and continuing in a fresh session; let the user choose whether to switch, save only, or dismiss until the next milestone."
	}
	if err := s.recordCheckpointCheck(sess.id, out); err != nil {
		return nil, checkpointResult{}, err
	}
	if out.PendingFeedback, err = s.pendingRetrievalNudges(checkpointPendingLimit); err != nil {
		return nil, checkpointResult{}, fmt.Errorf("list pending feedback: %w", err)
	}
	return nil, out, nil
}

func (s *server) checkpointSession(sessionID string) (checkpointSession, error) {
	const selected = `
		SELECT s.id, s.message_count,
		       (SELECT COUNT(DISTINCT ft.path) FROM file_touches ft WHERE ft.session_id = s.id)
		FROM sessions s
		WHERE s.id = ? AND s.project_id = ?`
	const latest = `
		SELECT s.id, s.message_count,
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
	if err := row.Scan(&out.id, &out.messages, &out.filesChanged); err != nil {
		if err == sql.ErrNoRows {
			return out, fmt.Errorf("no session found in this project")
		}
		return out, err
	}
	return out, nil
}

// recordCheckpointCheck stores this assessment, one row per session, latest
// wins. Healthy checks are stored too: without them there is no record of the
// sessions ctx said to carry on with, and so nothing to compare the
// recommendations against.
//
// What happens to an earlier outcome depends on what it answered:
//   - a session that was healthy and now gets a recommendation is being asked
//     a new question, so the old "continued" is cleared and the clock restarts;
//   - otherwise a recorded outcome stands, as it did before healthy rows
//     existed, and only the clock moves.
func (s *server) recordCheckpointCheck(sessionID string, out checkpointResult) error {
	reasons, err := json.Marshal(out.Reasons)
	if err != nil {
		return fmt.Errorf("encode checkpoint reasons: %w", err)
	}
	const newQuestion = `(checkpoint_observations.level = 'healthy' AND excluded.level <> 'healthy')`
	if _, err := s.db.Exec(`INSERT INTO checkpoint_observations
		(session_id,project_id,level,message_count,duration_minutes,files_changed,reasons)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET
			message_count=excluded.message_count,
			duration_minutes=excluded.duration_minutes, files_changed=excluded.files_changed,
			reasons=excluded.reasons,
			recommended_at=CASE WHEN checkpoint_observations.outcome IS NULL AND NOT `+newQuestion+`
				THEN checkpoint_observations.recommended_at ELSE excluded.recommended_at END,
			outcome=CASE WHEN `+newQuestion+` THEN NULL ELSE checkpoint_observations.outcome END,
			note=CASE WHEN `+newQuestion+` THEN NULL ELSE checkpoint_observations.note END,
			note_id=CASE WHEN `+newQuestion+` THEN NULL ELSE checkpoint_observations.note_id END,
			resolved_at=CASE WHEN `+newQuestion+` THEN NULL ELSE checkpoint_observations.resolved_at END,
			level=excluded.level`,
		sessionID, s.projectID, out.Level, out.Metrics.Messages, out.Metrics.DurationMins,
		out.Metrics.FilesChanged, string(reasons)); err != nil {
		return fmt.Errorf("record checkpoint check: %w", err)
	}
	return nil
}

// sessionActiveTime reads a session's message times in order and measures the
// time spent working; see activeDuration.
func (s *server) sessionActiveTime(sessionID string) (time.Duration, error) {
	rows, err := s.db.Query(`SELECT created_at FROM messages
		WHERE session_id = ? AND created_at IS NOT NULL AND created_at <> ''
		ORDER BY seq`, sessionID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var times []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		// A timestamp that does not parse is skipped, not fatal: one odd line
		// must not stop the health check for the whole session.
		if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			times = append(times, at)
		}
	}
	return activeDuration(times, activeGapLimit), rows.Err()
}

// activeDuration sums the gaps between consecutive message times, leaving out
// any gap longer than maxGap. Like a taxi meter that stops when the cab is
// parked: a session left overnight and resumed is charged for the driving,
// not for the night. A gap that runs backwards (clocks differ between
// machines) counts as nothing rather than as negative time.
//
// A session with fewer than two timed messages has no gap to measure and
// reads as zero. That under-counts rather than guesses: the older measure,
// started-to-ended span, flagged a 33-message session resumed over 25 days as
// 36,044 minutes long.
func activeDuration(times []time.Time, maxGap time.Duration) time.Duration {
	var total time.Duration
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap > 0 && gap <= maxGap {
			total += gap
		}
	}
	return total
}
