// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package agentcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenameCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ctx", "kae"},
		{"ctx.exe", "kae.exe"},
		{"/home/a/go/bin/ctx", "/home/a/go/bin/kae"},
		{`C:\Users\a\go\bin\ctx.exe`, `C:\Users\a\go\bin\kae.exe`},
		{`C:\\Users\\a\\go\\bin\\ctx.exe`, `C:\\Users\\a\\go\\bin\\kae.exe`},
		// Not the old binary: a folder named ctx, or another tool entirely.
		{"/opt/ctx/server", "/opt/ctx/server"},
		{"ctxd", "ctxd"},
		{"npx", "npx"},
	}
	for _, c := range cases {
		if got := RenameCommand(c.in); got != c.want {
			t.Errorf("RenameCommand(%q) = %q, want %q", c.in, got, c.want)
		}
		if got, want := IsLegacyCommand(c.in), c.in != c.want; got != want {
			t.Errorf("IsLegacyCommand(%q) = %v, want %v", c.in, got, want)
		}
	}
}

// fakeClaude stands in for the `claude` command. It applies `mcp add` and
// `mcp remove` to the user-scope mcpServers in home's .claude.json, which is
// what the real command does, so a second migration run sees the result of
// the first.
type fakeClaude struct {
	home    string
	missing bool
	calls   [][]string
}

