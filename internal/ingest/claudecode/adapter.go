// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package claudecode adapts Claude Code's session JSONL format (one JSON
// object per line under ~/.claude/projects/<sanitized-cwd>/<session-id>.jsonl)
// into the shapes internal/ingest knows how to store.
//
// The format carries far more than conversation: alongside `user` and
// `assistant` turns, a real session file interleaves harness/UI bookkeeping
// types (`mode`, `permission-mode`, `atis-latch`, `last-prompt`, `ai-title`,
// `file-history-snapshot`, `file-history-delta`, `attachment`, ...) that
// this adapter treats as non-conversational and skips. `ai-title` is the one
// exception worth keeping: it's an agent-generated session title, exactly
// what sessions.title wants ("first user prompt, trimmed, or
// agent-provided").
//
// A subtlety discovered from a real file: a single logical assistant turn
// that both thinks and calls a tool isn't one line with a multi-block
// content array — Claude Code writes each content block as its OWN line,
// sharing message.id and requestId with an incrementing apiBlockIndex. This
// adapter treats each line as at most one block; it never needs to
// reassemble them, because each block becomes its own messages row anyway
// (messages.kind already distinguishes text/tool_call/tool_result/thinking).
package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/danPoku/kaectx/internal/ingest"
)

type Adapter struct{}

func New() Adapter { return Adapter{} }

func (Adapter) Agent() string { return "claude-code" }

// line is the subset of Claude Code's JSONL schema this adapter reads.
// Both "sessionId" and a redundant "session_id" appear in real files; only
// the former is used.
type line struct {
	Type      string        `json:"type"`
	Message   *messageBlock `json:"message"`
	Content   string        `json:"content"` // system lines only: plain string
	Timestamp string        `json:"timestamp"`
	SessionID string        `json:"sessionId"`
	CWD       string        `json:"cwd"`
	GitBranch string        `json:"gitBranch"`
	Version   string        `json:"version"`
	AITitle   string        `json:"aiTitle"`
	// IsMeta marks a role=user line that Claude Code itself injected (a
	// skill's instructions, a usage-limit notice, an image placeholder):
	// not something the person typed.
	IsMeta bool `json:"isMeta"`
	// IsSidechain is true on every line of a subagent transcript
	// (<session>/subagents/agent-*.jsonl), which reuses the parent's
	// sessionId.
	IsSidechain bool `json:"isSidechain"`
}

type messageBlock struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // string, OR an array of contentBlock
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result: string, OR an array of {type,text} sub-blocks
}

func (a Adapter) ParseLine(raw []byte) (ingest.ParseResult, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("invalid JSON: %w", err)
	}

	switch l.Type {
	case "user", "assistant":
		return a.parseConversational(l, raw)
	case "system":
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role:      "system",
				Kind:      "event",
				Content:   l.Content,
				Raw:       raw,
				CreatedAt: l.Timestamp,
			},
			Session: sessionUpdate(l),
		}, nil
	case "ai-title":
		upd := sessionUpdate(l)
		upd.Title = l.AITitle
		return ingest.ParseResult{Session: upd}, nil
	default:
		// mode, permission-mode, atis-latch, last-prompt,
		// file-history-snapshot, file-history-delta, attachment, and any
		// future bookkeeping type we don't recognize yet: not
		// conversation, nothing to store.
		return ingest.ParseResult{Skip: true}, nil
	}
}

func sessionUpdate(l line) ingest.SessionUpdate {
	return ingest.SessionUpdate{
		NativeID:  l.SessionID,
		CWD:       l.CWD,
		GitBranch: l.GitBranch,
		Version:   l.Version,
		Timestamp: l.Timestamp,
		Sidechain: l.IsSidechain,
	}
}

