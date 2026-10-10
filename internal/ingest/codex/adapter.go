// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package codex adapts Codex CLI's rollout JSONL format (one JSON object
// per line under ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl) into the
// shapes internal/ingest knows how to store.
//
// Every line has the shape {timestamp, ordinal, type, payload}. Confirmed
// against two real files: one that failed on an auth error before doing any
// work, and one deliberately generated (with the user's permission, using
// their own authenticated `codex exec`) to capture a real tool call and
// assistant reply, since the first sample never got that far.
//
// Two structural differences from Claude Code's format, both discovered
// from those real files:
//
//  1. No per-line session id. A rollout file is one session (like Claude
//     Code), but unlike Claude Code, individual lines don't repeat the
//     session id — only the first `session_meta` line carries it. This
//     adapter is therefore STATEFUL: it remembers the session id from
//     session_meta and attaches it to every later line. Adapter values must
//     not be reused across files (see internal/ingest.RunAll, which
//     constructs a fresh one per file for exactly this reason).
//
//  2. Framework-injected context (skills instructions, permission policy,
//     plugin recommendations, an environment snapshot) rides in the SAME
//     role=user / role=developer messages a genuine human prompt arrives
//     in — there's no separate isMeta-style flag like Claude Code has. The
//     one thing that does discriminate them is
//     internal_chat_message_metadata_passthrough.content_item_kinds: a
//     genuine human turn's block is tagged "user.text"; everything else
//     seen ("host_skills.instructions", "plugins.recommendations",
//     "environments.environment_context", "multi_agent.role_instructions",
//     ...) is injected context. This matters beyond labeling — the
//     shared chunker treats a new role=user/kind=text message as a turn
//     boundary, so misclassifying injected context as a genuine turn would
//     fragment chunks around framework noise instead of real turns.
//
// custom_tool_call / custom_tool_call_output (not "function_call" — that
// was an assumption from general Responses-API knowledge that turned out
// wrong against the real file) are confirmed from the generated sample.
// `reasoning` items are NOT confirmed — this session used reasoning effort
// "none" and never produced one — so that parser is best-effort from public
// Responses API convention and degrades to Skip rather than guess wrong if
// the shape doesn't match.
package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/danPoku/kaectx/internal/ingest"
)

// Adapter is stateful: it must not be shared across files. Use New() to get
// a fresh one, or ingest.RunAll's per-file factory.
type Adapter struct {
	sessionID string
	// cwd is remembered from session_meta / turn_context: only those lines
	// carry it, but every later line needs it to relativize file paths and
	// to resolve which git commit was checked out at that moment.
	cwd string
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Agent() string { return "codex" }

type line struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

func (a *Adapter) ParseLine(raw []byte) (ingest.ParseResult, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("invalid JSON: %w", err)
	}

	switch l.Type {
	case "session_meta":
		var p struct {
			SessionID  string `json:"session_id"`
			CWD        string `json:"cwd"`
			CliVersion string `json:"cli_version"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return ingest.ParseResult{}, fmt.Errorf("session_meta payload: %w", err)
		}
		a.sessionID = p.SessionID
		a.cwd = p.CWD
		return ingest.ParseResult{Session: ingest.SessionUpdate{
			NativeID: a.sessionID, CWD: p.CWD, Version: p.CliVersion, Timestamp: l.Timestamp,
		}}, nil

	case "turn_context":
		// No message id it carries session-level info (model, cwd for this
		// turn) worth keeping current, same pattern as Claude Code's
		// ai-title: a session update with no message to store.
		var p struct {
			CWD   string `json:"cwd"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return ingest.ParseResult{}, fmt.Errorf("turn_context payload: %w", err)
		}
		if p.CWD != "" {
			a.cwd = p.CWD
		}
		return ingest.ParseResult{Session: ingest.SessionUpdate{
			NativeID: a.sessionID, CWD: a.cwd, Model: p.Model, Timestamp: l.Timestamp,
		}}, nil

	case "response_item":
		return a.parseResponseItem(l, raw)

	case "event_msg":
		return a.parseEventMsg(l, raw)

	default:
		// world_state (a config/instructions snapshot) and
		// token_usage_record (pure telemetry), plus any future bookkeeping
		// type we don't recognize yet: not conversation, nothing to store.
		return ingest.ParseResult{Skip: true}, nil
	}
}

