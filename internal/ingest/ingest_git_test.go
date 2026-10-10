// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/ingest/codex"
)

// gitRepo makes a repo with one empty commit per date and returns its dir and
// the commit hashes, oldest first.
func gitRepo(t *testing.T, dates ...string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
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

func claudeLine(cwd, ts, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "sessionId": "sess-git", "cwd": cwd, "timestamp": ts,
		"message": map[string]any{"role": "user", "content": text},
	})
	return string(b) + "\n"
}

// The commit span must describe the repository as the session saw it: a
// session logged in January and ingested today is not "at HEAD".
func TestRunRecordsCommitSpanFromLogTimestampsNotIngestTime(t *testing.T) {
	db := openTestDB(t)
	repo, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z")

	path := filepath.Join(t.TempDir(), "s.jsonl")
	content := claudeLine(repo, "2026-01-15T00:00:00Z", "first") + claudeLine(repo, "2026-02-15T00:00:00Z", "second")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	get := func() (start, end string) {
		var s, e *string
		if err := db.QueryRow(`SELECT starting_commit, ending_commit FROM sessions WHERE id='claude-code:sess-git'`).Scan(&s, &e); err != nil {
			t.Fatalf("query: %v", err)
		}
		if s != nil {
			start = *s
		}
		if e != nil {
			end = *e
		}
		return
	}
	if start, end := get(); start != c[0] || end != c[1] {
		t.Fatalf("span = %q..%q, want %q..%q (HEAD is %q and must not appear)", start, end, c[0], c[1], c[2])
	}

	// The session continues later: ending advances, starting stays put.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(claudeLine(repo, "2026-03-15T00:00:00Z", "third"))
	f.Close()
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if start, end := get(); start != c[0] || end != c[2] {
		t.Errorf("after append span = %q..%q, want %q..%q", start, end, c[0], c[2])
	}
}

// Outside a git checkout ingest must still work and leave the fields empty.
func TestRunLeavesCommitsEmptyOutsideGit(t *testing.T) {
	db := openTestDB(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(claudeLine(t.TempDir(), "2026-01-15T00:00:00Z", "hi")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE starting_commit IS NULL AND ending_commit IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Errorf("sessions with empty commits = %d, %v; want 1", n, err)
	}
}

// Regression: git used to be spawned for every ingested line. A backfill of
// many lines across many files in one repo must read the history once.
func TestRunAllReadsGitHistoryOncePerRepoNotPerLine(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z")

	// A shim ahead of the real git on PATH that records every invocation.
	bin := t.TempDir()
	logFile := filepath.Join(bin, "calls.log")
	writeGitShim(t, bin, logFile, realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	for f := 0; f < 3; f++ {
		var sb strings.Builder
		for i := 0; i < 40; i++ {
			sb.WriteString(claudeLine(repo, fmt.Sprintf("2026-01-%02dT00:00:00Z", i%28+1), fmt.Sprintf("line %d", i)))
		}
		p := filepath.Join(root, fmt.Sprintf("f%d.jsonl", f))
		// distinct session per file
		body := strings.ReplaceAll(sb.String(), "sess-git", fmt.Sprintf("sess-%d", f))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	results, err := ingest.RunAll(openTestDB(t), func() ingest.Adapter { return claudecode.New() }, root)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Path, r.Err)
		}
	}

	raw, _ := os.ReadFile(logFile)
	// Only history reads count: project resolution runs its own
	// `git remote get-url`, which is not per line either but is not this
	// test's concern.
	calls := 0
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(l, " log ") {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("git log ran %d times for 120 lines in 3 files, want 1:\n%s", calls, raw)
	}
}

func writeGitShim(t *testing.T, bin, logFile, realGit string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		shim := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexec %q \"$@\"\n", logFile, realGit)
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	// Windows cannot execute a shebang shim. Build a tiny native proxy so the
	// test exercises the same PATH lookup and process boundary as production.
	source := `package main
import ("fmt"; "os"; "os/exec"; "strings")
func main() {
	f, err := os.OpenFile(os.Getenv("CTX_TEST_GIT_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil { panic(err) }
	fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
	f.Close()
	c := exec.Command(os.Getenv("CTX_TEST_REAL_GIT"), os.Args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil { if x, ok := err.(*exec.ExitError); ok { os.Exit(x.ExitCode()) }; panic(err) }
}`
	src := filepath.Join(bin, "git-shim.go")
	if err := os.WriteFile(src, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, "git.exe"), src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build git shim: %v\n%s", err, out)
	}
	t.Setenv("CTX_TEST_GIT_LOG", logFile)
	t.Setenv("CTX_TEST_REAL_GIT", realGit)
}