func (a Adapter) parseConversational(l line, raw []byte) (ingest.ParseResult, error) {
	if l.Message == nil {
		return ingest.ParseResult{}, fmt.Errorf("%s line missing message", l.Type)
	}
	upd := sessionUpdate(l)
	upd.Model = l.Message.Model

	// Plain string content: a real user prompt (or occasionally
	// system-injected boilerplate under role=user).
	var text string
	if err := json.Unmarshal(l.Message.Content, &text); err == nil {
		if l.Message.Role == "user" {
			return ingest.ParseResult{Message: userMessage(l, raw, []string{text}), Session: upd}, nil
		}
		return ingest.ParseResult{
			Message: &ingest.RawMessage{
				Role: l.Message.Role, Kind: "text", Content: text,
				Raw: raw, CreatedAt: l.Timestamp,
			},
			Session: upd,
		}, nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("message.content is neither a string nor a block array: %w", err)
	}
	if len(blocks) == 0 {
		return ingest.ParseResult{Skip: true}, nil
	}

	// A user prompt can span several blocks. The VS Code extension sends
	// the open file and the selection as their own text blocks ahead of
	// what the person typed, and attached images come as image blocks:
	// real lines look like [ide_opened_file, prompt] or [text, image,
	// text]. Reading only the first block stored the IDE notice and
	// dropped the actual prompt, so every text block is read here.
	if l.Message.Role == "user" {
		if texts, ok := promptBlocks(blocks); ok {
			if len(texts) == 0 {
				return ingest.ParseResult{Skip: true}, nil // images only
			}
			return ingest.ParseResult{Message: userMessage(l, raw, texts), Session: upd}, nil
		}
	}

	// Everything else (assistant output, tool results) was observed with
	// exactly one block per line. Take the first defensively rather than
	// assume, so an unexpected shape degrades instead of crashing.
	b := blocks[0]

	msg := &ingest.RawMessage{Raw: raw, CreatedAt: l.Timestamp}
	switch b.Type {
	case "text":
		msg.Role, msg.Kind, msg.Content = l.Message.Role, "text", b.Text
	case "thinking":
		msg.Role, msg.Kind, msg.Content = l.Message.Role, "thinking", b.Thinking
	case "tool_use":
		msg.Role, msg.Kind = l.Message.Role, "tool_call"
		msg.ToolName, msg.ToolUseID = b.Name, b.ID
		msg.Content = string(b.Input)
		if ft := fileTouchFor(b.Name, b.Input); ft != nil {
			msg.FileTouches = []ingest.FileTouch{*ft}
		}
	case "tool_result":
		// A tool result is logged under role=user in the raw format (it's
		// how the Anthropic API represents "continue the conversation with
		// this tool's output"), but semantically it's neither the human nor
		// the assistant speaking — messages.role has a dedicated 'tool'
		// value for exactly this.
		msg.Role, msg.Kind = "tool", "tool_result"
		msg.ToolUseID = b.ToolUseID
		msg.Content = flattenToolResultContent(b.Content)
	default:
		return ingest.ParseResult{Skip: true}, nil
	}
	return ingest.ParseResult{Message: msg, Session: upd}, nil
}

// promptBlocks returns the text of every text block when blocks are a
// prompt (only text and image blocks). ok is false for any other mix, such
// as a tool result, which the single-block path handles.
func promptBlocks(blocks []contentBlock) (texts []string, ok bool) {
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
		default:
			return nil, false
		}
	}
	return texts, true
}

// userMessage builds the message for a role=user line from its text
// blocks. Blocks the harness injected (see ingest.IsInjectedUserText) are
// separated from what the person typed: if anything typed remains, the
// message is that, as a user turn; if everything was injected, or Claude
// Code flagged the whole line isMeta, it is stored as a system event. Either
// way the raw line keeps every block.
func userMessage(l line, raw []byte, texts []string) *ingest.RawMessage {
	var typed, injected []string
	for _, t := range texts {
		t = cleanUserText(t)
		if strings.TrimSpace(t) == "" {
			continue
		}
		if l.IsMeta || ingest.IsInjectedUserText(t) {
			injected = append(injected, t)
		} else {
			typed = append(typed, t)
		}
	}
	msg := &ingest.RawMessage{Raw: raw, CreatedAt: l.Timestamp}
	if len(typed) > 0 {
		msg.Role, msg.Kind, msg.Content = "user", "text", strings.Join(typed, "\n\n")
	} else {
		msg.Role, msg.Kind, msg.Content = "system", "event", strings.Join(injected, "\n\n")
	}
	return msg
}

// fileTouchAction maps a tool name to file_touches.action. Confirmed against
// this project's own real session file for Read/Write/Edit, which all key
// their target path as "file_path"; MultiEdit is assumed to share Edit's
// shape (same editing tool family) but hasn't been seen directly. Write
// can't be told apart from "create" vs. "overwrite an existing file" just
// from the tool call — Claude Code always sends the full file content
// either way — so it's mapped to the more conservative 'edit' rather than
// asserting 'create'. NotebookEdit is deliberately NOT handled: its path
// field name hasn't been confirmed from a real sample and guessing wrong
// would silently record garbage in file_touches.
var fileTouchAction = map[string]string{
	"Read":      "read",
	"Edit":      "edit",
	"MultiEdit": "edit",
	"Write":     "edit",
}

// fileTouchFor extracts a FileTouch from a recognized file-editing tool's
// JSON input, or returns nil for anything else (most tools, like Bash,
// don't name a file in a structured way this can reliably pull out).
func fileTouchFor(toolName string, input json.RawMessage) *ingest.FileTouch {
	action, ok := fileTouchAction[toolName]
	if !ok {
		return nil
	}
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.FilePath == "" {
		return nil
	}
	return &ingest.FileTouch{Path: args.FilePath, Action: action}
}

// flattenToolResultContent handles tool_result's content being either a
// plain string or a list of {type, text} sub-blocks (as used for structured
// or multi-part tool output). Non-text sub-blocks (e.g. images) are noted
// rather than silently dropped.
func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	out := ""
	for _, b := range blocks {
		if b.Type == "text" {
			out += b.Text
		} else {
			out += "[" + b.Type + " content omitted]"
		}
	}
	return out
}
