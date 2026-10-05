// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package repair

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
	"github.com/danPoku/kaectx/internal/redact"
)

// MessagesResult counts what a Messages run found and changed. With DryRun
// the counts describe what would have changed; nothing is written.
type MessagesResult struct {
	Examined    int // user/system messages re-read from their raw line
	Reclassed   int // messages whose role/kind changed (user turn <-> injected event)
	Recovered   int // user turns whose text changed, e.g. a prompt that had been cut to its first block
	Expanded    int // injected events that now keep every block, not only the first; chunks are unaffected
	Unparseable int // raw lines that re-read to no message (e.g. an empty prompt) or an error; left as they were
	Sessions    int // sessions whose chunks were rebuilt
	Titled      int // sessions whose title was set or replaced
}

// Messages re-derives user and system messages from the raw line stored
// with each one, using the current adapters, for databases ingested before
// ctx told harness-injected text apart from what the person typed, and
// before it read prompts spread over several content blocks.
//
// Like reprinting index cards after correcting the filing rules: the
// original letters (messages.raw) never changed, only how they were read.
// For each session where any message changes, the chunks are rebuilt from
// scratch, so injected text stops starting its own chunk. New chunks have no
// embedding yet; the embedding worker (`ctx daemon` or `ctx embed`) picks
// them up as it does any new chunk. Sessions that are untitled, or whose
// title came from injected text, get a title from their first typed prompt.
//
// Every write happens in one transaction, so an interrupted repair changes
// nothing, and the repair is idempotent: a second run finds nothing to do.
func Messages(db *sql.DB, dryRun bool) (MessagesResult, error) {
	var res MessagesResult
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin repair: %w", err)
	}
	defer tx.Rollback()

	changed, err := reclassifyMessages(tx, &res)
	if err != nil {
		return res, err
	}

	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM chunks WHERE session_id = ?`, id); err != nil {
			return res, fmt.Errorf("clear chunks of %s: %w", id, err)
		}
		if err := ingest.ChunkSession(tx, id); err != nil {
			return res, fmt.Errorf("rechunk %s: %w", id, err)
		}
		res.Sessions++
	}

	if err := retitleSessions(tx, &res); err != nil {
		return res, err
	}

	if dryRun {
		return res, nil // deferred Rollback discards the writes
	}
	return res, tx.Commit()
}

// parseRaw re-reads one stored raw line with the agent's current adapter.
// A fresh adapter per line is fine: only Codex's is stateful, and only for
// the session id and cwd, neither of which a message needs here.
func parseRaw(agent string, raw []byte) (*ingest.RawMessage, bool) {
	var a ingest.Adapter
	switch agent {
	case "claude-code":
		a = claudecode.New()
	case "codex":
		a = codex.New()
	default:
		return nil, false
	}
	res, err := a.ParseLine(raw)
	if err != nil || res.Skip || res.Message == nil {
		return nil, false
	}
	return res.Message, true
}

type storedMessage struct {
	id                  int64
	sessionID, agent    string
	role, kind, content string
	raw                 []byte
}

// reclassifyMessages updates every user/system text message whose re-read
// differs from what is stored, and returns the sessions it touched.
//
// Only role, kind and content are compared. Content is redacted the same
// way ingest redacts it, from a raw line that was itself already redacted,
// so the only differences come from how the line was read, not from
// redaction.
func reclassifyMessages(tx *sql.Tx, res *MessagesResult) (map[string]bool, error) {
	rows, err := tx.Query(`
		SELECT m.id, m.session_id, s.agent, m.role, m.kind, m.content, m.raw
		  FROM messages m
		  JOIN sessions s ON s.id = m.session_id
		 WHERE (m.role = 'user' AND m.kind = 'text')
		    OR (m.role = 'system' AND m.kind = 'event')`)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	var msgs []storedMessage
	for rows.Next() {
		var m storedMessage
		var raw sql.NullString
		if err := rows.Scan(&m.id, &m.sessionID, &m.agent, &m.role, &m.kind, &m.content, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		m.raw = []byte(raw.String)
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	changed := map[string]bool{}
	for _, m := range msgs {
		res.Examined++
		got, ok := parseRaw(m.agent, m.raw)
		if !ok {
			res.Unparseable++
			continue
		}
		content := redact.Redact(got.Content)
		if got.Role == m.role && got.Kind == m.kind && content == m.content {
			continue
		}
		if _, err := tx.Exec(`UPDATE messages SET role = ?, kind = ?, content = ? WHERE id = ?`,
			got.Role, got.Kind, content, m.id); err != nil {
			return nil, fmt.Errorf("update message %d: %w", m.id, err)
		}
		switch {
		case got.Role != m.role || got.Kind != m.kind:
			res.Reclassed++
		case m.role == "system":
			// Chunks hold only user and assistant text, so a fuller event
			// is worth storing but no reason to rebuild the session.
			res.Expanded++
			continue
		default:
			res.Recovered++
		}
		changed[m.sessionID] = true
	}
	return changed, nil
}

// retitleSessions gives a fallback title to every session that has none, or
// whose title is injected text (from before injected text was recognized).
// Every other title is kept, but redacted if it still holds a secret.
func retitleSessions(tx *sql.Tx, res *MessagesResult) error {
	rows, err := tx.Query(`
		SELECT s.id, COALESCE(s.title, ''),
		       COALESCE((SELECT m.content FROM messages m
		                  WHERE m.session_id = s.id AND m.role = 'user' AND m.kind = 'text'
		                    AND TRIM(m.content) <> ''
		                  ORDER BY m.seq LIMIT 1), '')
		  FROM sessions s`)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	type retitle struct{ id, title string }
	var todo []retitle
	for rows.Next() {
		var id, title, prompt string
		if err := rows.Scan(&id, &title, &prompt); err != nil {
			rows.Close()
			return err
		}
		if title != "" && !ingest.IsInjectedUserText(title) {
			// A title worth keeping may still hold a secret: earlier builds
			// cut the fallback title from the prompt BEFORE redacting it.
			if red := redact.Redact(title); red != title {
				todo = append(todo, retitle{id, red})
			}
			continue
		}
		next := ""
		if prompt != "" {
			next = ingest.TitleFromPrompt(prompt)
		}
		if next != title {
			todo = append(todo, retitle{id, next})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range todo {
		if _, err := tx.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, nullIfEmpty(r.title), r.id); err != nil {
			return fmt.Errorf("retitle %s: %w", r.id, err)
		}
		res.Titled++
	}
	return nil
}
