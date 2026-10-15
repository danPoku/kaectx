// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveProject finds or creates the project that owns cwd. Matching is by
// the longest known project_paths prefix, so an agent invoked in a
// subdirectory of a known project root still resolves to that project — the
// same rule the daemon will use later when mapping a live agent's cwd to a
// project.
//
// A brand-new cwd creates a new project. If cwd is inside a git repo with an
// `origin` remote, that remote (normalized) becomes the project's identity:
// if a project with the same normalized remote already exists — the same
// repo cloned at a different path, WSL vs. a Codespace being the motivating
// case — cwd is just added as another path to that existing project instead
// of creating a duplicate. No remote (not a git repo, or a repo with none
// configured) means the project is identified purely by path.
func ResolveProject(db DBTX, cwd string) (int64, error) {
	cwd = filepath.Clean(cwd)

	if id, ok, err := lookupByPathPrefix(db, cwd); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}

	remote := normalizedOriginRemote(cwd)
	if remote != "" {
		var id int64
		err := db.QueryRow(`SELECT id FROM projects WHERE git_remote = ?`, remote).Scan(&id)
		if err == nil {
			if _, err := db.Exec(`INSERT OR IGNORE INTO project_paths(path, project_id) VALUES (?, ?)`, cwd, id); err != nil {
				return 0, err
			}
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}

	name := filepath.Base(cwd)
	res, err := db.Exec(`INSERT INTO projects(name, git_remote) VALUES (?, ?)`, name, nullIfEmpty(remote))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := db.Exec(`INSERT INTO project_paths(path, project_id) VALUES (?, ?)`, cwd, id); err != nil {
		return 0, err
	}
	return id, nil
}

// DBTX is what project resolution needs from a connection: satisfied by both
// *sql.DB (ingest, commands) and *sql.Tx (repair, which resolves projects
// inside one transaction so an interrupted repair changes nothing).
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// LookupProject finds the project that owns cwd without registering a new
// project or path. Read-only commands use it to avoid changing the catalogue
// merely because they were invoked from a new checkout or unrelated folder.
func LookupProject(db DBTX, cwd string) (int64, bool, error) {
	cwd = filepath.Clean(cwd)
	if id, ok, err := lookupByPathPrefix(db, cwd); err != nil || ok {
		return id, ok, err
	}
	remote := normalizedOriginRemote(cwd)
	if remote == "" {
		return 0, false, nil
	}
	var id int64
	err := db.QueryRow(`SELECT id FROM projects WHERE git_remote = ?`, remote).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func lookupByPathPrefix(db DBTX, cwd string) (int64, bool, error) {
	rows, err := db.Query(`SELECT path, project_id FROM project_paths`)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()

	bestPath := ""
	var bestID int64
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			return 0, false, err
		}
		if (cwd == p || strings.HasPrefix(cwd, p+string(filepath.Separator))) && len(p) > len(bestPath) {
			bestPath, bestID = p, id
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return bestID, bestPath != "", nil
}

// normalizedOriginRemote returns dir's `origin` remote normalized to
// 'host/owner/repo' (no scheme, no .git, lowercase), or "" if dir isn't a
// git repo or has no origin configured.
func normalizedOriginRemote(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return normalizeGitRemote(string(out))
}

var (
	sshShorthand = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)
	schemeURL    = regexp.MustCompile(`^\w+://(?:[^@/]+@)?([^/]+)/(.+)$`)
)

func normalizeGitRemote(url string) string {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	if m := sshShorthand.FindStringSubmatch(url); m != nil {
		return strings.ToLower(m[1] + "/" + m[2])
	}
	if m := schemeURL.FindStringSubmatch(url); m != nil {
		return strings.ToLower(m[1] + "/" + m[2])
	}
	return strings.ToLower(url)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
