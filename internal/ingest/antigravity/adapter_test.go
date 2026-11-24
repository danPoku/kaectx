// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package antigravity

import (
	"bufio"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
)

// transcriptPath is where Antigravity would keep conversation id's
// transcript under data. Nothing is created on disk.
func transcriptPath(data, id string) string {
	return filepath.Join(data, "brain", id, ".system_generated", "logs", TranscriptName)
}

// bound returns an adapter bound to a conversation whose data folder holds
// no database, so lookups find nothing and tests see only what the line says.
func bound(t *testing.T) *Adapter {
	t.Helper()
	a := New()
	a.BindFile(transcriptPath(t.TempDir(), "conv-1"))
	return a
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantErr     bool
		wantUnknown string
		wantRole    string // of the first message; "" means no message
		wantKind    string
		wantContent string
		wantMore    int
		wantModel   string
	}{
		{
			name:     "a prompt is unwrapped and loses the context Antigravity appends",
			line:     `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-24T08:08:09Z","content":"<USER_REQUEST>\nexplore this repo\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-24T08:08:09Z.\n</ADDITIONAL_METADATA>"}`,
			wantRole: "user", wantKind: "text", wantContent: "explore this repo",
		},
		{
			name:     "a model step with thinking and two calls becomes three messages",
			line:     `{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-24T08:08:21Z","model":"gemini-3.8-flash-high","thinking":"Look first.\n\n\n","tool_calls":[{"name":"read_file","args":{"Path":"C:/Users/dev/proj/go.mod"}},{"name":"run_command","args":{"CommandLine":"ls"}}]}`,
			wantRole: "assistant", wantKind: "thinking", wantContent: "Look first.", wantMore: 2, wantModel: "gemini-3.8-flash-high",
		},
		{
			name:     "a model step with only text is one text message",
			line:     `{"step_index":62,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-24T08:15:24Z","model":"gemini-3.8-flash-high","content":"Done."}`,
			wantRole: "assistant", wantKind: "text", wantContent: "Done.", wantModel: "gemini-3.8-flash-high",
		},
		{
			name:      "a model step with nothing in it stores no message",
			line:      `{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-24T08:15:24Z","model":"gemini-3.8-flash-high"}`,
			wantModel: "gemini-3.8-flash-high",
		},
		{
			name:     "a tool result loses its timing header",
			line:     `{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-24T08:08:16Z","content":"Created At: 2026-11-24T08:08:16Z\nCompleted At: 2026-11-24T08:08:21Z\nCreated At: 2026-11-24T08:08:16Z\nCompleted At: 2026-11-24T08:08:21Z\nCommand exited with code 0."}`,
			wantRole: "tool", wantKind: "tool_result", wantContent: "Command exited with code 0.",
		},
		{
			name:     "a refused tool call is still a tool result, with the reason",
			line:     `{"step_index":12,"source":"MODEL","type":"GENERIC","status":"ERROR","error":"user denied permission","created_at":"2026-11-24T08:20:26Z","content":"Created At: 2026-11-24T08:20:26Z\nCompleted At: 2026-11-24T08:20:26Z\nEncountered error in step execution: user denied permission"}`,
			wantRole: "tool", wantKind: "tool_result", wantContent: "Encountered error in step execution: user denied permission",
		},
		{
			name:     "a harness reminder is an event, not a turn",
			line:     `{"step_index":16,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-24T08:10:18Z","content":"Reminder: work efficiently."}`,
			wantRole: "system", wantKind: "event", wantContent: "Reminder: work efficiently.",
		},
		{
			name:     "an API error is an event",
			line:     `{"step_index":10,"source":"SYSTEM","type":"ERROR_MESSAGE","status":"DONE","error":"API error (attempt 1): HTTP 503","created_at":"2026-11-24T08:08:42Z"}`,
			wantRole: "system", wantKind: "event", wantContent: "API error (attempt 1): HTTP 503",
		},
		{
			name:        "a step type never seen is reported, not stored",
			line:        `{"step_index":20,"source":"MODEL","type":"SUBAGENT_RESULT","status":"DONE","created_at":"2026-11-24T08:10:18Z","content":"x"}`,
			wantUnknown: "SUBAGENT_RESULT",
		},
		{
			name:        "a line with no type is reported too",
			line:        `{"step_index":21,"created_at":"2026-11-24T08:10:18Z"}`,
			wantUnknown: "(no type)",
		},
		{name: "a broken line is an error, not a panic", line: `{"step_index":`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := bound(t).ParseLine([]byte(c.line))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if res.Unknown != c.wantUnknown || res.Skip != (c.wantUnknown != "") {
				t.Fatalf("Unknown = %q, Skip = %v; want unknown %q", res.Unknown, res.Skip, c.wantUnknown)
			}
			if res.Session.NativeID != "conv-1" {
				t.Errorf("NativeID = %q, want the folder name conv-1", res.Session.NativeID)
			}
			if res.Session.Model != c.wantModel {
				t.Errorf("Model = %q, want %q", res.Session.Model, c.wantModel)
			}
			if c.wantRole == "" {
				if res.Message != nil {
					t.Fatalf("got a message %+v, want none", res.Message)
				}
				return
			}
			if res.Message == nil {
				t.Fatal("got no message")
			}
			m := res.Message
			if m.Role != c.wantRole || m.Kind != c.wantKind || m.Content != c.wantContent {
				t.Errorf("message = %s/%s %q, want %s/%s %q", m.Role, m.Kind, m.Content, c.wantRole, c.wantKind, c.wantContent)
			}
			if len(res.More) != c.wantMore {
				t.Errorf("More has %d messages, want %d", len(res.More), c.wantMore)
			}
			if m.CreatedAt == "" || len(m.Raw) == 0 {
				t.Errorf("message lacks created_at or raw: %+v", m)
			}
		})
	}
}

