// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package ingest is the agent-agnostic ingest driver: byte-offset
// bookkeeping per source file, session upsert, message insertion, and
// chunking. It knows nothing about any particular agent's log format — that
// lives in a per-agent Adapter (internal/ingest/claudecode, codex,
// antigravity) that turns one raw log line into the normalized shapes below.
package ingest

import "strings"

// RawMessage is one adapter-normalized event extracted from a single log
// line, ready for redaction and insertion into the messages table.
// Everything an agent's log format needs to express funnels through this
// shape so the driver never has to know which agent produced it.
type RawMessage struct {
	Role      string // "user" | "assistant" | "system" | "tool" (messages.role)
	Kind      string // "text" | "tool_call" | "tool_result" | "thinking" | "event"
	ToolName  string // set for tool_call / tool_result; may be resolved later, see ToolUseID
	ToolUseID string // present on tool_call and tool_result blocks; not persisted directly,
	// used by the driver to correlate a tool_result back to the tool_call
	// that produced it (the log format only carries the name on the call).
	Content   string // cleaned text, redacted before insert
	Raw       []byte // the original log line, redacted before insert
	CreatedAt string // ISO-8601 UTC, from the log line's own timestamp

	// FileTouches is set when this message is a tool_call the adapter
	// recognizes as touching files (an edit, a read, a multi-file patch...),
	// so the driver can also record them in file_touches. Left nil for everything else —
	// most tool calls (a shell command, an unrecognized tool) don't name a
	// file in a structured way an adapter can reliably extract.
	FileTouches []FileTouch
}

// FileTouch is "which file did this tool call touch, and how" — extracted
// by an adapter from a tool_call's structured input, not guessed from
// message text. Path may be absolute (as the tool reported it); the driver
// relativizes it against the session's cwd before storing it, since
// file_touches.path is project-relative.
type FileTouch struct {
	Path   string
	Action string // 'read' | 'edit' | 'create' | 'delete' | 'rename'
}

// SessionUpdate carries session-level fields discovered on a log line that
// isn't itself a conversational message (an agent-provided title event) or
// that ride along with one (cwd, branch, model). Zero-value fields mean "no
// new information" and leave the existing column alone.
type SessionUpdate struct {
	NativeID  string // the agent's own session id — required on every non-skip line
	CWD       string
	GitBranch string
	GitCommit string
	// CommitSource says how GitCommit was obtained (store.Source*). The
	// driver fills it in when it resolves the commit itself.
	CommitSource string
	Version      string
	Model        string
	Title        string
	Timestamp    string
	// Sidechain marks a line from a subagent transcript that carries its
	// parent's session id (Claude Code writes these under
	// <session>/subagents/). Its cwd is wherever the subagent was sent, not
	// where the session lives, so it may only set the project provisionally,
	// like a branch filing a claim under a temporary reference until the
	// policyholder's own form arrives.
	Sidechain bool
}

// ParseResult is what an Adapter returns for one input line.
type ParseResult struct {
	// Message is nil when the line carries only a session-level update (an
	// ai-title event) and nothing to store in messages.
	Message *RawMessage
	// More holds further messages from the same line, stored after Message
	// in this order. Claude Code and Codex write one block per line and
	// never need it. Antigravity writes a whole model step on one line: its
	// thinking, its text and every tool call it made, like one envelope
	// holding several separate forms that each get their own file entry.
	// Ignored when Message is nil.
	More    []RawMessage
	Session SessionUpdate
	// Skip is true for pure UI/harness bookkeeping (mode changes,
	// permission-mode, environment attachments, ...) that isn't
	// conversation and carries no session update either.
	Skip bool
	// Unknown is set, together with Skip, when the adapter skipped the line
	// not because it knows the type to be bookkeeping but because it has
	// never seen the type at all. The two look the same to the database
	// (nothing is stored) and must not look the same to the user: a known
	// bookkeeping line is noise, while a new type may be the conversation
	// itself in a format a newer agent version started writing. Like a
	// sorting office that bins circulars but must log an envelope it cannot
	// read the address on.
	//
	// The value is the type as the agent wrote it. A nested type carries
	// its parent: "response_item/function_call", "content/document". The
	// driver counts these per source file; see recordUnknown. For an
	// unknown line the adapter also fills Session.Version and
	// Session.Timestamp when the log has them, so the report can say which
	// agent version started writing the type and when.
	Unknown string
}

// Adapter turns one agent's raw log format into the driver's normalized
// shapes. Implementations must never panic on malformed input — return an
// error instead, which the driver logs to sources.last_error and continues
// past.
type Adapter interface {
	// Agent is this adapter's value for sessions.agent / sources.agent,
	// e.g. "claude-code".
	Agent() string
	// ParseLine parses one line (without its trailing newline) of the
	// agent's log format.
	ParseLine(line []byte) (ParseResult, error)
}

// FileBinder is an optional interface for an adapter whose log lines do not
// say which session they belong to, because the agent records that in the
// file's location instead. Antigravity is the case: a transcript's lines
// carry no conversation id, only the folder name does. The driver calls
// BindFile once, before the first ParseLine, with the path it is reading.
//
// BindFile must be cheap (no I/O): it runs for every file on every scan,
// including files with nothing new in them.
type FileBinder interface {
	BindFile(path string)
}

// FileSelector is an optional interface for an adapter whose agent keeps
// other .jsonl files, and many folders that hold no transcripts, under its
// session root. Without it every .jsonl file under a root is a session log,
// which is true for Claude Code and Codex.
type FileSelector interface {
	// WantsFile reports whether path is one of this agent's session logs.
	WantsFile(path string) bool
	// SkipDir reports whether the folder at path can hold no session logs,
	// so a scan need not descend into it and the daemon need not watch it.
	SkipDir(path string) bool
}

// IsSessionLog reports whether path is a session log for the agent adapter
// reads. Every place that walks a session root (RunAll, CheckFreshness, the
// daemon) asks this one question, so they cannot disagree about which files
// count.
func IsSessionLog(adapter Adapter, path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	if sel, ok := adapter.(FileSelector); ok {
		return sel.WantsFile(path)
	}
	return true
}

// SkipsDir reports whether a walk of adapter's session root can skip the
// folder at path. See FileSelector.
func SkipsDir(adapter Adapter, path string) bool {
	sel, ok := adapter.(FileSelector)
	return ok && sel.SkipDir(path)
}
