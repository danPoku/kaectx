// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"database/sql"
	"time"
)

// Drain calls Run repeatedly until no pending chunk is left to embed and
// returns how many it embedded in total. On an error it returns the count so
// far along with the error, so callers can report partial progress.
//
// Like emptying an inbox tray: keep taking a batch until the tray is empty.
// Chunks Run skips (a session with no project) stay pending but count as zero
// processed, so they end the loop instead of spinning it forever.
func Drain(ctx context.Context, db *sql.DB, client Embedder, batchSize int) (int, error) {
	total := 0
	for {
		n, err := Run(ctx, db, client, batchSize)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
}

// RunEvery drains the pending-chunk queue immediately and then again every
// interval until ctx is cancelled. It is meant to run as a daemon goroutine
// next to the ingest loop, so freshly ingested chunks become searchable
// without anyone remembering to run `kae embed`.
//
// An error (Ollama not running, model not pulled) is reported through logf
// and retried on the next tick rather than stopping the loop: the embedder is
// an optional service that can come and go, unlike the store the daemon
// cannot live without. The same error repeating is logged only once, so an
// Ollama that stays down for a day does not write a line every tick; recovery
// is logged when it comes back.
func RunEvery(ctx context.Context, db *sql.DB, client Embedder, batchSize int, interval time.Duration, logf func(format string, args ...any)) {
	var lastErr string
	tick := func() {
		n, err := Drain(ctx, db, client, batchSize)
		if n > 0 {
			logf("kae daemon: embedded %d chunk(s)", n)
		}
		switch {
		case ctx.Err() != nil:
			// Shutting down: a cancelled request is not a real failure.
		case err != nil:
			if msg := err.Error(); msg != lastErr {
				logf("kae daemon: embedding paused, will retry every %s: %v", interval, err)
				lastErr = msg
			}
		case lastErr != "":
			logf("kae daemon: embedding recovered")
			lastErr = ""
		}
	}

	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
