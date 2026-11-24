// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package antigravity adapts Antigravity CLI's transcript format into the
// shapes internal/ingest knows how to store. Antigravity CLI (the `agy`
// command) is Google's replacement for Gemini CLI.
//
// Everything here was read from real files written by Antigravity CLI
// 1.5.16 on Windows: one interactive conversation in a repository and one
// headless (`agy -p`) conversation in a scratch folder. Nothing is assumed
// from documentation.
//
// Where the files are. The CLI keeps its data in ~/.gemini/antigravity-cli.
// Each conversation has a folder named by its id:
//
//	brain/<conversation-id>/.system_generated/logs/transcript_full.jsonl
//
// one JSON object per step, appended as the conversation goes. Beside it:
//
//   - transcript.jsonl: the same steps with long tool output cut short
//     (marked by a truncated_fields list). Not read; the full one has it all.
//   - chunks/transcript_full/00000000.jsonl, 00000001.jsonl, ...: the full
//     transcript again, cut every 102400 bytes whether or not a line ends
//     there. Not read; transcript_full.jsonl is those pieces joined.
//   - ../steps/<n>/output.txt, ../../scratch/<n>: one folder per tool call.
//
// So, unlike Claude Code and Codex, most .jsonl files under the root are not
// session logs, and the adapter says which one is (WantsFile, SkipDir).
//
// Three structural differences from the other two agents:
//
//  1. A line does not say which conversation it belongs to; only the folder
//     name does. The driver hands the adapter the path (BindFile) and the
//     id is read from it. Like a ward where the charts carry no patient
//     name and you tell whose they are from the bed they hang on.
//
//  2. No line records the folder the agent was working in. That is kept in
//     a separate SQLite database, conversation_summaries.db, with the
//     conversation's title. The adapter reads it read-only, once per file
//     per run. See workspace.go.
//
//  3. One PLANNER_RESPONSE line holds a whole model step: its thinking, its
//     text and every tool call. It becomes several messages
//     (ingest.ParseResult.More). Tool calls carry no id, and their results
//     arrive as the GENERIC lines that follow, one per call, in the same
//     order, so the adapter pairs them by position.
//
// Step types seen: USER_INPUT, PLANNER_RESPONSE, GENERIC, SYSTEM_MESSAGE,
// ERROR_MESSAGE. Any other is reported as unknown, not guessed at. Tools
// seen: run_command, read_file, write_file, edit_file.
//
// Not verified, because no real file showed it: whether a step is ever
// written before it finishes and rewritten later (every line seen had status
// DONE or ERROR); subagent conversations (the database has a
// parent_conversation_id column, always empty here); and the desktop app's
// folder, ~/.gemini/antigravity, which has the same layout but held no
// conversations on the machine this was built on. The transcript does not
// record the CLI version, so sessions.version stays empty.
package antigravity

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danPoku/kaectx/internal/ingest"
)

// TranscriptName is the one file per conversation this adapter reads.
const TranscriptName = "transcript_full.jsonl"

