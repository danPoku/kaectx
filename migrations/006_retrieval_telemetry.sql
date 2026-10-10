-- Migration 006: retrieval telemetry.
--
-- These tables are deliberately append-only observations. Search must remain
-- useful if recording fails, and future ranking experiments need the original
-- result order rather than a mutable "current" score.
CREATE TABLE IF NOT EXISTS retrieval_runs (
    id               TEXT PRIMARY KEY,
    project_id       INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    surface          TEXT NOT NULL CHECK (surface IN ('cli', 'mcp', 'internal')),
    query            TEXT NOT NULL,
    agent_filter     TEXT,
    requested_limit  INTEGER NOT NULL CHECK (requested_limit > 0),
    mode             TEXT NOT NULL CHECK (mode IN ('keyword', 'hybrid')),
    fallback_reason  TEXT,
    duration_ms      INTEGER NOT NULL CHECK (duration_ms >= 0),
    result_count     INTEGER NOT NULL CHECK (result_count >= 0),
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
) STRICT;

CREATE INDEX IF NOT EXISTS idx_retrieval_runs_project_time
    ON retrieval_runs(project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS retrieval_candidates (
    run_id          TEXT NOT NULL REFERENCES retrieval_runs(id) ON DELETE CASCADE,
    rank            INTEGER NOT NULL CHECK (rank > 0),
    evidence_key    TEXT NOT NULL,
    chunk_id        INTEGER REFERENCES chunks(id) ON DELETE SET NULL,
    session_id      TEXT NOT NULL,
    first_seq       INTEGER NOT NULL,
    last_seq        INTEGER NOT NULL,
    score           REAL NOT NULL,
    score_kind      TEXT NOT NULL CHECK (score_kind IN ('bm25', 'rrf')),
    PRIMARY KEY (run_id, rank)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_retrieval_candidates_evidence
    ON retrieval_candidates(evidence_key);

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (6, 'retrieval_telemetry');
