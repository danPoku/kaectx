// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/agentcfg"
)

// fakeMachine stands in for the commands setup runs: `claude mcp add`, which
// it applies to .claude.json the way the real one does, and systemctl,
// launchctl and reg, which it only records. It lets one test run setup twice
// and see the second run find the first run's work.
type fakeMachine struct {
	home     string
	noClaude bool
	calls    []string
	runKey   string // what `reg query` should print once the value is set
}

func (m *fakeMachine) LookPath(name string) (string, error) {
	if name == "claude" && m.noClaude {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}

func (m *fakeMachine) Start(name string, args ...string) error {
	m.calls = append(m.calls, "start "+name+" "+strings.Join(args, " "))
	return nil
}

func (m *fakeMachine) Run(name string, args ...string) (string, error) {
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "claude":
		// mcp add --scope user kae -- COMMAND mcp
		if len(args) >= 8 && args[1] == "add" {
			path := agentcfg.ClaudeConfigPath(m.home)
			doc := map[string]any{}
			if data, err := os.ReadFile(path); err == nil {
				_ = json.Unmarshal(data, &doc)
			}
			servers, _ := doc["mcpServers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			servers[args[4]] = map[string]any{"type": "stdio", "command": args[6], "args": args[7:]}
			doc["mcpServers"] = servers
			out, _ := json.Marshal(doc)
			return "", os.WriteFile(path, out, 0600)
		}
	case "reg":
		if args[0] == "query" {
			if m.runKey == "" {
				return "", errors.New("value not found")
			}
			return m.runKey, nil
		}
		if args[0] == "add" {
			m.runKey = strings.Join(args, " ")
		}
	}
	return "", nil
}

func (m *fakeMachine) ran(prefix string) bool {
	for _, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// A Codex config with content setup must not disturb.
const existingCodexConfig = `# my settings
model = "gpt-5"   # keep this comment

[mcp_servers.other]
command = "npx"
args = ["other-server"]

[desktop]
conversationDetailMode = "STEPS_COMMANDS"
`

func newSetupMachine(t *testing.T) (string, *fakeMachine) {
	t.Helper()
	home := useHome(t)
	for _, dir := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(agentcfg.CodexConfigPath(home), []byte(existingCodexConfig), 0600); err != nil {
		t.Fatal(err)
	}
	return home, &fakeMachine{home: home}
}

func runSetupWith(t *testing.T, o setupOptions) string {
	t.Helper()
	var out bytes.Buffer
	o.Out = &out
	if o.In == nil {
		o.In = strings.NewReader("")
	}
	if o.Ingest == nil {
		o.Ingest = func() (string, error) { return "ingested 0 session log file(s), 0 failed", nil }
	}
	if err := setup(o); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestSetupConnectsBothAgentsAndIsRepeatable(t *testing.T) {
	home, m := newSetupMachine(t)
	exe := filepath.Join(home, "bin", "kae")
	opts := setupOptions{Home: home, Exe: exe, GOOS: "linux", Yes: true, Run: m}

	ingested := 0
	opts.Ingest = func() (string, error) { ingested++; return "ingested 3 session log file(s), 0 failed", nil }

	out := runSetupWith(t, opts)
	for _, want := range []string{
		`claude-code: registered MCP server "kae"`,
		"codex: added [mcp_servers.kae]",
		"ingested 3 session log file(s)",
		"daemon: installed and started the systemd user service",
		"start a new chat",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("first run output lacks %q:\n%s", want, out)
		}
	}

	// Both agents can now start the server: Claude Code has the entry, and
	// Codex's table names the same binary.
	if !m.ran("claude mcp add --scope user kae -- " + exe + " mcp") {
		t.Errorf("claude was not asked to register kae: %v", m.calls)
	}
	codex, err := os.ReadFile(agentcfg.CodexConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	wantCodex := existingCodexConfig + "\n[mcp_servers.kae]\ncommand = '" + exe + "'\nargs = [\"mcp\"]\ndefault_tools_approval_mode = \"approve\"\n"
	if string(codex) != wantCodex {
		t.Errorf("config.toml must keep its other tables and comments and gain one table:\n%s\nwant:\n%s", codex, wantCodex)
	}
	unit, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "kae-daemon.service"))
	if err != nil || !strings.Contains(string(unit), `ExecStart="`+exe+`" daemon`) {
		t.Errorf("systemd unit missing or wrong: %v\n%s", err, unit)
	}
	if !m.ran("systemctl --user enable kae-daemon.service") {
		t.Errorf("service not enabled: %v", m.calls)
	}

	// A second run changes nothing and says so.
	m.calls = nil
	out = runSetupWith(t, opts)
	if !strings.Contains(out, "nothing to change: kae is already set up") {
		t.Errorf("second run should say nothing changed:\n%s", out)
	}
	for _, c := range m.calls {
		if strings.HasPrefix(c, "claude mcp add") || strings.HasPrefix(c, "systemctl") {
			t.Errorf("second run made a change: %s", c)
		}
	}
	again, _ := os.ReadFile(agentcfg.CodexConfigPath(home))
	if string(again) != wantCodex {
		t.Errorf("second run rewrote config.toml:\n%s", again)
	}
	if ingested != 2 {
		t.Errorf("ingest should run on every setup, ran %d times", ingested)
	}
}

func TestSetupDryRunWritesNothing(t *testing.T) {
	home, m := newSetupMachine(t)
	opts := setupOptions{Home: home, Exe: "/opt/kae", GOOS: "linux", DryRun: true, Yes: true, Run: m}
	opts.Ingest = func() (string, error) { t.Fatal("a dry run must not ingest"); return "", nil }

	out := runSetupWith(t, opts)
	for _, want := range []string{
		`claude-code: would have registered MCP server "kae"`,
		"codex: would have added [mcp_servers.kae]",
		"would ask: Codex",
		"would ask: Run the daemon",
		"dry run: nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("dry run ran commands: %v", m.calls)
	}
	codex, _ := os.ReadFile(agentcfg.CodexConfigPath(home))
	if string(codex) != existingCodexConfig {
		t.Error("dry run changed config.toml")
	}
	if _, err := os.Stat(agentcfg.ClaudeConfigPath(home)); err == nil {
		t.Error("dry run wrote .claude.json")
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil {
		t.Error("dry run wrote a service file")
	}
}

// Without --yes, the two optional changes are asked about, and silence (a
// closed stdin, as in a script) means no.
func TestSetupAsksBeforeOptionalChanges(t *testing.T) {
	cases := []struct {
		name, input          string
		wantApprove, wantSvc bool
	}{
		{"no answers", "", false, false},
		{"yes to approval, no to the daemon", "y\nn\n", true, false},
		{"no to approval, yes to the daemon", "n\nyes\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, m := newSetupMachine(t)
			runSetupWith(t, setupOptions{Home: home, Exe: "/opt/kae", GOOS: "linux", Run: m, In: strings.NewReader(c.input)})

			codex, _ := os.ReadFile(agentcfg.CodexConfigPath(home))
			if !strings.Contains(string(codex), "[mcp_servers.kae]") {
				t.Fatalf("the registration itself is not optional:\n%s", codex)
			}
			if got := strings.Contains(string(codex), "default_tools_approval_mode"); got != c.wantApprove {
				t.Errorf("approval mode set = %v, want %v", got, c.wantApprove)
			}
			if got := m.ran("systemctl --user enable"); got != c.wantSvc {
				t.Errorf("service installed = %v, want %v", got, c.wantSvc)
			}
		})
	}
}

func TestSetupNoDaemonAndNoAgents(t *testing.T) {
	home := useHome(t)
	m := &fakeMachine{home: home}
	out := runSetupWith(t, setupOptions{Home: home, Exe: "/opt/kae", GOOS: "linux", Yes: true, NoDaemon: true, Run: m})
	if !strings.Contains(out, "no supported agent found") || !strings.Contains(out, "nothing to change") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if len(m.calls) != 0 {
		t.Errorf("--no-daemon with no agents should run nothing: %v", m.calls)
	}
}

// A machine that registered the server as `ctx` ends with kae only, and the
// old entry is not listed twice when there is no claude command to move it.
func TestSetupMovesAnOldCtxRegistration(t *testing.T) {
	home, m := newSetupMachine(t)
	m.noClaude = true
	old := `{"mcpServers": {"ctx": {"type": "stdio", "command": "/opt/ctx", "args": ["mcp"]}}}`
	if err := os.WriteFile(agentcfg.ClaudeConfigPath(home), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentcfg.CodexConfigPath(home), []byte("[mcp_servers.ctx]\ncommand = \"/opt/ctx\"\nargs = [\"mcp\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}

	out := runSetupWith(t, setupOptions{Home: home, Exe: "/opt/kae", GOOS: "linux", NoDaemon: true, Run: m})
	if n := strings.Count(out, "claude mcp add"); n != 1 {
		t.Errorf("the command to run by hand should appear once, appeared %d times:\n%s", n, out)
	}
	codex, _ := os.ReadFile(agentcfg.CodexConfigPath(home))
	if want := "[mcp_servers.kae]\ncommand = \"/opt/kae\"\nargs = [\"mcp\"]\n"; string(codex) != want {
		t.Errorf("codex config:\n%s\nwant:\n%s", codex, want)
	}
}

func TestSetupInstallsTheServiceForEachSystem(t *testing.T) {
	cases := []struct {
		goos, wantCall, wantFile string
	}{
		{"darwin", "launchctl bootstrap gui/501 ", filepath.Join("Library", "LaunchAgents", "io.github.danpoku.kae.daemon.plist")},
		{"windows", `reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v kae-daemon`, ""},
	}
	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			home := useHome(t)
			m := &fakeMachine{home: home}
			opts := setupOptions{Home: home, Exe: filepath.Join(home, "kae"), GOOS: c.goos, UID: 501, Yes: true, Run: m}
			runSetupWith(t, opts)
			if !m.ran(c.wantCall) {
				t.Fatalf("want a call starting %q, got %v", c.wantCall, m.calls)
			}
			if c.wantFile != "" {
				data, err := os.ReadFile(filepath.Join(home, c.wantFile))
				if err != nil || !strings.Contains(string(data), opts.Exe) {
					t.Fatalf("service file missing or does not name the binary: %v", err)
				}
			}
			if c.goos == "windows" && !m.ran("start conhost.exe --headless") {
				t.Fatalf("the daemon was not started: %v", m.calls)
			}

			m.calls = nil
			out := runSetupWith(t, opts)
			if !strings.Contains(out, "daemon: already installed") || m.ran("launchctl bootstrap") || m.ran("reg add") {
				t.Fatalf("second run reinstalled the service: %v\n%s", m.calls, out)
			}
		})
	}
}
