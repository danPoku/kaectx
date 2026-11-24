// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package agentcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file holds what `kae setup` needs: registering kae with an agent that
// does not have it yet. The migration from the old `ctx` name is in claude.go
// and codex.go; both sides share the config readers and the text editor.

// HasClaude reports whether Claude Code has been used on this machine.
func HasClaude(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".claude"))
	return err == nil
}

// HasCodex reports whether Codex has been used on this machine.
func HasCodex(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".codex"))
	return err == nil
}

// LegacyClaudeRegistered reports whether Claude Code still has kae's old
// `ctx` server registered at user scope, the entry MigrateClaude replaces.
func LegacyClaudeRegistered(home string) bool {
	cfg, _, err := readClaudeConfig(home)
	if err != nil {
		return false
	}
	old, ok := cfg.MCPServers[LegacyServerName]
	return ok && IsLegacyCommand(old.Command) && isServeArgs(old.Args)
}

// EnsureClaude registers kae's MCP server with Claude Code at user scope,
// starting command, unless a kae registration is already there. An existing
// one is left exactly as it is, whatever it runs: the user may have pointed
// it somewhere on purpose.
func EnsureClaude(home string, run Runner, command string, dryRun bool) ([]Change, error) {
	const agent = "claude-code"
	cfg, _, err := readClaudeConfig(home)
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.MCPServers[ServerName]; ok {
		return nil, nil
	}
	add := []string{"mcp", "add", "--scope", "user", ServerName, "--", command, "mcp"}
	if _, err := run.LookPath("claude"); err != nil {
		return []Change{{Agent: agent, Manual: true,
			Detail: "the claude command is not on PATH; run: claude " + shellQuote(add)}}, nil
	}
	if !dryRun {
		if out, err := run.Run("claude", add...); err != nil {
			return nil, fmt.Errorf("claude mcp add: %w: %s", err, strings.TrimSpace(out))
		}
	}
	return []Change{{Agent: agent,
		Detail: fmt.Sprintf("registered MCP server %q (%s mcp) at user scope", ServerName, command)}}, nil
}

// approvalKey is the setting that lets `codex exec`, which has nobody to
// click "approve", call an MCP server's tools.
const approvalKey = "default_tools_approval_mode"

var approvalLine = regexp.MustCompile(`^\s*` + approvalKey + `\s*=`)

// CodexState is what config.toml says about kae.
type CodexState struct {
	// Registered is true when [mcp_servers.kae] exists.
	Registered bool
	// ApprovalSet is true when that table sets default_tools_approval_mode,
	// to any value.
	ApprovalSet bool
}

// ReadCodex reports the kae registration in Codex's config, if any.
func ReadCodex(home string) (CodexState, error) {
	data, err := os.ReadFile(CodexConfigPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return CodexState{}, nil
	}
	if err != nil {
		return CodexState{}, err
	}
	f := parseTOML(string(data))
	return CodexState{Registered: f.hasServer(ServerName), ApprovalSet: f.mainTableHas(ServerName, approvalLine)}, nil
}

// mainTableHas reports whether a line of [mcp_servers.<name>] matches re.
func (f tomlFile) mainTableHas(name string, re *regexp.Regexp) bool {
	want := "mcp_servers." + name
	for _, span := range f.serverTables(name) {
		if tableName(f.lines[span[0]]) != want {
			continue
		}
		for _, line := range f.lines[span[0]+1 : span[1]] {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}

// tomlString writes s as a TOML string. A literal (single-quoted) string
// takes a Windows path as it is; a path containing a single quote falls back
// to a basic string with its backslashes and quotes escaped.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n") {
		return "'" + s + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// EnsureCodex adds a [mcp_servers.kae] table that starts command to Codex's
// config.toml, creating the file if Codex has none yet. With approve it also
// sets default_tools_approval_mode = "approve" in that table when the table
// does not set it already. Existing content is never rewritten: a new table
// is appended at the end of the file, and the approval line is inserted
// directly under the table's header.
func EnsureCodex(home, command string, approve, dryRun bool) ([]Change, error) {
	const agent = "codex"
	path := CodexConfigPath(home)
	data, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return nil, err
	}
	f := parseTOML(string(data))
	var changes []Change

	if !f.hasServer(ServerName) {
		// Drop trailing blank lines, then leave exactly one before the new
		// table (none in an empty file).
		for len(f.lines) > 0 && strings.TrimSpace(f.lines[len(f.lines)-1]) == "" {
			f.lines = f.lines[:len(f.lines)-1]
		}
		if len(f.lines) > 0 {
			f.lines = append(f.lines, "")
		}
		f.lines = append(f.lines,
			"[mcp_servers."+ServerName+"]",
			"command = "+tomlString(command),
			`args = ["mcp"]`,
		)
		if approve {
			f.lines = append(f.lines, approvalKey+` = "approve"`)
		}
		f.lines = append(f.lines, "")
		detail := fmt.Sprintf("added [mcp_servers.%s] to %s (command %s)", ServerName, path, command)
		if approve {
			detail += `, with ` + approvalKey + ` = "approve"`
		}
		changes = append(changes, Change{Agent: agent, Detail: detail})
	} else if approve && !f.mainTableHas(ServerName, approvalLine) {
		want := "mcp_servers." + ServerName
		for _, span := range f.serverTables(ServerName) {
			if tableName(f.lines[span[0]]) != want {
				continue
			}
			at := span[0] + 1
			f.lines = append(f.lines[:at], append([]string{approvalKey + ` = "approve"`}, f.lines[at:]...)...)
			break
		}
		changes = append(changes, Change{Agent: agent,
			Detail: fmt.Sprintf(`set %s = "approve" in [mcp_servers.%s] in %s`, approvalKey, ServerName, path)})
	}

	if len(changes) == 0 || dryRun {
		return changes, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	var previous []byte
	if !missing {
		previous = data
	}
	if err := writeWithBackup(path, previous, f.String()); err != nil {
		return nil, err
	}
	return changes, nil
}
