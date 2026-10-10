// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/danPoku/kaectx/internal/paths"
	"time"
)

// maxHistoryCommits bounds how much history one lookup table holds. A session
// older than the oldest commit in the window resolves to "" rather than a
// wrong answer.
const maxHistoryCommits = 20000

// Where a commit came from, recorded in sessions.commit_source so `explain`
// can say how much to trust it.
const (
	// SourceBranch: resolved on the branch the session logged. Reliable, up to
	// the branch having been rebased since.
	SourceBranch = "branch"
	// SourceHead: the branch was unknown (Codex logs none) or has since been
	// deleted, so the commit was read from whatever HEAD's history says now.
	// Usually right, unverifiable.
	SourceHead = "head"
	// SourceUnverified: a commit stored by an earlier build (some stamped HEAD
	// at ingest time) that `ctx repair git` could not re-derive because the
	// repository is gone. Kept, but do not lean on it.
	SourceUnverified = "unverified"
	// SourceLogged: the agent's own log carried the commit.
	SourceLogged = "logged"
)

// SourceRank orders sources from most to least trustworthy; a session's span
// is only as good as its weakest end, so the stored source only ever worsens.
var SourceRank = map[string]int{"": 0, SourceLogged: 1, SourceBranch: 2, SourceHead: 3, SourceUnverified: 4}

type history struct {
	entries []commitEntry
	source  string
}

type commitEntry struct {
	hash string
	at   int64 // committer time, unix seconds
}

// GitResolver answers "which commit was checked out around time T?" for
// session logs, without one subprocess per log line: it reads each
// directory's first-parent history once (one `git log`), then answers every
// lookup from memory. Non-git directories are cached as such, so they cost a
// single failed spawn.
//
// The answer is the newest commit on the branch (or HEAD when the branch no
// longer exists) whose committer time is at or before the timestamp. That is
// the state the session started from, which is what matters for
// retroactively ingested logs — HEAD as of ingest time would describe a
// repository the session never saw.
//
// A GitResolver is meant to live for one ingest run: commits made after it
// loaded a directory are invisible to it, and the next run reloads.
type GitResolver struct {
	run     func(dir string, args ...string) (string, error)
	history map[string]history      // key: dir + "\x00" + branch
	roots   map[string]repoLocation // key: dir
	spawns  int
}

// repoLocation is where a working directory sits in its repository. root is
// "" when dir is not (or no longer) inside a git checkout; realDir is dir
// with symlinks resolved, because git reports real paths while an agent may
// have logged the symlinked spelling.
type repoLocation struct{ root, realDir string }

func NewGitResolver() *GitResolver {
	return &GitResolver{run: gitOutput, history: map[string]history{}, roots: map[string]repoLocation{}}
}

func gitOutput(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return string(out), err
}

// Spawns reports how many git subprocesses the resolver has started.
func (g *GitResolver) Spawns() int { return g.spawns }

// CommitAt returns the commit that was current in dir at timestamp ts
// (RFC 3339), preferring branch's history when branch is non-empty. It
// returns "" when dir is not a git checkout, ts is unparsable, or every
// known commit is newer than ts.
func (g *GitResolver) CommitAt(dir, branch, ts string) string {
	hash, _ := g.Resolve(dir, branch, ts)
	return hash
}

// Resolve is CommitAt plus how the answer was obtained (SourceBranch or
// SourceHead). The source is "" whenever the hash is.
func (g *GitResolver) Resolve(dir, branch, ts string) (hash, source string) {
	if dir == "" {
		return "", ""
	}
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "", ""
	}
	h := g.load(dir, branch)
	// entries are newest-first: find the first one not after `at`.
	i := sort.Search(len(h.entries), func(i int) bool { return h.entries[i].at <= at.Unix() })
	if i == len(h.entries) {
		return "", ""
	}
	return h.entries[i].hash, h.source
}

