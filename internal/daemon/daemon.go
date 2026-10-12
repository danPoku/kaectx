// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package daemon runs continuous ingestion: an initial catch-up pass over
// each source's whole tree, then live incremental re-ingestion as session
// files change, watched via fsnotify.
package daemon

import (
	"context"
	"database/sql"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/danPoku/kaectx/internal/ingest"
)

// Source is one agent's session tree: where it lives, and how to get a
// fresh Adapter for it. Mirrors cmd/ctx's agentSource — kept as its own
// type here so this package doesn't depend on cmd/ctx's path-resolution
// concerns (home directory, per-agent subpaths), only on "here's a root and
// an adapter factory".
type Source struct {
	Agent      string
	Root       string
	NewAdapter func() ingest.Adapter
}

// Run performs an initial catch-up ingest across every source, then blocks,
// watching each source's directory tree for live changes and incrementally
// re-ingesting touched files. A periodic fallback re-scan (every
// fallbackInterval) catches anything fsnotify missed — filesystem event
// delivery isn't perfectly reliable on every platform WSL might run this
// under, so this isn't just defensive theater — and is also what notices a
// source root that didn't exist yet at startup (an agent used for the
// first time after the daemon started) and starts watching it.
//
// debounceDelay is how long a .jsonl file must go quiet before it's
// re-ingested — a real writer appends line by line, and re-running ingest
// on every single Write event would be wasteful. Exposed as a parameter
// (rather than a hardcoded constant) mainly so tests aren't stuck waiting
// out a production-sized delay.
// afterInitialScan runs once the startup scan finishes. It does nothing in
// production; a test sets it to write at exactly the moment a startup race
// would lose the write.
var afterInitialScan = func() {}

func Run(ctx context.Context, db *sql.DB, sources []Source, fallbackInterval, debounceDelay time.Duration) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	// Watch first, then scan. The other order left a gap: a line appended
	// after the scan read its file but before the watch was in place raised
	// no event and waited for the fallback tick, up to an hour. With the
	// watch already up, such a write raises an event; if the scan also saw
	// it, ingest's byte offsets make the second read a no-op.
	for _, src := range sources {
		addWatchesRecursive(watcher, src.Root)
	}
	for _, src := range sources {
		runAllLogged(db, src)
	}
	afterInitialScan()

	debounce := newDebouncer(debounceDelay)
	defer debounce.stop()

	fallback := time.NewTicker(fallbackInterval)
	defer fallback.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			handleEvent(db, sources, watcher, debounce, event)

		case path := <-debounce.ready:
			if src, ok := sourceFor(sources, path); ok {
				if err := ingest.Run(db, src.NewAdapter(), path); err != nil {
					log.Printf("ctx daemon: ingest %s: %v", path, err)
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("ctx daemon: watcher error: %v", err)

		case <-fallback.C:
			for _, src := range sources {
				runAllLogged(db, src)
				// Re-attempting a watch on a root that appeared since the
				// last tick (or since startup) is how a source that didn't
				// exist yet gets picked up — Add is harmless to repeat on
				// an already-watched path.
				addWatchesRecursive(watcher, src.Root)
			}
		}
	}
}

func runAllLogged(db *sql.DB, src Source) {
	results, err := ingest.RunAll(db, src.NewAdapter, src.Root)
	if err != nil {
		log.Printf("ctx daemon: scan %s (%s): %v", src.Root, src.Agent, err)
		return
	}
	for _, r := range results {
		if r.Err != nil {
			log.Printf("ctx daemon: ingest %s: %v", r.Path, r.Err)
		}
	}
}

// addWatchesRecursive watches root and every existing subdirectory beneath
// it. A root that doesn't exist yet is silently skipped — not an error,
// since the daemon may start before an agent has ever been used; the
// periodic fallback tick retries this until the directory appears.
func addWatchesRecursive(watcher *fsnotify.Watcher, root string) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // missing root/subdir: nothing to watch yet, not fatal
		}
		if d.IsDir() {
			if err := watcher.Add(path); err != nil {
				log.Printf("ctx daemon: watch %s: %v", path, err)
			}
		}
		return nil
	})
}

// handleEvent reacts to one fsnotify event: a newly created directory gets
// watched immediately (this is how Claude Code's per-project directories
// and Codex's date-nested directories get picked up as they're created,
// without waiting for a fallback tick) AND catch-up-scanned right away —
// there's an inherent race between "directory created" and "watch
// established on it" during which a file written immediately inside it
// (exactly what happens in practice: mkdir, then write the session file)
// could otherwise be missed until the next fallback tick. A write or
// create on a .jsonl file schedules a debounced ingest.
func handleEvent(db *sql.DB, sources []Source, watcher *fsnotify.Watcher, debounce *debouncer, event fsnotify.Event) {
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			addWatchesRecursive(watcher, event.Name)
			if src, ok := sourceFor(sources, event.Name); ok {
				runAllLogged(db, Source{Agent: src.Agent, Root: event.Name, NewAdapter: src.NewAdapter})
			}
			return
		}
	}
	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
		return
	}
	if !strings.HasSuffix(event.Name, ".jsonl") {
		return
	}
	debounce.schedule(event.Name)
}

func sourceFor(sources []Source, path string) (Source, bool) {
	for _, src := range sources {
		if strings.HasPrefix(path, src.Root+string(filepath.Separator)) {
			return src, true
		}
	}
	return Source{}, false
}
