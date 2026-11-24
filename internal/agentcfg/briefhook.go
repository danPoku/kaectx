// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package agentcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// This file gives an agent the session-start brief (`kae brief`).
//
// Claude Code runs a SessionStart hook and adds what the command prints to
// the new session's context, so the brief arrives without the agent doing
// anything. The hook lives in ~/.claude/settings.json, read from a real file:
//
//	"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "...", "timeout": 10}]}]}
//
// Codex (checked: codex-cli 0.147) has hooks of its own, including
// SessionStart, but a hook there must be reviewed and trusted inside Codex
// before it runs, a step kae cannot do or check. So Codex is asked through
// the instructions it already reads at the start of every session,
// ~/.codex/AGENTS.md, to call the get_brief tool itself.

// ClaudeSettingsPath is Claude Code's user settings file.
func ClaudeSettingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

// member is one key of a JSON object with its value kept as written.
type member struct {
	key   string
	value json.RawMessage
}

// parseObject reads a JSON object into its members in file order. A plain
// map would lose that order, and writing the file back would then shuffle
// every key the user has; with the order kept, a re-written settings file
// differs from the original only where kae changed it (and in indentation).
func parseObject(data []byte) ([]member, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{key: key, value: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	// Anything after the closing brace means this is not one object.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected content after the JSON object")
	}
	return out, nil
}

// marshalObject writes members back in order, indented by two spaces.
func marshalObject(members []member) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	for i, m := range members {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString("\n  ")
		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteString(": ")
		if err := json.Indent(&buf, m.value, "  ", "  "); err != nil {
			return nil, err
		}
	}
	if len(members) > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func get(members []member, key string) (json.RawMessage, int) {
	for i, m := range members {
		if m.key == key {
			return m.value, i
		}
	}
	return nil, -1
}

func set(members []member, key string, value json.RawMessage) []member {
	if _, i := get(members, key); i >= 0 {
		members[i].value = value
		return members
	}
	return append(members, member{key: key, value: value})
}

// hookGroup is one entry of a hook event's list.
type hookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// briefHookMarker is how an installed brief hook is recognised, whatever
// path it was installed with.
const briefHookMarker = "brief --hook"

// BriefHookCommand is the command line the hook runs. The path uses forward
// slashes and is quoted: Claude Code runs hooks through a shell, which on
// Windows may be Git Bash, where a backslash is an escape character.
func BriefHookCommand(exe string) string {
	return `"` + filepath.ToSlash(exe) + `" ` + briefHookMarker
}

func isBriefHook(command string) bool {
	return strings.Contains(command, briefHookMarker) && strings.Contains(strings.ToLower(command), ServerName)
}

// readSessionStart returns the settings file's members, its hooks object's
// members, and the SessionStart groups.
func readSessionStart(home string) (settings, hooks []member, groups []json.RawMessage, data []byte, err error) {
	data, err = os.ReadFile(ClaudeSettingsPath(home))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, nil, err
	}
	if settings, err = parseObject(data); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("parse %s: %w", ClaudeSettingsPath(home), err)
	}
	if raw, i := get(settings, "hooks"); i >= 0 {
		if hooks, err = parseObject(raw); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("parse hooks in %s: %w", ClaudeSettingsPath(home), err)
		}
	}
	if raw, i := get(hooks, "SessionStart"); i >= 0 {
		if err = json.Unmarshal(raw, &groups); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("parse hooks.SessionStart in %s: %w", ClaudeSettingsPath(home), err)
		}
	}
	return settings, hooks, groups, data, nil
}

// HasClaudeBriefHook reports whether the brief hook is installed.
func HasClaudeBriefHook(home string) bool {
	_, _, groups, _, err := readSessionStart(home)
	if err != nil {
		return false
	}
	for _, raw := range groups {
		var g hookGroup
		if json.Unmarshal(raw, &g) != nil {
			continue
		}
		for _, h := range g.Hooks {
			if isBriefHook(h.Command) {
				return true
			}
		}
	}
	return false
}

