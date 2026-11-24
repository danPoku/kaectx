// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn and returns what it printed to standard output.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	runErr := fn()
	w.Close()
	os.Stdout = old
	out := <-done
	if runErr != nil {
		t.Fatalf("command failed: %v\n%s", runErr, out)
	}
	return out
}

// seedAgentLogs puts the Claude Code fixture session where the adapter looks
// for logs under home, as if the agent had been used before kae was installed.
func seedAgentLogs(t *testing.T, home string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("..", "ingest", "claudecode", "testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude", "projects", "-home-dev-proj")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess-1.jsonl"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFirstIngestPrintsSummaryOnce(t *testing.T) {
	home := useHome(t)
	seedAgentLogs(t, home)

	first := captureStdout(t, func() error { return runIngest(nil) })
	for _, want := range []string{"ingested 1 file(s), 0 failed", "kae found 1 session across 1 project", "claude-code  1 session", "Secrets redacted before storing:", "kae search \""} {
		if !strings.Contains(first, want) {
			t.Errorf("first ingest output lacks %q:\n%s", want, first)
		}
	}
	// The suggestion comes with a result, so the user is never told to run
	// a search that finds nothing.
	if !strings.Contains(first, "Its top result:") || !strings.Contains(first, "[claude-code]") {
		t.Errorf("the suggested search shows no hit:\n%s", first)
	}

	// The next ingest prints the usual single line.
	second := captureStdout(t, func() error { return runIngest(nil) })
	if strings.Contains(second, "kae found") || !strings.Contains(second, "ingested") {
		t.Errorf("second ingest should print only the usual line:\n%s", second)
	}
}

func TestFirstIngestQuietAndEmpty(t *testing.T) {
	t.Run("--quiet suppresses the summary", func(t *testing.T) {
		home := useHome(t)
		seedAgentLogs(t, home)
		out := captureStdout(t, func() error { return runIngest([]string{"--quiet"}) })
		if strings.Contains(out, "kae found") || !strings.Contains(out, "ingested 1 file(s)") {
			t.Errorf("unexpected output:\n%s", out)
		}
	})
	t.Run("nothing found prints no summary", func(t *testing.T) {
		useHome(t)
		out := captureStdout(t, func() error { return runIngest(nil) })
		if strings.Contains(out, "kae found") {
			t.Errorf("an ingest that found no sessions should not summarise:\n%s", out)
		}
	})
}

// For most people setup is the first ingest, so it ends with the same report.
func TestSetupIngestStepCarriesTheSummary(t *testing.T) {
	home := useHome(t)
	seedAgentLogs(t, home)
	line, err := firstIngest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "kae found 1 session") {
		t.Errorf("setup's first ingest lacks the summary:\n%s", line)
	}
	again, err := firstIngest()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again, "kae found") {
		t.Errorf("a later setup run should not repeat it:\n%s", again)
	}
}
