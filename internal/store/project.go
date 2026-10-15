// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/danPoku/kaectx/internal/paths"
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
//
// Before any of that, a cwd inside a linked git worktree is mapped to its
// main worktree (see mainWorktree), and paths are compared spelling-blind
// (see lookupByPathPrefix).
func ResolveProject(db DBTX, cwd string) (int64, error) {
	cwd = mainWorktree(filepath.Clean(cwd))

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
	cwd = mainWorktree(filepath.Clean(cwd))
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

// lookupByPathPrefix finds the project whose known path is the longest
// prefix of cwd. Paths are compared by paths.Key, so C:\x, c:\x and /mnt/c/x
// are one folder: an agent's logged spelling (drive-letter case, WSL vs.
// Windows) must not decide which project a session lands in.
//
// Catalogues built before this rule can hold one folder under two projects.
// When keys tie, the lower project id wins. The choice must not depend on
// which spelling cwd used, or the three spellings would still split;
// `ctx repair projects` then re-files the loser's sessions.
func lookupByPathPrefix(db DBTX, cwd string) (int64, bool, error) {
	rows, err := db.Query(`SELECT path, project_id FROM project_paths ORDER BY project_id`)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()

	key := paths.Key(cwd)
	bestKey, found := "", false
	var bestID int64
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			return 0, false, err
		}
		// Strictly longer only: rows come in project id order, so on a tie
		// the lower id, seen first, keeps the match.
		if pk := paths.Key(p); paths.Within(key, pk) && (!found || len(pk) > len(bestKey)) {
			bestKey, bestID, found = pk, id, true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return bestID, found, nil
}

// worktreeRoots caches mainWorktree's answer per directory for the life of
// the process: ingest resolves the same few cwds for thousands of lines, and
// each answer costs a git subprocess.
var worktreeRoots sync.Map

// mainWorktree maps a directory inside a linked git worktree to the same
// place in the repository's main worktree, so a worktree's sessions belong
// to its repo's project, not to whatever known folder happens to contain it
// (worktrees often live under a parent folder that is a project of its own).
// Anything else, including a directory that no longer exists or is not a
// repo, comes back unchanged.
func mainWorktree(dir string) string {
	if v, ok := worktreeRoots.Load(dir); ok {
		return v.(string)
	}
	resolved := dir
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute",
		"--git-dir", "--git-common-dir", "--show-toplevel").Output()
	if err == nil {
		if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) == 3 {
			gitDir := filepath.Clean(strings.TrimSpace(lines[0]))
			common := filepath.Clean(strings.TrimSpace(lines[1]))
			top := filepath.Clean(strings.TrimSpace(lines[2]))
			// A linked worktree has its own git dir under the common one
			// (<repo>/.git/worktrees/<name>). A bare repo has no main
			// worktree to map to, so only a common dir named .git counts.
			if paths.Key(gitDir) != paths.Key(common) && filepath.Base(common) == ".git" {
				if rel, err := filepath.Rel(top, dir); err == nil && !strings.HasPrefix(rel, "..") {
					resolved = filepath.Join(filepath.Dir(common), rel)
				}
			}
		}
	}
	worktreeRoots.Store(dir, resolved)
	return resolved
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