func (f *fakeClaude) LookPath(name string) (string, error) {
	if f.missing {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}

func (f *fakeClaude) Start(name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func (f *fakeClaude) Run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	path := ClaudeConfigPath(f.home)
	var doc map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	switch {
	case len(args) > 1 && args[1] == "add":
		// mcp add --scope user NAME [-e K=V]... -- COMMAND ARGS...
		entry := map[string]any{"type": "stdio"}
		env := map[string]any{}
		name := args[4]
		rest := args[5:]
		for len(rest) > 0 && rest[0] == "-e" {
			k, v, _ := strings.Cut(rest[1], "=")
			env[k] = v
			rest = rest[2:]
		}
		entry["command"] = rest[1]
		entry["args"] = rest[2:]
		entry["env"] = env
		servers[name] = entry
	case len(args) > 1 && args[1] == "remove":
		delete(servers, args[4])
	}
	out, _ := json.Marshal(doc)
	return "", os.WriteFile(path, out, 0600)
}

func writeClaudeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(ClaudeConfigPath(home), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// The shape here is Claude Code 2.1.x's: user-scope servers at the top
// level, other keys the migration must ignore, and an env on the entry.
const claudeWithCtx = `{
  "numStartups": 12,
  "mcpServers": {
    "sentry": {"type": "http", "url": "https://example.invalid/mcp"},
    "ctx": {"type": "stdio", "command": "/home/a/go/bin/ctx", "args": ["mcp"], "env": {"OLLAMA_HOST": "localhost"}}
  },
  "projects": {"/home/a/app": {"mcpServers": {}}}
}`

func TestMigrateClaude(t *testing.T) {
	home := t.TempDir()
	writeClaudeConfig(t, home, claudeWithCtx)
	run := &fakeClaude{home: home}

	changes, err := MigrateClaude(home, run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Manual || changes[1].Manual {
		t.Fatalf("want an add and a remove, got %+v", changes)
	}
	wantCalls := [][]string{
		{"claude", "mcp", "add", "--scope", "user", "kae", "-e", "OLLAMA_HOST=localhost", "--", "/home/a/go/bin/kae", "mcp"},
		{"claude", "mcp", "remove", "--scope", "user", "ctx"},
	}
	if !reflect.DeepEqual(run.calls, wantCalls) {
		t.Fatalf("calls:\n got %v\nwant %v", run.calls, wantCalls)
	}

	cfg, _, err := readClaudeConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.MCPServers["ctx"]; ok {
		t.Fatal("ctx entry still registered")
	}
	if got := cfg.MCPServers["kae"]; got.Command != "/home/a/go/bin/kae" || !reflect.DeepEqual(got.Args, []string{"mcp"}) {
		t.Fatalf("kae entry is not a working registration: %+v", got)
	}
	if _, ok := cfg.MCPServers["sentry"]; !ok {
		t.Fatal("an unrelated server was lost")
	}

	// Running it twice changes nothing.
	run.calls = nil
	changes, err = MigrateClaude(home, run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || len(run.calls) != 0 {
		t.Fatalf("second run should do nothing, got %+v and calls %v", changes, run.calls)
	}
}

func TestMigrateClaudeDryRunAndMissingCLI(t *testing.T) {
	home := t.TempDir()
	writeClaudeConfig(t, home, claudeWithCtx)

	run := &fakeClaude{home: home}
	changes, err := MigrateClaude(home, run, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || len(run.calls) != 0 {
		t.Fatalf("dry run should report two changes and run nothing, got %+v, calls %v", changes, run.calls)
	}

	run = &fakeClaude{home: home, missing: true}
	changes, err = MigrateClaude(home, run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Manual || !strings.Contains(changes[0].Detail, "claude mcp add --scope user kae") {
		t.Fatalf("want the commands to run by hand, got %+v", changes)
	}
}

func TestMigrateClaudeLeavesAnotherToolsCtxAlone(t *testing.T) {
	home := t.TempDir()
	writeClaudeConfig(t, home, `{"mcpServers": {"ctx": {"type": "stdio", "command": "npx", "args": ["some-other-ctx"]}}}`)
	run := &fakeClaude{home: home}
	changes, err := MigrateClaude(home, run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 0 || len(changes) != 1 || !changes[0].Manual {
		t.Fatalf("a foreign ctx server must not be touched: %+v, calls %v", changes, run.calls)
	}
}

func TestMigrateClaudeNoConfig(t *testing.T) {
	changes, err := MigrateClaude(t.TempDir(), &fakeClaude{}, false)
	if err != nil || len(changes) != 0 {
		t.Fatalf("no .claude.json: got %+v, %v", changes, err)
	}
}

// codexConfig mirrors a real codex-cli 0.153 config.toml: comments, tables
// before and after, a server with a sub-table, and single-quoted Windows
// paths.
const codexConfig = `# personal settings
model = "gpt-5"

[mcp_servers.node_repl]
args = []
command = 'C:\Program Files\node_repl.exe'

[mcp_servers.node_repl.env]
NODE_REPL_TIMEOUT_MS = "1000"

[mcp_servers.ctx]
command = 'C:\Users\a\go\bin\ctx.exe' # installed by go install
args = ['mcp']
default_tools_approval_mode = "approve"

[mcp_servers.ctx.env]
OLLAMA_HOST = "localhost"

[desktop]
conversationDetailMode = "STEPS_COMMANDS"
`

func writeCodexConfig(t *testing.T, home, body string) string {
	t.Helper()
	path := CodexConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateCodex(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		home := t.TempDir()
		before := strings.ReplaceAll(codexConfig, "\n", eol)
		path := writeCodexConfig(t, home, before)

		changes, err := MigrateCodex(home, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 1 || changes[0].Manual {
			t.Fatalf("want one change, got %+v", changes)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Exactly three lines differ: two headers and the command.
		want := strings.NewReplacer(
			"[mcp_servers.ctx]", "[mcp_servers.kae]",
			"[mcp_servers.ctx.env]", "[mcp_servers.kae.env]",
			`bin\ctx.exe'`, `bin\kae.exe'`,
		).Replace(before)
		if string(got) != want {
			t.Fatalf("eol %q: config after migration:\n%s\nwant:\n%s", eol, got, want)
		}
		backup, err := os.ReadFile(path + ".kae-backup")
		if err != nil || string(backup) != before {
			t.Fatalf("backup missing or not the original: %v", err)
		}

		// Running it twice changes nothing.
		changes, err = MigrateCodex(home, false)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := os.ReadFile(path)
		if len(changes) != 0 || string(again) != want {
			t.Fatalf("second run changed something: %+v", changes)
		}
	}
}

func TestMigrateCodexEdgeCases(t *testing.T) {
	t.Run("dry run writes nothing", func(t *testing.T) {
		home := t.TempDir()
		path := writeCodexConfig(t, home, codexConfig)
		changes, err := MigrateCodex(home, true)
		if err != nil || len(changes) != 1 {
			t.Fatalf("got %+v, %v", changes, err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != codexConfig {
			t.Fatal("dry run changed the file")
		}
		if _, err := os.Stat(path + ".kae-backup"); err == nil {
			t.Fatal("dry run wrote a backup")
		}
	})

	t.Run("kae already present drops the old table", func(t *testing.T) {
		home := t.TempDir()
		body := "[mcp_servers.kae]\ncommand = \"kae\"\nargs = [\"mcp\"]\n\n[mcp_servers.ctx]\ncommand = \"ctx\"\nargs = [\"mcp\"]\n\n[desktop]\nx = 1\n"
		path := writeCodexConfig(t, home, body)
		if _, err := MigrateCodex(home, false); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		want := "[mcp_servers.kae]\ncommand = \"kae\"\nargs = [\"mcp\"]\n\n[desktop]\nx = 1\n"
		if string(got) != want {
			t.Fatalf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("another tool's ctx table is left alone", func(t *testing.T) {
		home := t.TempDir()
		body := "[mcp_servers.ctx]\ncommand = \"npx\"\nargs = [\"other\"]\n"
		path := writeCodexConfig(t, home, body)
		changes, err := MigrateCodex(home, false)
		if err != nil || len(changes) != 1 || !changes[0].Manual {
			t.Fatalf("got %+v, %v", changes, err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != body {
			t.Fatal("file changed")
		}
	})

	t.Run("no config file", func(t *testing.T) {
		changes, err := MigrateCodex(t.TempDir(), false)
		if err != nil || len(changes) != 0 {
			t.Fatalf("got %+v, %v", changes, err)
		}
	})
}