func (g *GitResolver) load(dir, branch string) history {
	// Claude Code logs the literal branch "HEAD" for a detached checkout. That
	// is not a branch: resolving it reads today's HEAD, so it must not be
	// reported as a reliable branch resolution.
	if branch == "HEAD" {
		branch = ""
	}
	key := dir + "\x00" + branch
	if h, ok := g.history[key]; ok {
		return h
	}
	var h history
	// Try the recorded branch first; a deleted or renamed branch falls back
	// to whatever is checked out now.
	refs := []string{"HEAD"}
	if branch != "" {
		refs = []string{branch, "HEAD"}
	}
	for _, ref := range refs {
		g.spawns++
		out, err := g.run(dir, "log", "--first-parent", "-n", strconv.Itoa(maxHistoryCommits),
			"--format=%H %ct", ref, "--")
		if err != nil {
			continue
		}
		h = history{entries: parseHistory(out), source: SourceHead}
		if ref == branch {
			h.source = SourceBranch
		}
		break
	}
	g.history[key] = h
	return h
}

func parseHistory(out string) []commitEntry {
	var entries []commitEntry
	for _, l := range strings.Split(out, "\n") {
		hash, ct, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok {
			continue
		}
		at, err := strconv.ParseInt(ct, 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, commitEntry{hash: hash, at: at})
	}
	// Committer dates can go backwards (rebases, clock skew); the binary
	// search needs a monotonic order, so sort newest-first explicitly.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at > entries[j].at })
	return entries
}

func (g *GitResolver) locate(dir string) repoLocation {
	if loc, ok := g.roots[dir]; ok {
		return loc
	}
	loc := repoLocation{realDir: dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		loc.realDir = real
	}
	g.spawns++
	if out, err := g.run(dir, "rev-parse", "--show-toplevel"); err == nil {
		loc.root = strings.TrimSpace(out)
	}
	g.roots[dir] = loc
	return loc
}

// Root returns the git toplevel containing dir, or "" if dir is not inside a
// checkout (including when the directory no longer exists).
func (g *GitResolver) Root(dir string) string {
	if dir == "" {
		return ""
	}
	if mapped, ok := paths.ToHost(dir); ok {
		if _, err := os.Stat(mapped); err == nil {
			dir = mapped
		}
	}
	return g.locate(dir).root
}

// ProjectPath turns a path a tool reported into the form file_touches stores:
// relative to the git repository root, slash-separated. path may be absolute
// or relative to the session's cwd.
//
// Anchoring at the repo root rather than at cwd matters because agents are
// often launched from a subdirectory: with cwd-relative paths the same file
// is "store.go" in one session and "internal/store/store.go" in another, and
// `ctx explain` from the repo root can only ever find the second.
//
// A path outside the repository is returned exactly as given. When cwd is not
// a git checkout at all, the root falls back to cwd, which is the behaviour
// before file-aware memory.
func (g *GitResolver) ProjectPath(cwd, path string) string {
	if cwd == "" || path == "" {
		return path
	}
	originalPath := path
	if mappedCWD, ok := paths.ToHost(cwd); ok {
		if _, err := os.Stat(mappedCWD); err == nil {
			cwd = mappedCWD
			if mappedPath, ok := paths.ToHost(path); ok {
				path = mappedPath
			}
		}
	}
	abs := path
	if paths.IsAbs(abs) && !filepath.IsAbs(abs) {
		if rel, ok := paths.RelIfWithin(cwd, abs); ok {
			return rel
		}
		return originalPath
	}
	if !paths.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	loc := g.locate(cwd)
	base := cwd
	if loc.root != "" {
		base = loc.root
		// The agent logged cwd as spelled; git reports the real path.
		if loc.realDir != cwd && (abs == cwd || strings.HasPrefix(abs, cwd+string(filepath.Separator))) {
			abs = loc.realDir + abs[len(cwd):]
		}
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return originalPath
	}
	return filepath.ToSlash(rel)
}
