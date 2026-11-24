// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// existingSettings has the shape of a real ~/.claude/settings.json: keys in
// an order of the user's choosing, and hooks for other events already set.
const existingSettings = `{
  "env": {"FOO": "1"},
  "permissions": {"allow": ["Bash(go test:*)"]},
  "model": "opus",
  "hooks": {
    "PreToolUse": [{"matcher": "Skill|Agent", "hooks": [{"type": "command", "command": "python gate.py", "timeout": 10}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "bash stop.sh", "timeout": 5}]}]
  },
  "effortLevel": "high"
}
`

func writeSettings(t *testing.T, home, body string) string {
	t.Helper()
	path := ClaudeSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// topLevelKeys returns a JSON object's keys in file order.
func topLevelKeys(t *testing.T, data []byte) []string {
	t.Helper()
	members, err := parseObject(data)
	if err != nil {
		t.Fatalf("result is not a JSON object: %v\n%s", err, data)
	}
	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = m.key
	}
	return keys
}

func TestEnsureClaudeBriefHookKeepsEverythingElse(t *testing.T) {
	home := t.TempDir()
	path := writeSettings(t, home, existingSettings)
	// A path with a space in it, in the form this system writes paths. On
	// Windows that means backslashes, which must reach the hook as forward
	// slashes; elsewhere a backslash is an ordinary character and the path
	// is used as it is.
	exe, wantCommand := "/Users/a b/bin/kae", `"/Users/a b/bin/kae" brief --hook`
	if runtime.GOOS == "windows" {
		exe, wantCommand = `C:\Users\a b\go\bin\kae.exe`, `"C:/Users/a b/go/bin/kae.exe" brief --hook`
	}

	changes, err := EnsureClaudeBriefHook(home, exe, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("want one change, got %+v", changes)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Key order is the user's, not alphabetical.
	if keys := strings.Join(topLevelKeys(t, got), ","); keys != "env,permissions,model,hooks,effortLevel" {
		t.Errorf("top-level keys reordered: %s", keys)
	}
	var doc struct {
		Env         map[string]string
		Model       string
		EffortLevel string
		Hooks       map[string][]hookGroup
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Env["FOO"] != "1" || doc.Model != "opus" || doc.EffortLevel != "high" {
		t.Errorf("other settings changed: %+v", doc)
	}
	if len(doc.Hooks["PreToolUse"]) != 1 || doc.Hooks["PreToolUse"][0].Matcher != "Skill|Agent" || len(doc.Hooks["Stop"]) != 1 {
		t.Errorf("existing hooks changed: %+v", doc.Hooks)
	}
	start := doc.Hooks["SessionStart"]
	if len(start) != 1 || len(start[0].Hooks) != 1 {
		t.Fatalf("SessionStart hook not added: %+v", start)
	}
	// Quoted, so the command survives a path with a space in it.
	if start[0].Hooks[0].Command != wantCommand || start[0].Hooks[0].Type != "command" {
		t.Errorf("hook command = %+v, want %s", start[0].Hooks[0], wantCommand)
	}
	if backup, err := os.ReadFile(path + ".kae-backup"); err != nil || string(backup) != existingSettings {
		t.Errorf("backup missing or not the original: %v", err)
	}

	// Installed once: a second call changes nothing, even with another path.
	if !HasClaudeBriefHook(home) {
		t.Fatal("HasClaudeBriefHook is false after install")
	}
	changes, err = EnsureClaudeBriefHook(home, "/somewhere/else/kae", false)
	again, _ := os.ReadFile(path)
	if err != nil || len(changes) != 0 || string(again) != string(got) {
		t.Fatalf("second call changed something: %+v, %v", changes, err)
	}
}

func TestEnsureClaudeBriefHookEdgeCases(t *testing.T) {
	t.Run("no settings file yet", func(t *testing.T) {
		home := t.TempDir()
		if _, err := EnsureClaudeBriefHook(home, "/opt/kae", false); err != nil {
			t.Fatal(err)
		}
		if !HasClaudeBriefHook(home) {
			t.Fatal("hook not installed into a new settings file")
		}
		if _, err := os.Stat(ClaudeSettingsPath(home) + ".kae-backup"); err == nil {
			t.Fatal("a backup of a file that did not exist was written")
		}
	})
	t.Run("an existing SessionStart hook is kept", func(t *testing.T) {
		home := t.TempDir()
		path := writeSettings(t, home, `{"hooks": {"SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "echo hi"}]}]}}`)
		if _, err := EnsureClaudeBriefHook(home, "/opt/kae", false); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !strings.Contains(string(got), "echo hi") || !strings.Contains(string(got), `\"/opt/kae\" brief --hook`) {
			t.Fatalf("want both hooks:\n%s", got)
		}
	})
	t.Run("dry run writes nothing", func(t *testing.T) {
		home := t.TempDir()
		path := writeSettings(t, home, existingSettings)
		changes, err := EnsureClaudeBriefHook(home, "/opt/kae", true)
		got, _ := os.ReadFile(path)
		if err != nil || len(changes) != 1 || string(got) != existingSettings {
			t.Fatalf("dry run: %+v, %v", changes, err)
		}
	})
	t.Run("a settings file that is not JSON is refused, not overwritten", func(t *testing.T) {
		home := t.TempDir()
		path := writeSettings(t, home, `{"model": "opus",`)
		if _, err := EnsureClaudeBriefHook(home, "/opt/kae", false); err == nil {
			t.Fatal("want an error for a broken settings file")
		}
		got, _ := os.ReadFile(path)
		if string(got) != `{"model": "opus",` {
			t.Fatal("the broken file was modified")
		}
	})
}

func TestEnsureCodexBriefInstruction(t *testing.T) {
	for _, existing := range []string{"", "# My rules\n\nAlways run the tests.\n", "# My rules\r\n\r\nAlways run the tests.\r\n"} {
		home := t.TempDir()
		path := CodexInstructionsPath(home)
		if existing != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
				t.Fatal(err)
			}
		}
		changes, err := EnsureCodexBriefInstruction(home, false)
		if err != nil || len(changes) != 1 {
			t.Fatalf("got %+v, %v", changes, err)
		}
		got, _ := os.ReadFile(path)
		text := string(got)
		if !strings.HasPrefix(text, existing[:len(strings.TrimRight(existing, "\r\n"))]) {
			t.Errorf("existing instructions were changed:\n%q", text)
		}
		if !strings.Contains(text, "`get_brief`") || !strings.Contains(text, codexBriefStart) || !strings.Contains(text, codexBriefEnd) {
			t.Errorf("instruction block missing:\n%s", text)
		}
		if strings.Contains(existing, "\r\n") && strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
			t.Errorf("a CRLF file gained bare LF lines:\n%q", text)
		}

		changes, err = EnsureCodexBriefInstruction(home, false)
		again, _ := os.ReadFile(path)
		if err != nil || len(changes) != 0 || string(again) != text {
			t.Fatalf("second call changed something: %+v, %v", changes, err)
		}
	}
}