// EnsureClaudeBriefHook adds a SessionStart hook that runs `kae brief --hook`
// to Claude Code's settings, unless one is already there. Every other
// setting and hook is kept, in the order it was in, and a copy of the file
// as it was is left beside it.
//
// The hook has no matcher, so it runs however a session starts (new,
// resumed, cleared, compacted). A resumed session therefore gets the brief a
// second time; that costs a few hundred tokens and is preferred over a
// matcher that might, on some Claude Code version, match nothing.
func EnsureClaudeBriefHook(home, exe string, dryRun bool) ([]Change, error) {
	const agent = "claude-code"
	if HasClaudeBriefHook(home) {
		return nil, nil
	}
	settings, hooks, groups, data, err := readSessionStart(home)
	if err != nil {
		return nil, err
	}
	path := ClaudeSettingsPath(home)
	change := []Change{{Agent: agent,
		Detail: fmt.Sprintf("added a SessionStart hook to %s that gives each new session a brief of past work (%s)", path, BriefHookCommand(exe))}}
	if dryRun {
		return change, nil
	}

	group, err := json.Marshal(hookGroup{Hooks: []hookCommand{{Type: "command", Command: BriefHookCommand(exe), Timeout: 10}}})
	if err != nil {
		return nil, err
	}
	groups = append(groups, group)
	rawGroups, err := json.Marshal(groups)
	if err != nil {
		return nil, err
	}
	hooks = set(hooks, "SessionStart", rawGroups)
	rawHooks, err := marshalObject(hooks)
	if err != nil {
		return nil, err
	}
	settings = set(settings, "hooks", bytes.TrimSpace(rawHooks))
	out, err := marshalObject(settings)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := writeWithBackup(path, data, string(out)); err != nil {
		return nil, err
	}
	return change, nil
}

// CodexInstructionsPath is the file Codex reads as standing instructions.
func CodexInstructionsPath(home string) string {
	return filepath.Join(home, ".codex", "AGENTS.md")
}

const (
	codexBriefStart = "<!-- kae:brief:start -->"
	codexBriefEnd   = "<!-- kae:brief:end -->"
)

// codexBriefBlock is the instruction added to Codex's AGENTS.md. The markers
// let kae find its own block again, and let the user see what to delete.
const codexBriefBlock = codexBriefStart + `
## Past work in this project (kae)

At the start of a session, call the ` + "`get_brief`" + ` tool of the ` + "`kae`" + ` MCP server once,
before other work, and treat what it returns as background on what was done
here before. If the tool is not available, carry on without it.
` + codexBriefEnd + "\n"

// HasCodexBriefInstruction reports whether the instruction is installed.
func HasCodexBriefInstruction(home string) bool {
	data, err := os.ReadFile(CodexInstructionsPath(home))
	return err == nil && strings.Contains(string(data), codexBriefStart)
}

// EnsureCodexBriefInstruction appends the instruction to Codex's AGENTS.md,
// creating the file if needed, unless it is already there.
func EnsureCodexBriefInstruction(home string, dryRun bool) ([]Change, error) {
	const agent = "codex"
	if HasCodexBriefInstruction(home) {
		return nil, nil
	}
	path := CodexInstructionsPath(home)
	change := []Change{{Agent: agent,
		Detail: fmt.Sprintf("added an instruction to %s to call get_brief at the start of each session", path)}}
	if dryRun {
		return change, nil
	}
	data, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return nil, err
	}
	content := string(data)
	eol := "\n"
	if strings.Contains(content, "\r\n") {
		eol = "\r\n"
	}
	if content != "" {
		content = strings.TrimRight(content, "\r\n") + eol + eol
	}
	content += strings.ReplaceAll(codexBriefBlock, "\n", eol)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	var previous []byte
	if !missing {
		previous = data
	}
	if err := writeWithBackup(path, previous, content); err != nil {
		return nil, err
	}
	return change, nil
}