// Codex only writes cwd on session_meta/turn_context, and edits arrive as
// apply_patch envelopes naming several files at once. Both must end up as
// project-relative file_touches so explain works for Codex sessions.
func TestRunCodexApplyPatchRecordsRelativeFileTouches(t *testing.T) {
	db := openTestDB(t)
	repo, c := gitRepo(t, "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z")

	patch := "*** Begin Patch\n" +
		"*** Update File: " + repo + "/a.go\n@@\n-x\n+y\n" +
		"*** Add File: " + repo + "/b.go\n+z\n" +
		"*** Delete File: c.go\n" +
		"*** Update File: d.go\n*** Move to: e.go\n@@\n-1\n+2\n" +
		"*** End Patch"
	jl := func(typ, ts string, payload map[string]any) string {
		b, _ := json.Marshal(map[string]any{"timestamp": ts, "type": typ, "payload": payload})
		return string(b) + "\n"
	}
	content := jl("session_meta", "2026-01-10T00:00:00Z", map[string]any{"session_id": "cx1", "cwd": repo, "cli_version": "0"}) +
		jl("response_item", "2026-01-10T00:00:01Z", map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "change things"}},
			"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"user.text"}},
		}) +
		jl("response_item", "2026-01-10T00:00:02Z", map[string]any{
			"type": "custom_tool_call", "call_id": "k1", "name": "apply_patch", "input": patch,
		}) +
		jl("response_item", "2026-01-10T00:00:03Z", map[string]any{
			"type": "custom_tool_call", "call_id": "k2", "name": "exec", "input": "ls " + repo + "/notme.go",
		})
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows, err := db.Query(`SELECT path, action FROM file_touches ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p, a string
		if err := rows.Scan(&p, &a); err != nil {
			t.Fatal(err)
		}
		got = append(got, p+":"+a)
	}
	want := "a.go:edit,b.go:create,c.go:delete,d.go:edit,e.go:rename"
	if strings.Join(got, ",") != want {
		t.Errorf("file_touches = %v, want %s (exec must add none)", got, want)
	}

	// Re-ingest is idempotent.
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM file_touches`).Scan(&n)
	if n != 5 {
		t.Errorf("file_touches after re-run = %d, want 5", n)
	}

	// Codex lines get a commit too, resolved from the recorded cwd.
	var start string
	if err := db.QueryRow(`SELECT starting_commit FROM sessions WHERE id='codex:cx1'`).Scan(&start); err != nil || start != c[0] {
		t.Errorf("codex starting_commit = %q, %v; want %q", start, err, c[0])
	}
}

// A session launched from a subdirectory must store repo-root-relative paths,
// for both agents, whether the tool reported an absolute or a relative path.
func TestRunStoresRepoRootRelativePathsFromASubdirectory(t *testing.T) {
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")
	sub := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	claude := func(id, tool string, input map[string]any, ts string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "assistant", "sessionId": "sess-sub", "cwd": sub, "timestamp": ts,
			"message": map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": id, "name": tool, "input": input}}},
		})
		return string(b) + "\n"
	}
	cpath := filepath.Join(t.TempDir(), "c.jsonl")
	if err := os.WriteFile(cpath, []byte(
		claude("t1", "Edit", map[string]any{"file_path": filepath.Join(sub, "store.go")}, "2026-01-10T00:00:01Z")+
			claude("t2", "Read", map[string]any{"file_path": filepath.Join(repo, "README.md")}, "2026-01-10T00:00:02Z")), 0o644); err != nil {
		t.Fatal(err)
	}

	jl := func(typ, ts string, payload map[string]any) string {
		b, _ := json.Marshal(map[string]any{"timestamp": ts, "type": typ, "payload": payload})
		return string(b) + "\n"
	}
	xpath := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(xpath, []byte(
		jl("session_meta", "2026-01-10T00:00:00Z", map[string]any{"session_id": "cx-sub", "cwd": sub})+
			jl("response_item", "2026-01-10T00:00:01Z", map[string]any{
				"type": "custom_tool_call", "call_id": "k", "name": "apply_patch",
				"input": "*** Begin Patch\n*** Update File: store.go\n*** Update File: ../../cmd/main.go\n*** End Patch"})), 0o644); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t)
	if err := ingest.Run(db, claudecode.New(), cpath); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, codex.New(), xpath); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT session_id, path, rooted FROM file_touches ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var sid, p string
		var rooted int
		if err := rows.Scan(&sid, &p, &rooted); err != nil {
			t.Fatal(err)
		}
		if rooted != 1 {
			t.Errorf("%s %s: rooted = %d, want 1", sid, p, rooted)
		}
		got = append(got, sid+"="+p)
	}
	want := "claude-code:sess-sub=internal/store/store.go,claude-code:sess-sub=README.md," +
		"codex:cx-sub=internal/store/store.go,codex:cx-sub=cmd/main.go"
	if strings.Join(got, ",") != want {
		t.Errorf("paths = %v\nwant   %s", got, want)
	}
}

