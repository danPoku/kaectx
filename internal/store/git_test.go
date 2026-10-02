// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectPathMapsWSLPathsToWindowsRepoRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows host mapping test")
	}
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")
	repo = realDir(t, repo)
	sub := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	toWSL := func(p string) string {
		volume := filepath.VolumeName(p)
		rest := strings.TrimPrefix(filepath.ToSlash(p[len(volume):]), "/")
		return "/mnt/" + strings.ToLower(volume[:1]) + "/" + rest
	}
	got := NewGitResolver().ProjectPath(toWSL(sub), toWSL(filepath.Join(repo, "cmd", "main.go")))
	if got != "cmd/main.go" {
		t.Errorf("cross-host ProjectPath = %q, want cmd/main.go", got)
	}
}

// gitRepo makes a repo with one empty commit per date and returns its dir
// and the commit hashes, oldest first. Dates are fixed so the resolver's
// answers are deterministic.
func gitRepo(t *testing.T, dates ...string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	run := func(date string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q", "-b", "main")
	var hashes []string
	for _, d := range dates {
		run(d, "commit", "-q", "--allow-empty", "-m", "c "+d)
		hashes = append(hashes, run("", "rev-parse", "HEAD"))
	}
	return dir, hashes
}

func TestGitResolverReturnsTheCommitCurrentAtTheTimestamp(t *testing.T) {
	dir, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z")
	g := NewGitResolver()

	cases := []struct {
		name, ts, want string
	}{
		{"before the first commit", "2025-12-31T00:00:00Z", ""},
		{"exactly at a commit", "2026-02-01T00:00:00Z", c[1]},
		{"between commits 1 and 2", "2026-01-15T00:00:00Z", c[0]},
		{"between commits 2 and 3", "2026-02-15T00:00:00Z", c[1]},
		{"after HEAD", "2026-09-01T00:00:00Z", c[2]},
		{"unparsable timestamp", "yesterday", ""},
		{"empty timestamp", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.CommitAt(dir, "", tc.ts); got != tc.want {
				t.Errorf("CommitAt(%q) = %q, want %q", tc.ts, got, tc.want)
			}
		})
	}
}

// The regression this guards: HEAD-at-ingest-time would stamp an old
// session with the newest commit. The answer must track the log's time.
func TestGitResolverDoesNotReturnHeadForOldSessions(t *testing.T) {
	dir, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z")
	if got := NewGitResolver().CommitAt(dir, "", "2026-01-10T00:00:00Z"); got != c[0] {
		t.Errorf("got %q, want the first commit %q (HEAD is %q)", got, c[0], c[1])
	}
}

func TestGitResolverPrefersTheSessionsBranchAndFallsBackToHead(t *testing.T) {
	dir, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z")
	if out, err := exec.Command("git", "-C", dir, "branch", "old", c[1]).CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	g := NewGitResolver()
	if got := g.CommitAt(dir, "old", "2026-09-01T00:00:00Z"); got != c[1] {
		t.Errorf("branch old: got %q, want %q", got, c[1])
	}
	if got := g.CommitAt(dir, "deleted-branch", "2026-09-01T00:00:00Z"); got != c[2] {
		t.Errorf("missing branch: got %q, want HEAD %q", got, c[2])
	}
}

func TestGitResolverNonGitOrEmptyDirYieldsNothing(t *testing.T) {
	g := NewGitResolver()
	if got := g.CommitAt(t.TempDir(), "", "2026-01-01T00:00:00Z"); got != "" {
		t.Errorf("non-git dir: got %q", got)
	}
	if got := g.CommitAt("", "", "2026-01-01T00:00:00Z"); got != "" {
		t.Errorf("empty dir: got %q", got)
	}
	if got := g.CommitAt("/definitely/not/a/dir", "", "2026-01-01T00:00:00Z"); got != "" {
		t.Errorf("missing dir: got %q", got)
	}
}

// The other regression: one subprocess per log line. However many lookups a
// directory sees, the history is read once — including for non-git dirs.
func TestGitResolverSpawnsOncePerDirectory(t *testing.T) {
	dir, _ := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z")
	plain := t.TempDir()
	g := NewGitResolver()
	for i := 0; i < 1000; i++ {
		g.CommitAt(dir, "", "2026-01-15T00:00:00Z")
		g.CommitAt(plain, "", "2026-01-15T00:00:00Z")
	}
	if g.Spawns() != 2 {
		t.Errorf("spawns = %d, want 2 (one per directory)", g.Spawns())
	}
}

// Committer dates can run backwards after a rebase or with clock skew; the
// binary search must not return a wrong commit because of it.
func TestParseHistorySortsOutOfOrderCommitterDates(t *testing.T) {
	entries := parseHistory("bbb 200\naaa 300\nccc 100\ngarbage\nddd notanumber\n")
	if len(entries) != 3 || entries[0].hash != "aaa" || entries[2].hash != "ccc" {
		t.Errorf("entries = %+v, want aaa,bbb,ccc newest-first", entries)
	}
}