// Tool calls carry no id in the log; results follow in the same order. The
// adapter must hand each result the id it gave the matching call, because
// that is how the driver names a result's tool.
func TestToolResultsPairWithCallsInOrder(t *testing.T) {
	a := bound(t)
	res, err := a.ParseLine([]byte(`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"t","tool_calls":[{"name":"read_file","args":{"Path":"C:/p/a.go"}},{"name":"write_file","args":{"Path":"C:\\p\\b.go","Content":"x"}},{"name":"run_command","args":{"CommandLine":"rm C:/p/c.go"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	calls := append([]string{res.Message.ToolUseID}, res.More[0].ToolUseID, res.More[1].ToolUseID)
	if calls[0] == "" || calls[0] == calls[1] || calls[1] == calls[2] {
		t.Fatalf("call ids must be set and distinct: %q", calls)
	}

	// File touches come from the structured Path only; a path inside a
	// shell command is not one.
	if got := res.Message.FileTouches; len(got) != 1 || got[0].Path != "C:/p/a.go" || got[0].Action != "read" {
		t.Errorf("read_file touches = %+v", got)
	}
	if got := res.More[0].FileTouches; len(got) != 1 || got[0].Path != `C:\p\b.go` || got[0].Action != "edit" {
		t.Errorf("write_file touches = %+v", got)
	}
	if got := res.More[1].FileTouches; got != nil {
		t.Errorf("run_command must record no touch, got %+v", got)
	}

	for i, want := range calls {
		r, err := a.ParseLine([]byte(`{"step_index":4,"type":"GENERIC","status":"DONE","created_at":"t","content":"ok"}`))
		if err != nil {
			t.Fatal(err)
		}
		if r.Message.ToolUseID != want {
			t.Errorf("result %d paired with %q, want %q", i, r.Message.ToolUseID, want)
		}
	}
	// A result nobody is waiting for is kept, unnamed, not mis-paired.
	r, _ := a.ParseLine([]byte(`{"step_index":9,"type":"GENERIC","status":"DONE","created_at":"t","content":"stray"}`))
	if r.Message == nil || r.Message.ToolUseID != "" {
		t.Errorf("stray result = %+v, want a message with no id", r.Message)
	}
}

// A call whose result never arrives must not shift the next step's pairing.
func TestUnansweredCallDoesNotShiftLaterPairs(t *testing.T) {
	a := bound(t)
	a.ParseLine([]byte(`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"t","tool_calls":[{"name":"run_command","args":{}}]}`))
	res, _ := a.ParseLine([]byte(`{"step_index":2,"type":"PLANNER_RESPONSE","created_at":"t","tool_calls":[{"name":"read_file","args":{"Path":"a"}}]}`))
	r, _ := a.ParseLine([]byte(`{"step_index":3,"type":"GENERIC","created_at":"t","content":"x"}`))
	if r.Message.ToolUseID != res.Message.ToolUseID {
		t.Errorf("result paired with %q, want the latest step's call %q", r.Message.ToolUseID, res.Message.ToolUseID)
	}
}

// A transcript outside brain/<id>/... has no conversation id. Its lines are
// refused with a reason, which the driver records against the file.
func TestParseLineRefusesATranscriptItCannotPlace(t *testing.T) {
	for _, path := range []string{
		"", // never bound
		filepath.Join(t.TempDir(), TranscriptName),
		filepath.Join(t.TempDir(), "notbrain", "id", ".system_generated", "logs", TranscriptName),
	} {
		a := New()
		if path != "" {
			a.BindFile(path)
		}
		_, err := a.ParseLine([]byte(`{"step_index":0,"type":"USER_INPUT","content":"hi"}`))
		if err == nil || !strings.Contains(err.Error(), "which conversation") {
			t.Errorf("path %q: err = %v, want one explaining the conversation is unknown", path, err)
		}
	}
}

func TestWantsFileAndSkipDir(t *testing.T) {
	a := New()
	conv := filepath.Join("data", "brain", "id")
	files := []struct {
		path string
		want bool
	}{
		{filepath.Join(conv, ".system_generated", "logs", "transcript_full.jsonl"), true},
		{filepath.Join(conv, ".system_generated", "logs", "transcript.jsonl"), false},
		{filepath.Join(conv, ".system_generated", "logs", "chunks", "transcript_full", "00000000.jsonl"), false},
		{filepath.Join(conv, "scratch", "2", "transcript_full.jsonl"), false},
		{filepath.Join(conv, ".user_uploaded", "data.jsonl"), false},
		{filepath.Join("data", "history.jsonl"), false},
	}
	for _, f := range files {
		if got := a.WantsFile(f.path); got != f.want {
			t.Errorf("WantsFile(%s) = %v, want %v", f.path, got, f.want)
		}
	}
	dirs := []struct {
		path string
		want bool
	}{
		{conv, false},
		{filepath.Join(conv, ".system_generated"), false},
		{filepath.Join(conv, ".system_generated", "logs"), false},
		{filepath.Join(conv, ".system_generated", "logs", "chunks"), true},
		{filepath.Join(conv, ".system_generated", "steps"), true},
		{filepath.Join(conv, "scratch"), true},
		{filepath.Join(conv, ".user_uploaded"), true},
	}
	for _, d := range dirs {
		if got := a.SkipDir(d.path); got != d.want {
			t.Errorf("SkipDir(%s) = %v, want %v", d.path, got, d.want)
		}
	}
}

// Every line of the real (redacted) transcripts must parse, and none may be
// reported as an unknown type: these are the shapes the adapter was written
// from.
func TestFixturesParseCleanly(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "antigravity-cli", "brain", "*", ".system_generated", "logs", TranscriptName))
	if err != nil || len(paths) != 2 {
		t.Fatalf("fixtures: %v, %v", paths, err)
	}
	tools := map[string]bool{}
	for _, path := range paths {
		a := New()
		a.BindFile(path)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for n := 1; sc.Scan(); n++ {
			res, err := a.ParseLine(sc.Bytes())
			if err != nil || res.Unknown != "" {
				t.Errorf("%s line %d: err %v, unknown %q", path, n, err, res.Unknown)
				continue
			}
			if res.Message == nil {
				continue
			}
			for _, m := range append([]ingest.RawMessage{*res.Message}, res.More...) {
				if m.Kind == "tool_call" {
					tools[m.ToolName] = true
				}
			}
		}
		f.Close()
	}
	// The fixtures between them show every tool the adapter knows.
	for _, name := range []string{"run_command", "read_file", "write_file", "edit_file"} {
		if !tools[name] {
			t.Errorf("no fixture line calls %s", name)
		}
	}
}

func TestUserRequest(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"wrapped, with appended blocks", "<USER_REQUEST>\nfix the bug\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nx\n</ADDITIONAL_METADATA>", "fix the bug"},
		{"several lines are kept", "<USER_REQUEST>\nline one\n\nline two\n</USER_REQUEST>", "line one\n\nline two"},
		{"a prompt quoting the closing tag is not cut short", "<USER_REQUEST>\nwhat does </USER_REQUEST> mean?\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nx\n</ADDITIONAL_METADATA>", "what does </USER_REQUEST> mean?"},
		{"no wrapper is returned as written", "  plain prompt\n", "plain prompt"},
		{"an unclosed wrapper keeps what follows", "<USER_REQUEST>\nhalf", "half"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := userRequest(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestToolOutput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"doubled header", "Created At: a\nCompleted At: b\nCreated At: a\nCompleted At: b\nCreated file.", "Created file."},
		{"single header", "Created At: a\nCompleted At: b\nout", "out"},
		{"no header", "plain output\nmore", "plain output\nmore"},
		{"the same words further down are output, not header", "Created At: a\nresult\nCreated At: z", "result\nCreated At: z"},
		{"header only", "Created At: a\nCompleted At: b", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := toolOutput(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPathFromFileURI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file:///C:/Users/dev/proj", `C:\Users\dev\proj`},
		{"file:///c:/Users/dev/my%20proj", `c:\Users\dev\my proj`},
		{"file:///home/dev/proj", "/home/dev/proj"},
		{"file:///home/dev/my%20proj", "/home/dev/my proj"},
		{"https://example.com/x", ""},
		{"not a uri", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := pathFromFileURI(c.in); got != c.want {
			t.Errorf("pathFromFileURI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// summariesSchema is the table as Antigravity CLI 1.5.16 creates it, copied
// from a real conversation_summaries.db.
const summariesSchema = "CREATE TABLE `conversation_summaries` (`conversation_id` text,`title` text NOT NULL DEFAULT \"\",`preview` text NOT NULL DEFAULT \"\",`step_count` integer NOT NULL DEFAULT 0,`last_modified_time` datetime NOT NULL,`workspace_uris` text NOT NULL,`status` text NOT NULL DEFAULT \"\",`source` text NOT NULL DEFAULT \"\",`project_id` text NOT NULL DEFAULT \"\",`agent_name` text NOT NULL DEFAULT \"\",`parent_conversation_id` text NOT NULL DEFAULT \"\",`nesting_depth` integer NOT NULL DEFAULT 0,`battle_id` text NOT NULL DEFAULT \"\",`winning_conversation_id` text NOT NULL DEFAULT \"\",`not_fully_idle` numeric NOT NULL DEFAULT false,`killed` numeric NOT NULL DEFAULT false,`last_user_input_time` datetime NOT NULL,`last_user_input_step_index` integer NOT NULL DEFAULT -1,`app_data_dir` text NOT NULL DEFAULT \"\",`raw_summary` blob,`group_id` text NOT NULL DEFAULT \"\",PRIMARY KEY (`conversation_id`))"

func writeSummaries(t *testing.T, dataDir string, rows [][3]string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(summariesSchema); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec("INSERT INTO conversation_summaries(conversation_id, workspace_uris, title, last_modified_time, last_user_input_time) VALUES (?,?,?,'2026-11-24 08:08:09+00:00','2026-11-24 08:08:09+00:00')", r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLookupConversation(t *testing.T) {
	// A folder name with a space and a "#": both would break a database
	// path that was not escaped.
	data := filepath.Join(t.TempDir(), "anti gravity #1")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSummaries(t, data, [][3]string{
		{"in-db", `["file:///home/dev/proj"]`, "Explore Repository"},
		{"two-folders", `["file:///home/dev/first","file:///home/dev/second"]`, ""},
		{"no-folder", `[]`, "Titled"},
		{"bad-json", `not json`, "Still titled"},
		{"db-wins", `["file:///home/dev/from-db"]`, ""},
	})
	history := `{"display":"first prompt","timestamp":1,"workspace":"/home/dev/elsewhere"}` + "\n" +
		`not json at all` + "\n" +
		`{"display":"second","timestamp":2,"workspace":"/home/dev/from-history","conversationId":"no-folder"}` + "\n" +
		`{"display":"x","timestamp":3,"workspace":"/home/dev/from-history","conversationId":"db-wins"}` + "\n" +
		`{"display":"y","timestamp":4,"workspace":"/home/dev/only-history","conversationId":"history-only"}` + "\n"
	if err := os.WriteFile(filepath.Join(data, "history.jsonl"), []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ id, wantCWD, wantTitle string }{
		{"in-db", "/home/dev/proj", "Explore Repository"},
		{"two-folders", "/home/dev/first", ""},
		{"no-folder", "/home/dev/from-history", "Titled"},
		{"bad-json", "", "Still titled"},
		{"db-wins", "/home/dev/from-db", ""},
		{"history-only", "/home/dev/only-history", ""},
		{"nowhere", "", ""},
	}
	for _, c := range cases {
		cwd, title := lookupConversation(data, c.id)
		if cwd != c.wantCWD || title != c.wantTitle {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.id, cwd, title, c.wantCWD, c.wantTitle)
		}
	}
}

// With neither file present the lookup finds nothing, quietly, and must not
// leave an empty database behind in Antigravity's folder.
func TestLookupConversationWithNoFiles(t *testing.T) {
	data := t.TempDir()
	if cwd, title := lookupConversation(data, "any"); cwd != "" || title != "" {
		t.Errorf("got (%q, %q), want nothing", cwd, title)
	}
	if entries, _ := os.ReadDir(data); len(entries) != 0 {
		t.Errorf("lookup created files in the data folder: %v", entries)
	}
}

// A database from a version that renamed the table must not fail the parse.
func TestLookupConversationSurvivesAnUnknownSchema(t *testing.T) {
	data := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(data, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE something_else (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if cwd, title := lookupConversation(data, "any"); cwd != "" || title != "" {
		t.Errorf("got (%q, %q), want nothing", cwd, title)
	}
}
