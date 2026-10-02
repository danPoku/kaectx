// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/danPoku/kaectx/internal/redact"
	"github.com/danPoku/kaectx/internal/store"
)

// FileResult is one file's outcome from RunAll.
type FileResult struct {
	Path string
	Err  error
}

// RunAll walks root for *.jsonl files and calls Run on each, using a fresh
// Adapter per file. The factory (rather than a single shared instance)
// matters for an adapter like Codex's, whose log format carries no per-line
// session id — the adapter has to remember the current session id as state
// across ParseLine calls within one file, and reusing one instance across
// multiple files would leak that state between unrelated sessions.
//
// A missing root (no sessions ingested yet on this machine) is not an
// error — it just yields no results.
func RunAll(db *sql.DB, newAdapter func() Adapter, root string) ([]FileResult, error) {
	var results []FileResult
	git := store.NewGitResolver() // shared, so a repo's history is read once for the whole walk
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		results = append(results, FileResult{Path: path, Err: run(db, newAdapter(), path, git)})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return results, nil
	}
	return results, err
}

// Run processes one log file: reads new bytes since the source's last byte
// offset, parses each complete line, upserts the session(s) it belongs to,
// inserts new messages, and chunks any turns that now look settled. A line
// that fails to parse is logged to sources.last_error and skipped — never
// fatal to the rest of the file.
func Run(db *sql.DB, adapter Adapter, path string) error {
	return run(db, adapter, path, store.NewGitResolver())
}

func run(db *sql.DB, adapter Adapter, path string, git *store.GitResolver) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	src, err := getOrCreateSource(db, adapter.Agent(), path)
	if err != nil {
		return err
	}

	byteOffset := src.ByteOffset
	// rewound is true when we had already ingested part of this file but must
	// now start over (rewritten, rotated, or truncated). Sessions it holds
	// then get their old rows replaced rather than appended to; see
	// processLine.
	rewound := false
	if byteOffset > 0 {
		// The fingerprint is the hash of the bytes we'd ALREADY consumed as
		// of the last run (capped at 4KB) — never "whatever's in the first
		// 4KB right now". A file under 4KB total grows on every legitimate
		// append, so hashing "the current first 4KB" would change on every
		// append too and falsely look rewritten. Hashing only the prefix we
		// already committed to means it can only change if that specific,
		// already-read span was actually altered.
		windowHash, err := headHashUpTo(f, byteOffset)
		if err != nil {
			return err
		}
		if src.ContentHeadHash != "" && src.ContentHeadHash != windowHash {
			// The file was rewritten or rotated since we last read it: our
			// bookmark no longer means anything, start over.
			byteOffset = 0
			rewound = true
		}
	}
	if byteOffset > info.Size() {
		// The file shrank (truncated/replaced): same story.
		byteOffset = 0
		rewound = true
	}

	if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
		return err
	}

	// Adapters can be stateful per file (Codex learns its session id only
	// from line 1's session_meta). A run that resumes mid-file gets a fresh
	// adapter that never saw that line, so every later line would come back
	// with no session id and be dropped — while the byte offset still
	// advanced, losing those messages for good. Like handing a new clerk the
	// case file's cover sheet before the day's new pages, replay the first
	// line into the adapter and discard the result. Harmless for stateless
	// adapters.
	if byteOffset > 0 {
		if err := primeAdapter(f, adapter); err != nil {
			return err
		}
	}

	r := bufio.NewReaderSize(f, 64*1024)
	toolNames := map[string]string{} // tool_use id -> tool name, this run only; see adapter doc
	sessionSeq := map[string]int{}   // session id -> next seq to assign
	var purged map[string]bool       // non-nil only when rewound: sessions whose old rows are already cleared
	if rewound {
		purged = map[string]bool{}
	}
	touched := map[string]bool{}
	var lastErr string
	consumed := byteOffset

	for {
		lineBytes, readErr := r.ReadBytes('\n')
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				// A non-empty tail with no trailing newline is a line the
				// writer hasn't finished yet (this file can be actively
				// growing). Leave it — and the byte offset — for the next
				// run rather than process a truncated JSON object.
				break
			}
			lastErr = readErr.Error()
			break
		}
		consumed += int64(len(lineBytes))
		trimmed := bytes.TrimRight(lineBytes, "\n")
		if len(bytes.TrimSpace(trimmed)) == 0 {
			continue
		}
		sid, err := processLine(db, adapter, git, trimmed, toolNames, sessionSeq, purged)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if sid != "" {
			touched[sid] = true
		}
	}

	for sid := range touched {
		if err := chunkSession(db, sid); err != nil {
			lastErr = err.Error()
		}
	}

	newHeadHash, err := headHashUpTo(f, consumed)
	if err != nil {
		return err
	}
	return updateSource(db, src.ID, consumed, newHeadHash, lastErr)
}

// primeAdapter feeds the file's first line to adapter, ignoring both its
// result and any parse error (a bad first line is already logged when the
// line was first ingested). ReadAt leaves f's read position alone.
func primeAdapter(f *os.File, adapter Adapter) error {
	r := bufio.NewReader(io.NewSectionReader(f, 0, 1<<20))
	first, err := r.ReadBytes('\n')
	if err != nil {
		// No complete first line yet, so nothing was ever consumed either.
		return nil
	}
	_, _ = adapter.ParseLine(bytes.TrimRight(first, "\n"))
	return nil
}

