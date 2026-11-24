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

// CodexConfigPath is Codex's configuration file.
func CodexConfigPath(home string) string {
	return filepath.Join(home, ".codex", "config.toml")
}

// tableHeader matches a TOML table header line and captures its dotted name.
// Array-of-tables headers ([[x]]) are matched too, so they end a table.
var tableHeader = regexp.MustCompile(`^\s*\[{1,2}\s*([^\[\]]+?)\s*\]{1,2}\s*(#.*)?$`)

// commandLine matches `command = "..."` or `command = '...'` and captures
// the text around the quoted value, so the value can be replaced while the
// indentation, quote style and any trailing comment stay as written.
var commandLine = regexp.MustCompile(`^(\s*command\s*=\s*)(["'])(.*)(["'])(\s*(?:#.*)?)$`)

// tomlFile is a config file held as lines, so an edit changes only the lines
// it means to. A TOML library would parse and re-serialise the whole file,
// which drops comments and reorders keys in a file people edit by hand.
type tomlFile struct {
	lines []string
	// eol is the file's line ending, kept so a Windows file stays CRLF.
	eol string
}

func parseTOML(data string) tomlFile {
	eol := "\n"
	if strings.Contains(data, "\r\n") {
		eol = "\r\n"
	}
	return tomlFile{lines: strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n"), eol: eol}
}

func (f tomlFile) String() string { return strings.Join(f.lines, f.eol) }

// tableName returns the dotted table name a header line declares, with the
// quotes TOML allows around a key removed, or "" if line is not a header.
func tableName(line string) string {
	m := tableHeader.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	parts := strings.Split(m[1], ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// serverTables returns the [start, end) line ranges of the table
// mcp_servers.<name> and its sub-tables (mcp_servers.<name>.env and so on).
// A table runs from its header to the line before the next header.
func (f tomlFile) serverTables(name string) [][2]int {
	want := "mcp_servers." + name
	var spans [][2]int
	start := -1
	for i, line := range f.lines {
		n := tableName(line)
		if n == "" {
			continue
		}
		if start >= 0 {
			spans = append(spans, [2]int{start, i})
			start = -1
		}
		if n == want || strings.HasPrefix(n, want+".") {
			start = i
		}
	}
	if start >= 0 {
		spans = append(spans, [2]int{start, len(f.lines)})
	}
	return spans
}

// hasServer reports whether mcp_servers.<name> is declared.
func (f tomlFile) hasServer(name string) bool {
	return len(f.serverTables(name)) > 0
}

// serverCommand returns the raw text of the table's command value.
func (f tomlFile) serverCommand(name string) (string, bool) {
	want := "mcp_servers." + name
	for _, span := range f.serverTables(name) {
		if tableName(f.lines[span[0]]) != want {
			continue
		}
		for _, line := range f.lines[span[0]+1 : span[1]] {
			if m := commandLine.FindStringSubmatch(line); m != nil {
				return m[3], true
			}
		}
	}
	return "", false
}

// renameServer rewrites the headers of mcp_servers.<from> and its sub-tables
// to <to>, and passes the main table's command through fix.
func (f *tomlFile) renameServer(from, to string, fix func(string) string) {
	main := "mcp_servers." + from
	for _, span := range f.serverTables(from) {
		header := f.lines[span[0]]
		isMain := tableName(header) == main
		// Replace only the name segment, wherever the header's own spacing
		// and quoting put it.
		seg := regexp.MustCompile(`(mcp_servers\s*\.\s*)(["']?)` + regexp.QuoteMeta(from) + `(["']?)`)
		f.lines[span[0]] = seg.ReplaceAllString(header, "${1}${2}"+to+"${3}")
		if !isMain {
			continue
		}
		for i := span[0] + 1; i < span[1]; i++ {
			if m := commandLine.FindStringSubmatch(f.lines[i]); m != nil {
				f.lines[i] = m[1] + m[2] + fix(m[3]) + m[4] + m[5]
			}
		}
	}
}

// removeServer deletes mcp_servers.<name> and its sub-tables.
func (f *tomlFile) removeServer(name string) {
	spans := f.serverTables(name)
	for i := len(spans) - 1; i >= 0; i-- {
		start, end := spans[i][0], spans[i][1]
		f.lines = append(f.lines[:start], f.lines[end:]...)
	}
}

// MigrateCodex renames a `[mcp_servers.ctx]` table in Codex's config.toml to
// `[mcp_servers.kae]` and points its command at the renamed binary. Nothing
// else in the file changes: other tables, key order, comments and line
// endings are kept. A copy of the file as it was is left beside it.
//
// If a kae table already exists the old one is removed instead. Running it
// again finds no ctx table and changes nothing.
func MigrateCodex(home string, dryRun bool) ([]Change, error) {
	const agent = "codex"
	path := CodexConfigPath(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := parseTOML(string(data))
	if !f.hasServer(LegacyServerName) {
		return nil, nil
	}
	if cmd, ok := f.serverCommand(LegacyServerName); !ok || !IsLegacyCommand(cmd) {
		return []Change{{Agent: agent, Manual: true,
			Detail: fmt.Sprintf("[mcp_servers.%s] in %s does not run kae's old binary; left as it is", LegacyServerName, path)}}, nil
	}

	var change Change
	if f.hasServer(ServerName) {
		f.removeServer(LegacyServerName)
		change = Change{Agent: agent, Detail: fmt.Sprintf("removed [mcp_servers.%s] from %s; [mcp_servers.%s] is already there", LegacyServerName, path, ServerName)}
	} else {
		f.renameServer(LegacyServerName, ServerName, RenameCommand)
		cmd, _ := f.serverCommand(ServerName)
		change = Change{Agent: agent, Detail: fmt.Sprintf("renamed [mcp_servers.%s] to [mcp_servers.%s] in %s (command %s)", LegacyServerName, ServerName, path, cmd)}
	}
	if dryRun {
		return []Change{change}, nil
	}
	if err := writeWithBackup(path, data, f.String()); err != nil {
		return nil, err
	}
	return []Change{change}, nil
}

// writeWithBackup replaces path with content, first saving the previous
// bytes as path+".kae-backup". The new content goes to a temporary file in
// the same folder and is renamed into place, so a crash part way leaves
// either the old file or the new one, never half of each.
func writeWithBackup(path string, previous []byte, content string) error {
	info, err := os.Stat(path)
	mode := fs.FileMode(0600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if previous != nil {
		if err := os.WriteFile(path+".kae-backup", previous, mode); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".kae-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
