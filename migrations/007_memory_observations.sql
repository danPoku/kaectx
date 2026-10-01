-- Migration 007: explicit outcomes for retrieved evidence.
--
-- Observations outlive the bounded retrieval telemetry that produced them.
-- The candidate foreign key therefore uses ON DELETE SET NULL while retaining
-- evidence_key, the stable session/message-range identity used for modelling.
CREATE TABLE IF NOT EXISTS memory_observations (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    evidence_key    TEXT NOT NULL,
    run_id          TEXT,
    candidate_rank  INTEGER,
    outcome         TEXT NOT NULL CHECK (outcome IN
                        ('helpful','unhelpful','stale','incorrect','superseded')),
    note            TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    FOREIGN KEY (run_id, candidate_rank)
        REFERENCES retrieval_candidates(run_id, rank) ON DELETE SET NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_memory_observations_evidence_time
    ON memory_observations(evidence_key, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_observations_project_time
    ON memory_observations(project_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_observations_one_per_candidate
    ON memory_observations(run_id, candidate_rank)
    WHERE run_id IS NOT NULL;

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (7, 'memory_observations');