// Adapter is stateful: it must not be shared across files. Use New() to get
// a fresh one, or ingest.RunAll's per-file factory.
type Adapter struct {
	// sessionID and dataDir come from the transcript's path (BindFile).
	// dataDir is the folder holding brain/ and conversation_summaries.db.
	sessionID string
	dataDir   string

	// looked is true once the conversation's folder and title have been
	// asked for, found or not, so a run asks once however many lines it
	// parses.
	looked bool
	cwd    string
	title  string

	// pending holds the ids given to the latest step's tool calls that have
	// not had a result yet, oldest first. See the package doc, point 3.
	pending []string
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Agent() string { return "antigravity" }

// BindFile takes the conversation id from the transcript's location:
// <data>/brain/<id>/.system_generated/logs/transcript_full.jsonl. A file
// anywhere else (a transcript copied out by hand) has no id to give, and
// ParseLine then says so instead of filing its lines under a made-up one.
func (a *Adapter) BindFile(path string) {
	*a = Adapter{}
	if !a.WantsFile(path) {
		return
	}
	conversation := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	brain := filepath.Dir(conversation)
	if filepath.Base(brain) != "brain" {
		return
	}
	a.sessionID = filepath.Base(conversation)
	a.dataDir = filepath.Dir(brain)
}

// WantsFile reports whether path is a conversation's full transcript. Its
// shortened copy, its 102400-byte pieces, the prompt history and anything
// the user uploaded are all .jsonl files under the same root and are not.
func (a *Adapter) WantsFile(path string) bool {
	logs := filepath.Dir(path)
	return filepath.Base(path) == TranscriptName &&
		filepath.Base(logs) == "logs" &&
		filepath.Base(filepath.Dir(logs)) == ".system_generated"
}

// skippedDirs are the folders inside a conversation that never hold its
// transcript. steps and scratch gain a subfolder per tool call.
var skippedDirs = map[string]bool{
	"chunks":         true,
	"steps":          true,
	"scratch":        true,
	".user_uploaded": true,
}

func (a *Adapter) SkipDir(path string) bool { return skippedDirs[filepath.Base(path)] }

// step is the subset of a transcript line this adapter reads. Token counts
// (input_tokens, cache_read_tokens, output_tokens) and step_index are left
// in raw and not otherwise used.
type step struct {
	StepIndex int        `json:"step_index"`
	Type      string     `json:"type"`
	Status    string     `json:"status"` // DONE | ERROR
	CreatedAt string     `json:"created_at"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking"`
	Error     string     `json:"error"`
	Model     string     `json:"model"`
	ToolCalls []toolCall `json:"tool_calls"`
}

type toolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (a *Adapter) ParseLine(raw []byte) (ingest.ParseResult, error) {
	var s step
	if err := json.Unmarshal(raw, &s); err != nil {
		return ingest.ParseResult{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if a.sessionID == "" {
		return ingest.ParseResult{}, fmt.Errorf("cannot tell which conversation this transcript belongs to: expected it at brain/<conversation-id>/.system_generated/logs/%s", TranscriptName)
	}
	if !a.looked {
		a.looked = true
		a.cwd, a.title = lookupConversation(a.dataDir, a.sessionID)
	}
	upd := ingest.SessionUpdate{
		NativeID: a.sessionID, CWD: a.cwd, Title: a.title,
		Model: s.Model, Timestamp: s.CreatedAt,
	}

	switch s.Type {
	case "USER_INPUT":
		// A new prompt ends whatever the previous step was waiting for.
		a.pending = nil
		text := userRequest(s.Content)
		if text == "" {
			return ingest.ParseResult{Session: upd}, nil
		}
		return ingest.ParseResult{
			Message: &ingest.RawMessage{Role: "user", Kind: "text", Content: text, Raw: raw, CreatedAt: s.CreatedAt},
			Session: upd,
		}, nil

	case "PLANNER_RESPONSE":
		msgs := a.plannerMessages(s, raw)
		if len(msgs) == 0 {
			return ingest.ParseResult{Session: upd}, nil
		}
		return ingest.ParseResult{Message: &msgs[0], More: msgs[1:], Session: upd}, nil

	case "GENERIC":
		// The result of the oldest tool call still waiting for one. A
		// failed or refused call is written the same way, with status ERROR
		// and the reason in both error and content.
		id := ""
		if len(a.pending) > 0 {
			id, a.pending = a.pending[0], a.pending[1:]
		}
		text := toolOutput(s.Content)
		if text == "" {
			text = s.Error
		}
		return ingest.ParseResult{
			Message: &ingest.RawMessage{Role: "tool", Kind: "tool_result", ToolUseID: id, Content: text, Raw: raw, CreatedAt: s.CreatedAt},
			Session: upd,
		}, nil

	case "SYSTEM_MESSAGE":
		// A reminder the harness sends the model between steps. Kept as an
		// event: stored, but not a turn and not indexed for search.
		return ingest.ParseResult{
			Message: &ingest.RawMessage{Role: "system", Kind: "event", Content: s.Content, Raw: raw, CreatedAt: s.CreatedAt},
			Session: upd,
		}, nil

	case "ERROR_MESSAGE":
		// For example "API error (attempt 2): ... No capacity available".
		return ingest.ParseResult{
			Message: &ingest.RawMessage{Role: "system", Kind: "event", Content: s.Error, Raw: raw, CreatedAt: s.CreatedAt},
			Session: upd,
		}, nil

	default:
		name := s.Type
		if name == "" {
			name = "(no type)"
		}
		return ingest.ParseResult{Skip: true, Unknown: name, Session: ingest.SessionUpdate{
			NativeID: a.sessionID, CWD: a.cwd, Timestamp: s.CreatedAt,
		}}, nil
	}
}

// plannerMessages splits one model step into its messages: thinking, then
// text, then each tool call. Any of the three can be absent.
//
// The full line is kept as raw on the first message only. Repeating it on
// each would store a long step several times over; the others get a short
// pointer to the step instead.
func (a *Adapter) plannerMessages(s step, raw []byte) []ingest.RawMessage {
	var msgs []ingest.RawMessage
	if t := strings.TrimSpace(s.Thinking); t != "" {
		msgs = append(msgs, ingest.RawMessage{Role: "assistant", Kind: "thinking", Content: t})
	}
	if t := strings.TrimSpace(s.Content); t != "" {
		msgs = append(msgs, ingest.RawMessage{Role: "assistant", Kind: "text", Content: t})
	}
	// A step's calls replace any still unanswered from the step before, so
	// one missing result cannot shift every later pairing by one.
	a.pending = nil
	for i, c := range s.ToolCalls {
		// The log gives calls no id, so one is made from the step number
		// and the call's place in it. It only has to be unique in the file.
		id := "step" + strconv.Itoa(s.StepIndex) + "." + strconv.Itoa(i)
		a.pending = append(a.pending, id)
		msgs = append(msgs, ingest.RawMessage{
			Role: "assistant", Kind: "tool_call",
			ToolName: c.Name, ToolUseID: id, Content: string(c.Args),
			FileTouches: fileTouches(c),
		})
	}
	pointer := []byte(`{"step_index":` + strconv.Itoa(s.StepIndex) + `,"type":"PLANNER_RESPONSE","note":"the full line is stored with this step's first message"}`)
	for i := range msgs {
		msgs[i].CreatedAt = s.CreatedAt
		msgs[i].Raw = pointer
	}
	if len(msgs) > 0 {
		msgs[0].Raw = raw
	}
	return msgs
}

// fileToolActions maps the file tools seen in real transcripts to
// file_touches.action. write_file is "edit", as Claude Code's Write is: the
// call does not say whether the file existed, and "create" would be a guess.
// A file removed through run_command leaves no touch, because a shell
// command does not name its files in a form that can be read reliably.
var fileToolActions = map[string]string{
	"read_file":  "read",
	"write_file": "edit",
	"edit_file":  "edit",
}

// fileTouches reads the file a tool call names. All three file tools put it
// in "Path", absolute, with either kind of slash.
func fileTouches(c toolCall) []ingest.FileTouch {
	action, ok := fileToolActions[c.Name]
	if !ok {
		return nil
	}
	var args struct {
		Path string `json:"Path"`
	}
	if err := json.Unmarshal(c.Args, &args); err != nil || args.Path == "" {
		return nil
	}
	return []ingest.FileTouch{{Path: args.Path, Action: action}}
}
