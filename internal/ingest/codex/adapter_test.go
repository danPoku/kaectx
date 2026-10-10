// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"bufio"
	"os"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantSkip    bool
		wantErr     bool
		wantMessage bool
		wantRole    string
		wantKind    string
		wantContent string
		wantTool    string
		wantModel   string
	}{
		{
			name:        "session_meta sets the session id and carries no message",
			line:        `{"timestamp":"2026-09-20T10:00:00Z","type":"session_meta","payload":{"session_id":"s1","cwd":"/home/dev/proj","cli_version":"0.153.0"}}`,
			wantMessage: false,
		},
		{
			name:        "turn_context updates model/cwd, no message",
			line:        `{"timestamp":"2026-09-20T10:00:01Z","type":"turn_context","payload":{"cwd":"/home/dev/proj","model":"gpt-5.5"}}`,
			wantMessage: false, wantModel: "gpt-5.5",
		},
		{
			name:        "genuine user turn (content_item_kinds says user.text)",
			line:        `{"timestamp":"2026-09-20T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"how many files?"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "how many files?",
		},
		{
			name:        "framework-injected context under role=user is NOT a user turn",
			line:        `{"timestamp":"2026-09-20T10:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>..."}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]}}}`,
			wantMessage: true, wantRole: "system", wantKind: "event",
		},
		{
			name:        "role=developer is always framework context",
			line:        `{"timestamp":"2026-09-20T10:00:04Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"skills..."}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["host_skills.instructions"]}}}`,
			wantMessage: true, wantRole: "system", wantKind: "event",
		},
		{
			name:        "role=user with no content_item_kinds at all is trusted at face value",
			line:        `{"timestamp":"2026-09-20T10:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "hello",
		},
		// The next four shapes are the common ones in real rollouts that
		// carry no content_item_kinds (older Codex versions).
		{
			name:        "AGENTS.md + environment context with no kinds is injected, not a user turn",
			line:        `{"timestamp":"2026-09-20T10:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /home/dev/proj\n\n<INSTRUCTIONS>..."},{"type":"input_text","text":"<environment_context>\n  <cwd>/home/dev/proj</cwd>"}]}}`,
			wantMessage: true, wantRole: "system", wantKind: "event",
		},
		{
			name:        "turn_aborted notice with no kinds is injected",
			line:        `{"timestamp":"2026-09-20T10:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted the previous turn.\n</turn_aborted>"}]}}`,
			wantMessage: true, wantRole: "system", wantKind: "event",
		},
		{
			name:        "prompt with images keeps the typed text and drops the <image> wrappers",
			line:        `{"timestamp":"2026-09-20T10:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"why is this modal clipped?"},{"type":"input_text","text":"<image>"},{"type":"input_image","image_url":"data:..."},{"type":"input_text","text":"</image>"}]}}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "why is this modal clipped?",
		},
		{
			name:        "per-block kinds: a prompt after injected context is still the user's turn",
			line:        `{"timestamp":"2026-09-20T10:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>..."},{"type":"input_text","text":"fix the toast"},{"type":"input_image"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context","user.text","user.image"]}}}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "fix the toast",
		},
		{
			name:        "assistant text",
			line:        `{"timestamp":"2026-09-20T10:00:06Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"here you go"}]}}`,
			wantMessage: true, wantRole: "assistant", wantKind: "text", wantContent: "here you go",
		},
		{
			name:        "custom_tool_call",
			line:        `{"timestamp":"2026-09-20T10:00:07Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-1","name":"exec","input":"ls -1"}}`,
			wantMessage: true, wantRole: "assistant", wantKind: "tool_call", wantTool: "exec", wantContent: "ls -1",
		},
		{
			name:        "custom_tool_call_output maps to role=tool",
			line:        `{"timestamp":"2026-09-20T10:00:08Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"main.go\n"}]}}`,
			wantMessage: true, wantRole: "tool", wantKind: "tool_result", wantContent: "main.go\n",
		},
		{
			name:        "task_complete with an error is kept as a system event",
			line:        `{"timestamp":"2026-09-20T10:00:09Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"401 Unauthorized"}}}`,
			wantMessage: true, wantRole: "system", wantKind: "event", wantContent: "401 Unauthorized",
		},
		{name: "task_started is bookkeeping, skipped", line: `{"timestamp":"2026-09-20T10:00:10Z","type":"event_msg","payload":{"type":"task_started"}}`, wantSkip: true},
		{name: "item_completed duplicates response_item, skipped", line: `{"timestamp":"2026-09-20T10:00:11Z","type":"event_msg","payload":{"type":"item_completed"}}`, wantSkip: true},
		{name: "task_complete with no error is skipped", line: `{"timestamp":"2026-09-20T10:00:12Z","type":"event_msg","payload":{"type":"task_complete"}}`, wantSkip: true},
		{name: "world_state is a config snapshot, skipped", line: `{"timestamp":"2026-09-20T10:00:13Z","type":"world_state","payload":{}}`, wantSkip: true},
		{name: "unrecognized reasoning shape degrades to skip, never errors", line: `{"timestamp":"2026-09-20T10:00:14Z","type":"response_item","payload":{"type":"reasoning"}}`, wantSkip: true},
		{name: "malformed JSON returns an error, never panics", line: `{"type": "response_item", "payload": {`, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Adapter{sessionID: "s1"} // most cases need an established session
			res, err := a.ParseLine([]byte(c.line))
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseLine(%q) = nil error, want one", c.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLine(%q): unexpected error: %v", c.line, err)
			}
			if res.Skip != c.wantSkip {
				t.Errorf("Skip = %v, want %v", res.Skip, c.wantSkip)
			}
			if c.wantModel != "" && res.Session.Model != c.wantModel {
				t.Errorf("Session.Model = %q, want %q", res.Session.Model, c.wantModel)
			}
			if !c.wantMessage {
				if res.Message != nil {
					t.Errorf("Message = %+v, want nil", res.Message)
				}
				return
			}
			if res.Message == nil {
				t.Fatalf("Message = nil, want non-nil")
			}
			if res.Message.Role != c.wantRole {
				t.Errorf("Role = %q, want %q", res.Message.Role, c.wantRole)
			}
			if res.Message.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q", res.Message.Kind, c.wantKind)
			}
			if c.wantContent != "" && res.Message.Content != c.wantContent {
				t.Errorf("Content = %q, want %q", res.Message.Content, c.wantContent)
			}
			if c.wantTool != "" && res.Message.ToolName != c.wantTool {
				t.Errorf("ToolName = %q, want %q", res.Message.ToolName, c.wantTool)
			}
		})
	}
}

