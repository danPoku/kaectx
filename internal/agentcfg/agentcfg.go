// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package agentcfg reads and changes how coding agents are told to start
// kae's MCP server: Claude Code's user-scope registration and the
// [mcp_servers.*] table in Codex's config.toml.
//
// The two agents are handled differently on purpose:
//
//   - Claude Code keeps its registrations in ~/.claude.json, a file it
//     rewrites constantly while running. Editing it from outside is like
//     correcting a ledger while the clerk is still writing in it. So kae only
//     reads that file, and makes changes through Claude Code's own
//     `claude mcp` command.
//   - Codex has no such command for an existing entry, and its config.toml is
//     a hand-edited file with comments. kae edits it as text, touching only
//     the lines of its own table, so everything else stays byte for byte.
//
// Shapes were read from real files: Claude Code 2.1.x (`mcpServers` at the
// top level of ~/.claude.json for user scope, and under projects.<path> for
// local scope) and codex-cli 0.153 (`[mcp_servers.<name>]` with `command`
// and `args`).
package agentcfg

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const (
	// ServerName is the name kae registers its MCP server under, and
	// LegacyServerName the one used while the command was `ctx`.
	ServerName       = "kae"
	LegacyServerName = "ctx"
)

// Change is one thing a migration did, would do (dry run), or could not do
// itself and is asking the user to do.
type Change struct {
	// Agent is "claude-code" or "codex".
	Agent string
	// Detail says what changed, in words a person can check.
	Detail string
	// Manual is true when kae did not make the change and Detail is the
	// instruction for the user.
	Manual bool
}

// Runner runs an agent's own command-line tool. It is an interface so tests
// can stand in for a `claude` binary without one installed.
type Runner interface {
	// LookPath reports whether name is an executable on PATH.
	LookPath(name string) (string, error)
	// Run runs name with args and returns its combined output.
	Run(name string, args ...string) (string, error)
}

// ExecRunner is the Runner that really runs commands.
type ExecRunner struct{}

func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (ExecRunner) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// legacyBinary matches a command whose last path segment is the old binary
// name, in the raw text of a config value: `ctx`, `/home/a/go/bin/ctx`,
// `C:\Users\a\go\bin\ctx.exe`, or the same with the backslashes doubled as
// TOML and JSON basic strings write them.
var legacyBinary = regexp.MustCompile(`(?i)(^|[\\/])ctx(\.exe)?$`)

// IsLegacyCommand reports whether command starts the old `ctx` binary.
func IsLegacyCommand(command string) bool {
	return legacyBinary.MatchString(command)
}

// RenameCommand returns command with a trailing `ctx` binary name replaced
// by `kae`, keeping its folder and extension: the two binaries are installed
// side by side by every install route. Other commands are returned as they
// are.
func RenameCommand(command string) string {
	return legacyBinary.ReplaceAllString(command, "${1}"+ServerName+"${2}")
}

// isServeArgs reports whether args are the ones that start the MCP server.
// Together with IsLegacyCommand this is how kae recognises an entry as its
// own: another tool that happens to be registered under the name "ctx" has a
// different command or arguments, and is left alone.
func isServeArgs(args []string) bool {
	return len(args) == 1 && args[0] == "mcp"
}

func shellQuote(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'\\") {
			a = fmt.Sprintf("%q", a)
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}
