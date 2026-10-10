// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package claudecode

import (
	"bufio"
	"os"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
)

func TestParseLine(t *testing.T) {
	a := New()

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
		wantTitle   string
	}{
		{
			name:        "plain user text",
			line:        `{"type":"user","message":{"role":"user","content":"hello there"},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "hello there",
		},
		// Harness text under role=user, in the shapes seen in real logs.
		{
			name:        "task-notification is a system event, not a user turn",
			line:        `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<summary>Monitor event: \"transcription progress\"</summary>\n</task-notification>"},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantMessage: true, wantRole: "system", wantKind: "event",
		},
		{
			name:        "isMeta line is a system event whatever it says",
			line:        `{"type":"user","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"Your claude.ai usage limit has reset. Continue the task."}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantMessage: true, wantRole: "system", wantKind: "event",
		},
		{
			name:        "IDE context block before the prompt: the prompt is kept, the notice dropped",
			line:        `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<ide_opened_file>The user opened the file ollama.go in the IDE.</ide_opened_file>"},{"type":"text","text":"why does embed time out?"}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "why does embed time out?",
		},
		{
			name:        "prompt split around an image keeps both text blocks",
			line:        `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"this toast"},{"type":"image","source":{"type":"base64","data":"..."}},{"type":"text","text":"looks wrong"}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantMessage: true, wantRole: "user", wantKind: "text", wantContent: "this toast\n\nlooks wrong",
		},
		{
			name:        "slash command is a genuine user turn",
			line:        `{"type":"user","message":{"role":"user","content":"<command-message>review</command-message>\n<command-name>/review</command-name>"},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantMessage: true, wantRole: "user", wantKind: "text",
		},
		{
			name:     "image-only prompt has no text to store",
			line:     `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"..."}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantSkip: true,
		},
		{
			name:        "assistant thinking block",
			line:        `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:01Z"}`,
			wantMessage: true, wantRole: "assistant", wantKind: "thinking", wantContent: "pondering",
		},
		{
			name:        "assistant tool_use block",
			line:        `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:02Z"}`,
			wantMessage: true, wantRole: "assistant", wantKind: "tool_call", wantTool: "Bash",
		},
		{
			name:        "tool_result with string content maps to role=tool",
			line:        `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file1\nfile2","is_error":false}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:03Z"}`,
			wantMessage: true, wantRole: "tool", wantKind: "tool_result", wantContent: "file1\nfile2",
		},
		{
			name:        "tool_result with block-array content",
			line:        `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"ok"}]}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:04Z"}`,
			wantMessage: true, wantRole: "tool", wantKind: "tool_result", wantContent: "ok",
		},
		{
			name:        "assistant text block",
			line:        `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"here's the answer"}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:05Z"}`,
			wantMessage: true, wantRole: "assistant", wantKind: "text", wantContent: "here's the answer",
		},
		{
			name:        "system line",
			line:        `{"type":"system","subtype":"informational","content":"a notice","sessionId":"s1","timestamp":"2026-09-20T10:00:06Z"}`,
			wantMessage: true, wantRole: "system", wantKind: "event", wantContent: "a notice",
		},
		{
			name:      "ai-title carries no message, only a session title update",
			line:      `{"type":"ai-title","aiTitle":"Fixing the WAL bug","sessionId":"s1"}`,
			wantTitle: "Fixing the WAL bug",
		},
		{name: "mode line is bookkeeping, skipped", line: `{"type":"mode","mode":"normal","sessionId":"s1"}`, wantSkip: true},
		{name: "permission-mode line is bookkeeping, skipped", line: `{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}`, wantSkip: true},
		{name: "attachment line is bookkeeping, skipped", line: `{"type":"attachment","attachment":{"type":"environment"},"sessionId":"s1"}`, wantSkip: true},
		{name: "malformed JSON returns an error, never panics", line: `{"type": "user", "message": {`, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
			if c.wantTitle != "" && res.Session.Title != c.wantTitle {
				t.Errorf("Session.Title = %q, want %q", res.Session.Title, c.wantTitle)
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

// TestParseLineAgainstFixture walks the real-shaped fixture file end to end
// and checks the aggregate classification counts, as a sanity check that
// the table-driven unit cases above match what a real multi-line session
// actually produces.
func TestParseLineAgainstFixture(t *testing.T) {
	f, err := os.Open("testdata/session.jsonl")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	a := New()
	var messages, skipped, titleUpdates int
	kinds := map[string]int{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		res, err := a.ParseLine(sc.Bytes())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if res.Skip {
			skipped++
			continue
		}
		if res.Message != nil {
			messages++
			kinds[res.Message.Kind]++
		} else if res.Session.Title != "" {
			titleUpdates++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if skipped != 2 { // mode, permission-mode
		t.Errorf("skipped = %d, want 2", skipped)
	}
	if titleUpdates != 1 { // the ai-title line
		t.Errorf("titleUpdates = %d, want 1", titleUpdates)
	}
	if messages != 8 {
		t.Errorf("messages = %d, want 8", messages)
	}
	wantKinds := map[string]int{"text": 3, "thinking": 1, "tool_call": 2, "tool_result": 2}
	for k, want := range wantKinds {
		if kinds[k] != want {
			t.Errorf("kind %q count = %d, want %d", k, kinds[k], want)
		}
	}

	var _ ingest.Adapter = a // Adapter satisfies the driver's interface
}

// TestFileTouchExtraction pins the "file_path" field name down against what
// was actually observed in this project's own real Claude Code session file
// for Read/Write/Edit (see the fileTouchAction doc comment) — a synthetic
// fixture using a made-up key like "path" would pass without ever proving
// extraction works against the real shape.
func TestFileTouchExtraction(t *testing.T) {
	a := New()

	cases := []struct {
		name      string
		line      string
		wantTouch *ingest.FileTouch
	}{
		{
			name:      "Read sets a FileTouch",
			line:      `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/home/dev/proj/main.go"}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:00Z"}`,
			wantTouch: &ingest.FileTouch{Path: "/home/dev/proj/main.go", Action: "read"},
		},
		{
			name:      "Edit sets a FileTouch",
			line:      `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/home/dev/proj/main.go","old_string":"a","new_string":"b"}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:01Z"}`,
			wantTouch: &ingest.FileTouch{Path: "/home/dev/proj/main.go", Action: "edit"},
		},
		{
			name:      "Write sets a FileTouch",
			line:      `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"/home/dev/proj/new.go","content":"package main"}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:02Z"}`,
			wantTouch: &ingest.FileTouch{Path: "/home/dev/proj/new.go", Action: "edit"},
		},
		{
			name:      "Bash does not set a FileTouch — no structured path to extract",
			line:      `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"cat main.go"}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:03Z"}`,
			wantTouch: nil,
		},
		{
			name:      "NotebookEdit is deliberately unhandled (unconfirmed field name)",
			line:      `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t5","name":"NotebookEdit","input":{"notebook_path":"/home/dev/proj/nb.ipynb"}}]},"sessionId":"s1","timestamp":"2026-09-20T10:00:04Z"}`,
			wantTouch: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := a.ParseLine([]byte(c.line))
			if err != nil {
				t.Fatalf("ParseLine: %v", err)
			}
			if res.Message == nil {
				t.Fatalf("Message = nil")
			}
			got := res.Message.FileTouches
			if (len(got) == 0) != (c.wantTouch == nil) {
				t.Fatalf("FileTouches = %+v, want %+v", got, c.wantTouch)
			}
			if len(got) > 0 && (len(got) != 1 || got[0] != *c.wantTouch) {
				t.Errorf("FileTouches = %+v, want [%+v]", got, *c.wantTouch)
			}
		})
	}
}