func claudeLineOn(session, cwd, branch, ts string) string {
	m := map[string]any{
		"type": "user", "sessionId": session, "cwd": cwd, "timestamp": ts,
		"message": map[string]any{"role": "user", "content": "hi " + ts},
	}
	if branch != "" {
		m["gitBranch"] = branch
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

// commit_source says how far to trust the span, and a span is only as good as
// its weakest end: it can worsen as lines arrive but never improve.
func TestRunRecordsHowMuchToTrustTheCommitSpan(t *testing.T) {
	db := openTestDB(t)
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z")
	nonGit := t.TempDir()

	files := map[string]string{
		"onbranch": claudeLineOn("s-branch", repo, "main", "2026-01-10T00:00:00Z") + claudeLineOn("s-branch", repo, "main", "2026-01-11T00:00:00Z"),
		"nobranch": claudeLineOn("s-head", repo, "", "2026-01-10T00:00:00Z"),
		"deleted":  claudeLineOn("s-deleted", repo, "was-deleted", "2026-01-10T00:00:00Z"),
		// branch first, then a line that lost it: must drop to "head"
		"worsens": claudeLineOn("s-worse", repo, "main", "2026-01-10T00:00:00Z") + claudeLineOn("s-worse", repo, "", "2026-01-11T00:00:00Z"),
		// head first, then a good line: must stay "head"
		"stays":  claudeLineOn("s-stays", repo, "", "2026-01-10T00:00:00Z") + claudeLineOn("s-stays", repo, "main", "2026-01-11T00:00:00Z"),
		"nongit": claudeLineOn("s-nongit", nonGit, "main", "2026-01-10T00:00:00Z"),
	}
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ingest.RunAll(db, func() ingest.Adapter { return claudecode.New() }, dir); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{
		"s-branch": "branch", "s-head": "head", "s-deleted": "head",
		"s-worse": "head", "s-stays": "head", "s-nongit": "",
	} {
		var got *string
		if err := db.QueryRow(`SELECT commit_source FROM sessions WHERE id = ?`, "claude-code:"+id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if g := deref(got); g != want {
			t.Errorf("%s: commit_source = %q, want %q", id, g, want)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Codex logs no branch, so its commits can only ever be read from HEAD's
// history and must be labelled approximate.
func TestRunLabelsCodexCommitsAsHeadResolved(t *testing.T) {
	db := openTestDB(t)
	repo, _ := gitRepo(t, "2026-01-01T00:00:00Z")
	body, _ := json.Marshal(map[string]any{"timestamp": "2026-01-10T00:00:00Z", "type": "session_meta",
		"payload": map[string]any{"session_id": "cx", "cwd": repo}})
	path := filepath.Join(t.TempDir(), "r.jsonl")
	os.WriteFile(path, append(body, '\n'), 0o644)
	if err := ingest.Run(db, codex.New(), path); err != nil {
		t.Fatal(err)
	}
	var got *string
	if err := db.QueryRow(`SELECT commit_source FROM sessions WHERE id='codex:cx'`).Scan(&got); err != nil || deref(got) != "head" {
		t.Errorf("commit_source = %q, %v; want head", deref(got), err)
	}
}
