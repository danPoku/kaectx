-- Migration 013: keep every retrieval open as its own row.
--
-- retrieval_candidates.opened_at holds only the latest open: each open
-- overwrote it. That is still useful (feedback_status orders pending results
-- by it), so it stays, now defined as the last-open time. This table keeps the
-- history, so opens can be counted per day, per session or per kind.
--
-- inferred marks an open get_chunk attributed to a search from chunk_id alone.
-- backfilled marks the one row this migration writes per result already
-- opened before it: its time is that result's last open and it stands for one
-- or more real opens, so analysis of open events should leave it out.
CREATE TABLE IF NOT EXISTS retrieval_open_events (
    id          INTEGER PRIMARY KEY,
    run_id      TEXT NOT NULL,
    rank        INTEGER NOT NULL,
    opened_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    inferred    INTEGER NOT NULL DEFAULT 0 CHECK (inferred IN (0, 1)),
    backfilled  INTEGER NOT NULL DEFAULT 0 CHECK (backfilled IN (0, 1)),
    FOREIGN KEY (run_id, rank)
        REFERENCES retrieval_candidates(run_id, rank) ON DELETE CASCADE
) STRICT;

CREATE INDEX IF NOT EXISTS idx_retrieval_open_events_candidate
    ON retrieval_open_events(run_id, rank);
CREATE INDEX IF NOT EXISTS idx_retrieval_open_events_time
    ON retrieval_open_events(opened_at);

INSERT INTO retrieval_open_events(run_id, rank, opened_at, inferred, backfilled)
SELECT run_id, rank, opened_at,
       inferred_open_count > 0 AND inferred_open_count = open_count, 1
  FROM retrieval_candidates
 WHERE opened_at IS NOT NULL;

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (13, 'retrieval_open_events');