type responseItemPayload struct {
	Type string `json:"type"` // message | custom_tool_call | custom_tool_call_output | reasoning | ...

	// message
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // []contentBlock
	Meta    struct {
		ContentItemKinds []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`

	// custom_tool_call
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`

	// custom_tool_call_output
	Output json.RawMessage `json:"output"` // []contentBlock
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (a *Adapter) parseResponseItem(l line, raw []byte) (ingest.ParseResult, error) {
	var p responseItemPayload
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("response_item payload: %w", err)
	}
	upd := ingest.SessionUpdate{NativeID: a.sessionID, CWD: a.cwd, Timestamp: l.Timestamp}

	switch p.Type {
	case "message":
		return a.parseMessage(p, raw, l.Timestamp, upd)

	case "custom_tool_call":
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role: "assistant", Kind: "tool_call",
				ToolName: p.Name, ToolUseID: p.CallID, Content: p.Input,
				Raw: raw, CreatedAt: l.Timestamp,
				FileTouches: PatchFileTouches(p.Name, p.Input),
			},
			Session: upd,
		}, nil

	case "custom_tool_call_output":
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role: "tool", Kind: "tool_result",
				ToolUseID: p.CallID, Content: flattenBlocks(p.Output),
				Raw: raw, CreatedAt: l.Timestamp,
			},
			Session: upd,
		}, nil

	case "reasoning":
		// UNVERIFIED against a real file (see package doc). Best-effort
		// from the public Responses API's reasoning-item shape: a
		// "summary" list of {type, text} blocks. If that doesn't match,
		// degrade to Skip — never store a guess as if it were real data.
		var r struct {
			Summary []contentBlock `json:"summary"`
		}
		if err := json.Unmarshal(l.Payload, &r); err != nil || len(r.Summary) == 0 {
			return ingest.ParseResult{Skip: true, Session: upd}, nil
		}
		text := ""
		for _, b := range r.Summary {
			text += b.Text
		}
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role: "assistant", Kind: "thinking", Content: text,
				Raw: raw, CreatedAt: l.Timestamp,
			},
			Session: upd,
		}, nil

	default:
		return ingest.ParseResult{Skip: true, Session: upd}, nil
	}
}

// typedBlock decides whether block i of a role=user/developer message is
// something the person typed — see the package doc for why this needs
// content_item_kinds rather than just the raw role.
//
// content_item_kinds has one entry per block ("user.text", "user.image",
// "environments.environment_context", ...), so each block is judged on its
// own: real messages mix them, e.g. [user.text, user.image], or AGENTS.md +
// environment context in one message. When kinds are missing (older Codex
// versions, the most common case in real logs), the block's own text is
// checked for the wrappers the harness puts around injected context
// (ingest.IsInjectedUserText). Trusting the raw role there, as an earlier
// version did, stored hundreds of <environment_context>, AGENTS.md and
// <turn_aborted> blocks as if the person had typed them.
func typedBlock(role string, kinds []string, i int, b contentBlock) bool {
	if role != "user" || b.Type != "input_text" {
		return false // developer is always framework context; images aren't text
	}
	if len(kinds) == 0 {
		return !ingest.IsInjectedUserText(b.Text)
	}
	return i < len(kinds) && kinds[i] == "user.text"
}

func (a *Adapter) parseMessage(p responseItemPayload, raw []byte, timestamp string, upd ingest.SessionUpdate) (ingest.ParseResult, error) {
	var blocks []contentBlock
	if err := json.Unmarshal(p.Content, &blocks); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("message.content: %w", err)
	}
	if len(blocks) == 0 {
		return ingest.ParseResult{Skip: true, Session: upd}, nil
	}

	if p.Role == "assistant" {
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role: "assistant", Kind: "text", Content: blocks[0].Text,
				Raw: raw, CreatedAt: timestamp,
			},
			Session: upd,
		}, nil
	}

	// A user message can hold several blocks: the prompt plus images, or
	// several injected sections. Reading every block (not just the first)
	// keeps a prompt that comes after injected context, and anything typed
	// makes the whole message a user turn.
	var typed, injected []string
	for i, b := range blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		if typedBlock(p.Role, p.Meta.ContentItemKinds, i, b) {
			// Only a genuine human prompt is escaped by the extension;
			// injected framework context is stored as written.
			typed = append(typed, cleanUserText(b.Text))
		} else {
			injected = append(injected, b.Text)
		}
	}
	msg := &ingest.RawMessage{Raw: raw, CreatedAt: timestamp}
	switch {
	case len(typed) > 0:
		msg.Role, msg.Kind, msg.Content = "user", "text", strings.Join(typed, "\n\n")
	case len(injected) > 0:
		msg.Role, msg.Kind, msg.Content = "system", "event", strings.Join(injected, "\n\n")
	default:
		return ingest.ParseResult{Skip: true, Session: upd}, nil // images only
	}
	return ingest.ParseResult{Message: msg, Session: upd}, nil
}

type eventMsgPayload struct {
	Type  string `json:"type"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Adapter) parseEventMsg(l line, raw []byte) (ingest.ParseResult, error) {
	var p eventMsgPayload
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("event_msg payload: %w", err)
	}
	upd := ingest.SessionUpdate{NativeID: a.sessionID, CWD: a.cwd, Timestamp: l.Timestamp}

	if p.Type == "task_complete" && p.Error != nil && p.Error.Message != "" {
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role: "system", Kind: "event", Content: p.Error.Message,
				Raw: raw, CreatedAt: l.Timestamp,
			},
			Session: upd,
		}, nil
	}
	// task_started, item_completed, token_count, and any other event_msg
	// subtype: a UI-facing broadcast that duplicates response_item's
	// authoritative record, or pure telemetry. Not stored.
	return ingest.ParseResult{Skip: true}, nil
}

// flattenBlocks concatenates every block's text, regardless of its `type`
// (Codex reuses "input_text" for tool output, not just user input, so this
// doesn't gate on a specific type name the way it plausibly could).
func flattenBlocks(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	out := ""
	for _, b := range blocks {
		out += b.Text
	}
	return out
}
