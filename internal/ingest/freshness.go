// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// Root is one agent's session tree: where its logs live and how to get a
// fresh Adapter for a file in it (fresh per file; see RunAll).
type Root struct {
	Agent      string
	Dir        string
	NewAdapter func() Adapter
}

// Freshness says how far behind the agents' own logs the index is.
//
// Without it, a search over a stale index looks exactly like a search over
// a quiet week: "no sessions after October 1" reads as "nobody worked after
// October 1" when really nobody ran ingest. Like a claims office that stamps
// every answer with "post opened up to: Tuesday 09:00", so a caller can tell
// "no claim on file" from "we haven't opened that day's mail yet".
type Freshness struct {
	// LastIngestAt is the most recent time any source file was read, in
	// ISO-8601 UTC. Empty if nothing has ever been ingested.
	LastIngestAt string
	// PendingFiles counts log files that have content ctx hasn't read yet:
	// files it has never seen, or files that grew or shrank since it last
	// read them. Files written to within the quiet window are not counted
	// (see CheckFreshness).
	PendingFiles int
	// OldestPendingAt is the modification time of the longest-waiting
	// pending file, in ISO-8601 UTC: roughly how far back the gap reaches.
	OldestPendingAt string
}

// FreshnessQuiet is how long a log file must go unwritten before unread
// content in it counts as pending. The calling agent's own live session
// file is written on every turn, so without a quiet window the index would
// always look stale from inside any session, and the warning would be
// noise. Two minutes is long enough to skip a live session between turns'
// tool calls, and short enough that a real gap is still reported.
const FreshnessQuiet = 2 * time.Minute

// CheckFreshness compares each log file under roots with what the sources
// table says ctx has read from it. It only stats files and reads one query;
// it never opens a log file, so it is cheap enough to run on every search
// (about a thousand stat calls on a machine with months of sessions).
func CheckFreshness(db *sql.DB, roots []Root, now time.Time) (Freshness, error) {
	var f Freshness
	var last sql.NullString
	if err := db.QueryRow(`SELECT MAX(last_ingested_at) FROM sources`).Scan(&last); err != nil {
		return f, err
	}
	f.LastIngestAt = last.String

	offsets := map[string]int64{}
	rows, err := db.Query(`SELECT path, byte_offset FROM sources`)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var path string
		var off int64
		if err := rows.Scan(&path, &off); err != nil {
			rows.Close()
			return f, err
		}
		offsets[path] = off
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, err
	}

	cutoff := now.Add(-FreshnessQuiet)
	var oldest time.Time
	for _, root := range roots {
		err := filepath.WalkDir(root.Dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A directory that vanished mid-walk, or one we can't read:
				// skip it rather than fail the whole check. This is a hint,
				// not an audit.
				if path == root.Dir {
					return err // a missing root is handled below
				}
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if info.ModTime().After(cutoff) {
				return nil // still being written; see FreshnessQuiet
			}
			// byte_offset stops before an unfinished last line, so a file
			// whose writer died mid-line stays "pending" for good. Rare, and
			// over-reporting one file beats hiding a real gap.
			if off, ok := offsets[path]; ok && off == info.Size() {
				return nil
			}
			f.PendingFiles++
			if oldest.IsZero() || info.ModTime().Before(oldest) {
				oldest = info.ModTime()
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return f, fmt.Errorf("%s: %w", root.Agent, err)
		}
	}
	if !oldest.IsZero() {
		f.OldestPendingAt = oldest.UTC().Format(time.RFC3339)
	}
	return f, nil
}

// CatchUp runs RunAll over every root while holding the ingest lock (see
// Lock). Several `ctx mcp` servers are often running at once, one per open
// agent session, and each catches up at startup; the lock makes all but one
// of them, and any of them while `ctx daemon` runs, back off with
// ErrIngestBusy instead of reading the same logs at the same moment.
//
// Per-file failures don't stop the run; they are in the returned results
// (FileResult.Err), as with RunAll.
func CatchUp(db *sql.DB, roots []Root, lockPath string) ([]FileResult, error) {
	release, err := Lock(lockPath)
	if err != nil {
		return nil, err
	}
	defer release()

	var all []FileResult
	for _, root := range roots {
		results, err := RunAll(db, root.NewAdapter, root.Dir)
		all = append(all, results...)
		if err != nil {
			return all, fmt.Errorf("%s: %w", root.Agent, err)
		}
	}
	return all, nil
}
