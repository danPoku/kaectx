// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package explain builds compact, file-centred memory briefings for the CLI
// and MCP server.
package explain

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/danPoku/kaectx/internal/paths"

	"github.com/danPoku/kaectx/internal/store"
	"github.com/danPoku/kaectx/queries"
)

const (
	DefaultLimit = 10
	// MaxLimit bounds how many sessions one call returns, so an agent passing
	// limit=100000 can't turn a compact briefing into a transcript dump.
	MaxLimit = 50
)

type Session struct {
	SessionID      string `json:"session_id"`
	Agent          string `json:"agent"`
	Title          string `json:"title,omitempty"`
	StartedAt      string `json:"started_at"`
	GitBranch      string `json:"git_branch,omitempty"`
	StartingCommit string `json:"starting_commit,omitempty"`
	EndingCommit   string `json:"ending_commit,omitempty"`
	// CommitSource is how the commits were obtained (store.Source*), empty
	// when unknown. CommitApproximate is true when they are a best guess:
	// resolved from HEAD rather than the session's branch, or unverifiable.
	CommitSource      string   `json:"commit_source,omitempty"`
	CommitApproximate bool     `json:"commit_approximate,omitempty"`
	Actions           []string `json:"actions"`
	Touches           int      `json:"touches"`
	Changes           int      `json:"changes"` // touches that were not plain reads
	LastTouchAt       string   `json:"last_touch_at,omitempty"`
	ChunkID           *int64   `json:"chunk_id,omitempty"`
	Preview           string   `json:"preview,omitempty"`
}

type Result struct {
	Path     string    `json:"path"`
	Sessions []Session `json:"sessions"`
}

// File explains path: the sessions that touched it in projectID, edits first.
// The result's slices are never nil, so it serialises as [] rather than null.
func File(db *sql.DB, projectID int64, path string, limit int) (Result, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	path = NormalizePath(path)
	sqlText, err := store.LoadQuery(queries.FS, "retrieval.sql", "ExplainFile")
	if err != nil {
		return Result{}, err
	}
	rows, err := db.Query(sqlText, sql.Named("project_id", projectID), sql.Named("path", path), sql.Named("limit", limit))
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()

	out := Result{Path: path, Sessions: []Session{}}
	for rows.Next() {
		var s Session
		var title, branch, startCommit, endCommit, source, actions, lastTouch, preview sql.NullString
		var chunkID sql.NullInt64
		if err := rows.Scan(&s.SessionID, &s.Agent, &title, &s.StartedAt, &branch, &startCommit, &endCommit, &source,
			&actions, &s.Touches, &s.Changes, &lastTouch, &chunkID, &preview); err != nil {
			return Result{}, err
		}
		s.Title = title.String
		s.GitBranch = branch.String
		s.StartingCommit = startCommit.String
		s.EndingCommit = endCommit.String
		s.CommitSource = source.String
		s.CommitApproximate = source.String == store.SourceHead || source.String == store.SourceUnverified
		s.LastTouchAt = lastTouch.String
		s.Actions = splitActions(actions.String)
		if chunkID.Valid {
			id := chunkID.Int64
			s.ChunkID = &id
		}
		s.Preview = compactPreview(preview.String)
		out.Sessions = append(out.Sessions, s)
	}
	return out, rows.Err()
}

// NormalizePath puts a user-supplied path in the form file_touches stores:
// slash-separated, cleaned, no leading "./".
func NormalizePath(p string) string {
	return paths.CleanSlash(p)
}

// ResolvePath turns a path the caller typed (relative to cwd, or absolute)
// into the project-relative form file_touches uses. Running
// `ctx explain foo.go` from internal/store means internal/store/foo.go, not a
// file called foo.go at the project root. A path that lands outside the
// project root is returned normalised but otherwise untouched.
func ResolvePath(cwd, p string) string {
	p = strings.TrimSpace(p)
	if p == "" || cwd == "" {
		return NormalizePath(p)
	}
	originalPath := p
	if mappedCWD, ok := paths.ToHost(cwd); ok {
		if _, err := os.Stat(mappedCWD); err == nil {
			cwd = mappedCWD
			if mappedPath, ok := paths.ToHost(p); ok {
				p = mappedPath
			}
		}
	}
	abs := p
	if paths.IsAbs(abs) && !filepath.IsAbs(abs) {
		if rel, ok := paths.RelIfWithin(cwd, abs); ok {
			return NormalizePath(rel)
		}
		return NormalizePath(originalPath)
	}
	if !paths.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	root := repoRoot(cwd)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return NormalizePath(originalPath)
	}
	return NormalizePath(rel)
}

// ProjectPath is ResolvePath for callers whose relative paths are already
// project-relative (the MCP tool's documented contract): only an absolute
// path is rewritten, and a relative one is just normalised. Rebasing a
// relative path onto the server's cwd would turn a correct
// "internal/store/store.go" into a wrong one whenever the agent launched
// `ctx mcp` from a subdirectory.
func ProjectPath(cwd, p string) string {
	if paths.IsAbs(strings.TrimSpace(p)) {
		return ResolvePath(cwd, p)
	}
	return NormalizePath(p)
}

// repoRoot is the git toplevel containing dir, or dir itself outside a repo.
func repoRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return dir
	}
	return strings.TrimSpace(string(out))
}

func splitActions(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func compactPreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 360
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
