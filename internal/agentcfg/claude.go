// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package agentcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ClaudeServer is one stdio MCP registration as Claude Code stores it.
type ClaudeServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// claudeConfig is the part of ~/.claude.json this package reads. Every other
// key is ignored, and the file is never written.
type claudeConfig struct {
	MCPServers map[string]ClaudeServer `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]ClaudeServer `json:"mcpServers"`
	} `json:"projects"`
}

// ClaudeConfigPath is where Claude Code keeps user-scope registrations.
func ClaudeConfigPath(home string) string {
	return filepath.Join(home, ".claude.json")
}

func readClaudeConfig(home string) (claudeConfig, bool, error) {
	var cfg claudeConfig
	data, err := os.ReadFile(ClaudeConfigPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, false, fmt.Errorf("parse %s: %w", ClaudeConfigPath(home), err)
	}
	return cfg, true, nil
}

// MigrateClaude replaces a user-scope `ctx` registration in Claude Code with
// a `kae` one that starts the renamed binary, keeping its environment.
//
// The new entry is added before the old one is removed, so a failure half
// way leaves the agent with a working server rather than none. Running it
// again after success finds no `ctx` entry and changes nothing.
func MigrateClaude(home string, run Runner, dryRun bool) ([]Change, error) {
	const agent = "claude-code"
	cfg, found, err := readClaudeConfig(home)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	var changes []Change

	// Local-scope registrations live under one project each. `claude mcp`
	// can only change the one for the directory it is run in, so these are
	// reported rather than guessed at.
	var local []string
	for path, p := range cfg.Projects {
		if s, ok := p.MCPServers[LegacyServerName]; ok && IsLegacyCommand(s.Command) && isServeArgs(s.Args) {
			local = append(local, path)
		}
	}
	sort.Strings(local)
	for _, path := range local {
		changes = append(changes, Change{Agent: agent, Manual: true,
			Detail: fmt.Sprintf("project %s has its own ctx registration; from that folder run: claude mcp remove ctx && claude mcp add kae -- kae mcp", path)})
	}

	old, hasOld := cfg.MCPServers[LegacyServerName]
	if !hasOld {
		return changes, nil
	}
	if !IsLegacyCommand(old.Command) || !isServeArgs(old.Args) {
		changes = append(changes, Change{Agent: agent, Manual: true,
			Detail: fmt.Sprintf("the server registered as %q runs %q, which is not kae's old binary; left as it is", LegacyServerName, old.Command)})
		return changes, nil
	}

	_, hasNew := cfg.MCPServers[ServerName]
	newCommand := RenameCommand(old.Command)
	add := []string{"mcp", "add", "--scope", "user", ServerName}
	keys := make([]string, 0, len(old.Env))
	for k := range old.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add = append(add, "-e", k+"="+old.Env[k])
	}
	add = append(add, "--", newCommand, "mcp")
	remove := []string{"mcp", "remove", "--scope", "user", LegacyServerName}

	if _, err := run.LookPath("claude"); err != nil {
		var cmds []string
		if !hasNew {
			cmds = append(cmds, "claude "+shellQuote(add))
		}
		cmds = append(cmds, "claude "+shellQuote(remove))
		changes = append(changes, Change{Agent: agent, Manual: true,
			Detail: "the claude command is not on PATH; run: " + strings.Join(cmds, " && ")})
		return changes, nil
	}

	if !hasNew {
		if !dryRun {
			if out, err := run.Run("claude", add...); err != nil {
				return changes, fmt.Errorf("claude mcp add: %w: %s", err, strings.TrimSpace(out))
			}
		}
		changes = append(changes, Change{Agent: agent,
			Detail: fmt.Sprintf("registered MCP server %q (%s mcp) at user scope", ServerName, newCommand)})
	}
	if !dryRun {
		if out, err := run.Run("claude", remove...); err != nil {
			return changes, fmt.Errorf("claude mcp remove: %w: %s", err, strings.TrimSpace(out))
		}
	}
	changes = append(changes, Change{Agent: agent,
		Detail: fmt.Sprintf("removed the old %q registration", LegacyServerName)})
	return changes, nil
}

// LegacyToolMentions lists the files under home's Claude Code settings that
// still name the old tools (`mcp__ctx__...`), typically permission
// allowlists. They are reported, not rewritten: an allowlist is the user's
// security decision, and kae should not widen or move it on its own.
func LegacyToolMentions(home string) []string {
	var hits []string
	for _, name := range []string{"settings.json", "settings.local.json", "CLAUDE.md"} {
		path := filepath.Join(home, ".claude", name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "mcp__"+LegacyServerName+"__") {
			hits = append(hits, path)
		}
	}
	return hits
}