// realDir resolves symlinks in a TempDir path (git reports real paths).
func realDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The same file must get the same stored path no matter which directory the
// agent was launched from.
func TestProjectPathAnchorsAtTheRepoRoot(t *testing.T) {
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")
	repo = realDir(t, repo)
	sub := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGitResolver()

	cases := []struct{ name, cwd, in, want string }{
		{"relative from a subdirectory", sub, "store.go", "internal/store/store.go"},
		{"absolute from a subdirectory", sub, filepath.Join(sub, "store.go"), "internal/store/store.go"},
		{"sibling of the subdirectory", sub, "../search/x.go", "internal/search/x.go"},
		{"absolute elsewhere in the repo", sub, filepath.Join(repo, "cmd", "main.go"), "cmd/main.go"},
		{"launched at the root", repo, "cmd/main.go", "cmd/main.go"},
		{"absolute from the root", repo, filepath.Join(repo, "cmd", "main.go"), "cmd/main.go"},
		{"absolute outside the repo is untouched", sub, "/etc/hosts", "/etc/hosts"},
		{"relative escaping the repo is untouched", repo, "../elsewhere.go", "../elsewhere.go"},
		{"empty path", sub, "", ""},
		{"empty cwd leaves the path alone", "", "/x/y.go", "/x/y.go"},
	}
	for _, c := range cases {
		if got := g.ProjectPath(c.cwd, c.in); got != c.want {
			t.Errorf("%s: ProjectPath(%q, %q) = %q, want %q", c.name, c.cwd, c.in, got, c.want)
		}
	}
}

// git reports real paths but the agent may have logged a symlinked cwd.
func TestProjectPathHandlesASymlinkedCwd(t *testing.T) {
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")
	repo = realDir(t, repo)
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	g := NewGitResolver()
	if got := g.ProjectPath(link, filepath.Join(link, "a.go")); got != "pkg/a.go" {
		t.Errorf("absolute via symlink = %q, want pkg/a.go", got)
	}
	if got := g.ProjectPath(link, "a.go"); got != "pkg/a.go" {
		t.Errorf("relative via symlink = %q, want pkg/a.go", got)
	}
}

// Outside git (or with the checkout gone) the old cwd-relative rule applies.
func TestProjectPathFallsBackToCwdOutsideGit(t *testing.T) {
	plain := t.TempDir()
	g := NewGitResolver()
	if got := g.ProjectPath(plain, filepath.Join(plain, "src", "a.go")); got != "src/a.go" {
		t.Errorf("non-git absolute = %q, want src/a.go", got)
	}
	if got := g.ProjectPath(plain, "src/a.go"); got != "src/a.go" {
		t.Errorf("non-git relative = %q", got)
	}
	if got := g.ProjectPath("/gone/checkout", "/gone/checkout/a.go"); got != "a.go" {
		t.Errorf("missing checkout = %q, want a.go", got)
	}
	if got := g.Root(plain); got != "" {
		t.Errorf("Root(non-git) = %q", got)
	}
	if got := g.Root(""); got != "" {
		t.Errorf("Root(empty) = %q", got)
	}
}

func TestProjectPathLooksUpTheRepoRootOncePerDirectory(t *testing.T) {
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")
	g := NewGitResolver()
	for i := 0; i < 500; i++ {
		g.ProjectPath(repo, "a.go")
	}
	if g.Spawns() != 1 {
		t.Errorf("spawns = %d, want 1", g.Spawns())
	}
}

// Callers show the source to users, so it has to say how the answer was got.
func TestGitResolverReportsHowACommitWasResolved(t *testing.T) {
	dir, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z")
	g := NewGitResolver()
	ts := "2026-01-15T00:00:00Z"

	for _, tc := range []struct {
		name, branch, wantHash, wantSource string
	}{
		{"branch exists", "main", c[0], SourceBranch},
		{"no branch recorded", "", c[0], SourceHead},
		{"branch since deleted", "gone", c[0], SourceHead},
		{"detached HEAD is logged as branch HEAD", "HEAD", c[0], SourceHead},
	} {
		hash, src := g.Resolve(dir, tc.branch, ts)
		if hash != tc.wantHash || src != tc.wantSource {
			t.Errorf("%s: Resolve = %q, %q; want %q, %q", tc.name, hash, src, tc.wantHash, tc.wantSource)
		}
	}
	if hash, src := g.Resolve(dir, "main", "2025-01-01T00:00:00Z"); hash != "" || src != "" {
		t.Errorf("before history: %q, %q; want both empty", hash, src)
	}
	if hash, src := g.Resolve(t.TempDir(), "main", ts); hash != "" || src != "" {
		t.Errorf("non-git: %q, %q; want both empty", hash, src)
	}
}

func TestSourceRankOrdersFromMostToLeastTrustworthy(t *testing.T) {
	order := []string{"", SourceLogged, SourceBranch, SourceHead, SourceUnverified}
	for i := 1; i < len(order); i++ {
		if SourceRank[order[i]] <= SourceRank[order[i-1]] {
			t.Errorf("%q must rank above %q", order[i], order[i-1])
		}
	}
}
