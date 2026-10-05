// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrIngestBusy means another process holds the ingest lock.
var ErrIngestBusy = errors.New("another ctx process is already ingesting (ctx daemon, ctx ingest, or an MCP server catching up)")

// Lock takes the machine-wide ingest lock at lockPath without waiting, and
// returns a function that releases it.
//
// Every ingest entry point takes it: `ctx daemon` for its whole life,
// `ctx ingest` for one run, and each MCP server's catch-up. Ingest is
// idempotent for ONE reader, but two processes reading the same log at the
// same moment both start from the same byte offset and number the same
// lines from a MAX(seq) that is moving under them, which stores those lines
// twice. Like two clerks working one in-tray: either one alone files every
// letter once, together they file some twice.
//
// It is an operating-system lock (flock on Unix, LockFileEx on Windows),
// not a "this file exists" marker, so the OS releases it the moment its
// holder exits or crashes. There is no stale lock to detect and no
// takeover race. The lock file itself is left in place; only the lock on
// it comes and goes.
func Lock(lockPath string) (release func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open ingest lock: %w", err)
	}
	ok, err := tryLockFile(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !ok {
		f.Close()
		return nil, ErrIngestBusy
	}
	return func() {
		unlockFile(f)
		f.Close()
	}, nil
}

// LockWait is Lock, retrying every poll until it succeeds or ctx ends. For
// the daemon, which must eventually own ingest even if it starts while an
// MCP server is midway through a catch-up.
func LockWait(ctx context.Context, lockPath string, poll time.Duration) (func(), error) {
	for {
		release, err := Lock(lockPath)
		if !errors.Is(err, ErrIngestBusy) {
			return release, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}
