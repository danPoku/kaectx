// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
)

func claudeRoot(dir string) []ingest.Root {
	return []ingest.Root{{Agent: "claude-code", Dir: dir, NewAdapter: func() ingest.Adapter { return claudecode.New() }}}
}

// writeAged writes a log file and backdates it past the quiet window, as a
// session that finished a while ago would be.
func writeAged(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func mustRun(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	if err := ingest.Run(db, claudecode.New(), path); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCheckFreshnessCountsOnlySettledUnreadFiles(t *testing.T) {
	fixture, err := os.ReadFile("claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		setup       func(t *testing.T, db *sql.DB, dir string)
		wantPending int
	}{
		{
			name: "file never ingested is pending",
			setup: func(t *testing.T, _ *sql.DB, dir string) {
				writeAged(t, filepath.Join(dir, "p", "a.jsonl"), fixture)
			},
			wantPending: 1,
		},
		{
			name: "fully ingested file is not pending",
			setup: func(t *testing.T, db *sql.DB, dir string) {
				path := filepath.Join(dir, "p", "a.jsonl")
				writeAged(t, path, fixture)
				mustRun(t, db, path)
			},
			wantPending: 0,
		},
		{
			name: "file that grew after ingest is pending",
			setup: func(t *testing.T, db *sql.DB, dir string) {
				path := filepath.Join(dir, "p", "a.jsonl")
				writeAged(t, path, fixture)
				mustRun(t, db, path)
				writeAged(t, path, append(append([]byte{}, fixture...), fixture...))
			},
			wantPending: 1,
		},
		{
			// The calling agent's own live session: written seconds ago,
			// unread, and deliberately not reported.
			name: "file still being written is not pending",
			setup: func(t *testing.T, _ *sql.DB, dir string) {
				path := filepath.Join(dir, "p", "live.jsonl")
				os.MkdirAll(filepath.Dir(path), 0755)
				if err := os.WriteFile(path, fixture, 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantPending: 0,
		},
		{
			name:        "missing root is not an error",
			setup:       func(t *testing.T, _ *sql.DB, dir string) { os.RemoveAll(dir) },
			wantPending: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openTestDB(t)
			dir := filepath.Join(t.TempDir(), "projects")
			c.setup(t, db, dir)

			f, err := ingest.CheckFreshness(db, claudeRoot(dir), time.Now())
			if err != nil {
				t.Fatalf("CheckFreshness: %v", err)
			}
			if f.PendingFiles != c.wantPending {
				t.Errorf("PendingFiles = %d, want %d", f.PendingFiles, c.wantPending)
			}
			if (f.OldestPendingAt != "") != (c.wantPending > 0) {
				t.Errorf("OldestPendingAt = %q with %d pending", f.OldestPendingAt, f.PendingFiles)
			}
		})
	}
}

func TestCatchUpIngestsPendingFilesAndClearsTheGap(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	fixture, err := os.ReadFile("claudecode/testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	writeAged(t, filepath.Join(dir, "p", "a.jsonl"), fixture)
	lock := filepath.Join(t.TempDir(), "ingest.lock")

	if _, err := ingest.CatchUp(db, claudeRoot(dir), lock); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	f, err := ingest.CheckFreshness(db, claudeRoot(dir), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.PendingFiles != 0 || f.LastIngestAt == "" {
		t.Errorf("after catch-up: %+v, want nothing pending and a last-ingest time", f)
	}
	release, err := ingest.Lock(lock)
	if err != nil {
		t.Fatalf("lock still held after catch-up returned: %v", err)
	}
	release()
}

func TestIngestLockAdmitsOneHolderAtATime(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	lock := filepath.Join(t.TempDir(), "ingest.lock")

	// The daemon (or another MCP server) holds the lock: back off.
	release, err := ingest.Lock(lock)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := ingest.CatchUp(db, claudeRoot(dir), lock); !errors.Is(err, ingest.ErrIngestBusy) {
		t.Fatalf("CatchUp while the lock is held: err = %v, want ErrIngestBusy", err)
	}
	if _, err := ingest.Lock(lock); !errors.Is(err, ingest.ErrIngestBusy) {
		t.Fatalf("second Lock while held: err = %v, want ErrIngestBusy", err)
	}

	// Released (or its holder exited): the next one gets it.
	release()
	if _, err := ingest.CatchUp(db, claudeRoot(dir), lock); err != nil {
		t.Fatalf("CatchUp after release: %v", err)
	}
}

func TestLockWaitGetsTheLockOnceReleased(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "ingest.lock")
	release, err := ingest.Lock(lock)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := ingest.LockWait(ctx, lock, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("LockWait: %v", err)
	}
	got()
}