func TestParseLineTracksSessionIDAcrossLines(t *testing.T) {
	a := New()
	metaLine := `{"timestamp":"2026-09-20T10:00:00Z","type":"session_meta","payload":{"session_id":"s1","cwd":"/x"}}`
	if _, err := a.ParseLine([]byte(metaLine)); err != nil {
		t.Fatalf("ParseLine(session_meta): %v", err)
	}

	userLine := `{"timestamp":"2026-09-20T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`
	res, err := a.ParseLine([]byte(userLine))
	if err != nil {
		t.Fatalf("ParseLine(user message): %v", err)
	}
	if res.Session.NativeID != "s1" {
		t.Errorf("Session.NativeID = %q, want %q (remembered from session_meta)", res.Session.NativeID, "s1")
	}
}

// TestParseLineAgainstFixture walks the real-shaped fixture end to end and
// checks aggregate classification counts, as a sanity check that the
// table-driven cases above match a real multi-line session.
func TestParseLineAgainstFixture(t *testing.T) {
	f, err := os.Open("testdata/session.jsonl")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	a := New()
	var messages, skipped, sessionOnly int
	kinds := map[string]int{}
	roles := map[string]int{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		res, err := a.ParseLine(sc.Bytes())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		switch {
		case res.Skip:
			skipped++
		case res.Message != nil:
			messages++
			kinds[res.Message.Kind]++
			roles[res.Message.Role]++
		default:
			sessionOnly++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if skipped != 4 { // task_started, item_completed, token_count, task_complete(no error)
		t.Errorf("skipped = %d, want 4", skipped)
	}
	if sessionOnly != 2 { // session_meta, turn_context
		t.Errorf("sessionOnly = %d, want 2", sessionOnly)
	}
	if messages != 9 {
		t.Errorf("messages = %d, want 9", messages)
	}
	wantKinds := map[string]int{"event": 2, "text": 4, "tool_call": 2, "tool_result": 1}
	for k, want := range wantKinds {
		if kinds[k] != want {
			t.Errorf("kind %q count = %d, want %d", k, kinds[k], want)
		}
	}
	if roles["system"] != 2 {
		t.Errorf(`role "system" count = %d, want 2 (framework-injected context correctly excluded from user turns)`, roles["system"])
	}
	if roles["user"] != 2 {
		t.Errorf(`role "user" count = %d, want 2 (only the genuine user.text-tagged turns)`, roles["user"])
	}
}
