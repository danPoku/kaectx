// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/danPoku/kaectx/internal/ingest"
)

// catchUpCooldown is the shortest gap between two background catch-ups
// started by the same server. A search that finds pending files starts
// one; without a cooldown, an agent firing five searches in a row would
// start five.
const catchUpCooldown = 2 * time.Minute

// indexStatus is attached to search_context and recent_sessions results so
// an agent can tell "nothing happened" from "ctx hasn't read it yet". It is
// omitted entirely when the server wasn't given any roots to check (tests,
// or an embedding of the server that doesn't own ingest).
type indexStatus struct {
	LastIngestAt    string `json:"last_ingest_at,omitempty"`
	PendingFiles    int    `json:"pending_files"`
	OldestPendingAt string `json:"oldest_pending_at,omitempty"`
	CatchingUp      bool   `json:"catching_up,omitempty"`
	// Warning is a ready-to-relay sentence, set only when results may be
	// missing recent work.
	Warning string `json:"warning,omitempty"`
}

// catchUp is the server's background-ingest state. A `ctx mcp` server runs
// for as long as the agent session that spawned it, so it is the one ctx
// process guaranteed to be alive whenever an agent asks a question. If no
// `ctx daemon` is running, nothing else keeps the index current; this
// lets the server close that gap itself instead of answering from a
// days-old index.
type catchUp struct {
	roots    []ingest.Root
	lockPath string

	mu      sync.Mutex
	running bool
	lastRun time.Time
	// wg tracks the background goroutine so tests (and a clean shutdown)
	// can wait for it rather than race it.
	wg sync.WaitGroup
}

// start launches a background catch-up unless one is already running or
// one started within catchUpCooldown. It reports whether a catch-up is now
// in progress, whether or not this call started it.
func (s *server) startCatchUp() bool {
	c := s.catchUp
	if c == nil || len(c.roots) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return true
	}
	if !c.lastRun.IsZero() && time.Since(c.lastRun) < catchUpCooldown {
		return false
	}
	c.running = true
	c.lastRun = time.Now()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		results, err := ingest.CatchUp(s.db, c.roots, c.lockPath)
		// stdout is the MCP transport, so diagnostics go to stderr, which
		// MCP clients capture into their server logs. Another process
		// holding the lock is the normal case with several sessions open
		// or the daemon running, not worth a log line.
		if err != nil && !errors.Is(err, ingest.ErrIngestBusy) {
			fmt.Fprintf(os.Stderr, "ctx mcp: background catch-up: %v\n", err)
		}
		logFileFailures(results)
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	return true
}

// logFileFailures reports files a catch-up could not ingest (unreadable,
// or a database error mid-file). Parse errors on single lines are not here:
// those are skipped and recorded in sources.last_error, as in every ingest.
// The count plus a few examples is enough to notice; the rest would only
// flood the client's log.
func logFileFailures(results []ingest.FileResult) {
	const show = 3
	failed := 0
	for _, r := range results {
		if r.Err == nil {
			continue
		}
		if failed < show {
			fmt.Fprintf(os.Stderr, "ctx mcp: catch-up: %s: %v\n", r.Path, r.Err)
		}
		failed++
	}
	if failed > show {
		fmt.Fprintf(os.Stderr, "ctx mcp: catch-up: %d more file(s) failed\n", failed-show)
	}
}

// indexStatus checks freshness and, if the index is behind, starts a
// background catch-up. The current call still answers from what is indexed
// now (waiting would make every search as slow as a full ingest); the
// warning tells the agent to retry shortly.
//
// It fails open: a freshness check that errors returns nil, because the
// status is advice and must never turn a good search into a failed one.
func (s *server) indexStatus() *indexStatus {
	if s.catchUp == nil || len(s.catchUp.roots) == 0 {
		return nil
	}
	f, err := ingest.CheckFreshness(s.db, s.catchUp.roots, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ctx mcp: freshness check: %v\n", err)
		return nil
	}
	st := &indexStatus{LastIngestAt: f.LastIngestAt, PendingFiles: f.PendingFiles, OldestPendingAt: f.OldestPendingAt}
	if f.PendingFiles == 0 {
		return st
	}
	st.CatchingUp = s.startCatchUp()
	st.Warning = fmt.Sprintf("ctx has %d session log file(s) it hasn't read yet (oldest from %s), so these results may be missing recent work.",
		f.PendingFiles, f.OldestPendingAt)
	if st.CatchingUp {
		st.Warning += " A background catch-up is running; retry in a minute."
	}
	st.Warning += " Run `ctx daemon` to keep the index current."
	return st
}