// headHashUpTo hashes the first min(4096, offset) bytes of f, read from the
// start regardless of f's current read position (ReadAt doesn't move it).
func headHashUpTo(f *os.File, offset int64) (string, error) {
	n := offset
	if n > 4096 {
		n = 4096
	}
	buf := make([]byte, n)
	if n > 0 {
		if _, err := f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
	}
	return sha256Hex(buf), nil
}

// processLine parses one line, upserts the session it belongs to, and
// inserts its message if it has one. It returns the session id touched (for
// chunking afterward), or "" if the line carried no session information at
// all (shouldn't happen for a well-formed conversational or ai-title line,
// but Skip lines legitimately return "").
func processLine(db *sql.DB, adapter Adapter, git *store.GitResolver, line []byte, toolNames map[string]string, sessionSeq map[string]int, purged map[string]bool) (string, error) {
	res, err := adapter.ParseLine(line)
	if err != nil {
		return "", err
	}
	if res.Skip {
		return "", nil
	}
	if res.Session.NativeID == "" {
		return "", nil
	}
	if res.Session.GitCommit == "" {
		// The commit that was current when the line was written, not HEAD
		// now: a session ingested weeks later must not be stamped with a
		// repository state it never saw.
		res.Session.GitCommit, res.Session.CommitSource = git.Resolve(res.Session.CWD, res.Session.GitBranch, res.Session.Timestamp)
	} else if res.Session.CommitSource == "" {
		res.Session.CommitSource = store.SourceLogged
	}

	sessionID := adapter.Agent() + ":" + res.Session.NativeID
	projectID, err := upsertSession(db, sessionID, adapter.Agent(), res.Session, res.Message)
	if err != nil {
		return sessionID, err
	}

	if purged != nil && !purged[sessionID] {
		// The file was rewritten and we are re-reading it from byte 0, so
		// the rows we stored last time describe a version of the file that no
		// longer exists. Appending would leave every message in there twice:
		// seq would continue from MAX(seq)+1 and never collide with
		// UNIQUE(session_id, seq). Like replacing a reissued statement, throw
		// the old pages away first — the first time this session shows up,
		// even if the new version has no message for it yet. file_touches go
		// with their messages (ON DELETE CASCADE); chunks are keyed by
		// session, and their vectors are removed by the chunks_vec_ad trigger.
		if _, err := db.Exec(`DELETE FROM chunks WHERE session_id = ?`, sessionID); err != nil {
			return sessionID, err
		}
		if _, err := db.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
			return sessionID, err
		}
		purged[sessionID] = true
	}

	if res.Message == nil {
		return sessionID, nil
	}
	msg := *res.Message

	// Resolve a tool_result's name from the tool_call that produced it.
	// This only sees calls from earlier in THIS ingest run — if a call and
	// its result straddle two separate one-shot runs (the file was ingested
	// mid-turn), the name is lost. Acceptable for a one-shot batch ingester;
	// closed properly once ctx daemon keeps this map alive continuously.
	if msg.Kind == "tool_result" {
		if name, ok := toolNames[msg.ToolUseID]; ok {
			msg.ToolName = name
		}
	}
	if msg.Kind == "tool_call" && msg.ToolUseID != "" {
		toolNames[msg.ToolUseID] = msg.ToolName
	}

	msg.Content = redact.Redact(msg.Content)
	redactedRaw := redact.Redact(string(msg.Raw))

	seq, ok := sessionSeq[sessionID]
	if !ok {
		var maxSeq sql.NullInt64
		if err := db.QueryRow(`SELECT MAX(seq) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
			return sessionID, err
		}
		seq = 0
		if maxSeq.Valid {
			seq = int(maxSeq.Int64) + 1
		}
	}
	sessionSeq[sessionID] = seq + 1

	res2, err := db.Exec(`INSERT OR IGNORE INTO messages(session_id, seq, role, kind, tool_name, content, created_at, raw)
	                   VALUES (?,?,?,?,?,?,?,?)`,
		sessionID, seq, msg.Role, msg.Kind, nullIfEmpty(msg.ToolName), msg.Content, nullIfEmpty(msg.CreatedAt), redactedRaw)
	if err != nil {
		return sessionID, err
	}

	if len(msg.FileTouches) == 0 {
		return sessionID, nil
	}
	// INSERT OR IGNORE means a re-ingest replaying an already-seen seq
	// inserts no row — RowsAffected distinguishes that from a genuine new
	// message, so a re-run never double-records the same file touch.
	// (last_insert_rowid() is unreliable here for the same reason: SQLite
	// leaves it untouched when INSERT OR IGNORE inserts nothing.)
	affected, err := res2.RowsAffected()
	if err != nil || affected == 0 {
		return sessionID, err
	}
	messageID, err := res2.LastInsertId()
	if err != nil {
		return sessionID, err
	}
	for _, ft := range msg.FileTouches {
		path := ft.Path
		path = git.ProjectPath(res.Session.CWD, path)
		rooted := 0
		if git.Root(res.Session.CWD) != "" {
			rooted = 1
		}
		if _, err := db.Exec(`INSERT INTO file_touches(session_id, message_id, project_id, path, action, created_at, rooted)
		                   VALUES (?,?,?,?,?,?,?)`,
			sessionID, messageID, projectID, path, ft.Action, nullIfEmpty(msg.CreatedAt), rooted); err != nil {
			return sessionID, err
		}
	}
	return sessionID, nil
}
