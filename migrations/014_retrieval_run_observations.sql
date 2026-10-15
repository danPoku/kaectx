-- Migration 014: outcomes for a whole search.
--
-- memory_observations judges one exchange: was this evidence right? A search
-- can return only correct exchanges and still miss half the story, because the
-- answer sat in another project, had not been ingested, had been overtaken by
-- later work, or ranked too low. Like a claims audit that reviews the file as a
-- whole rather than each receipt in it, this table records that verdict and the
-- reason, so the reasons can be counted to decide which gap to close.
--
-- governance never reads this table. A run's verdict says nothing about any one
-- exchange's reliability, and folding it into credibility would charge good
-- evidence for coverage it was never responsible for.
--
-- Like memory_observations, a verdict outlives the bounded telemetry it came
-- from: pruning the run sets run_id to NULL rather than deleting the verdict.
CREATE TABLE IF NOT EXISTS retrieval_run_observations (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    run_id      TEXT REFERENCES retrieval_runs(id) ON DELETE SET NULL,
    outcome     TEXT NOT NULL CHECK (outcome IN ('helpful','partial','unhelpful')),
    reason      TEXT CHECK (reason IS NULL OR reason IN
                    ('wrong_project','not_indexed','stale_state','ranking')),
    note        TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    -- A search that fully answered the question has no gap to name.
    CHECK (outcome <> 'helpful' OR reason IS NULL)
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_retrieval_run_observations_one_per_run
    ON retrieval_run_observations(run_id)
    WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_retrieval_run_observations_project_time
    ON retrieval_run_observations(project_id, created_at DESC);

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (14, 'retrieval_run_observations');
