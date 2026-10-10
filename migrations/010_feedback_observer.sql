-- Migration 010: checkpoint decisions and the feedback observer.
--
-- Retrieval nudges are derived from retrieval_candidates and
-- memory_observations. Only checkpoint recommendations need durable state:
-- one row per assessed session, updated as its metrics change and resolved
-- when the user chooses what to do.
CREATE TABLE IF NOT EXISTS checkpoint_observations (
    session_id        TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    level             TEXT NOT NULL CHECK (level IN
                          ('checkpoint_recommended','checkpoint_strongly_recommended')),
    message_count     INTEGER NOT NULL CHECK (message_count >= 0),
    duration_minutes  INTEGER NOT NULL CHECK (duration_minutes >= 0),
    files_changed     INTEGER NOT NULL CHECK (files_changed >= 0),
    reasons           TEXT NOT NULL DEFAULT '[]'
                      CHECK (json_valid(reasons) AND json_type(reasons) = 'array'),
    outcome           TEXT CHECK (outcome IS NULL OR outcome IN
                          ('started_new_session','saved_handoff','continued','dismissed')),
    note              TEXT,
    recommended_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    resolved_at       TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_checkpoint_observations_project_pending
    ON checkpoint_observations(project_id, recommended_at DESC)
    WHERE outcome IS NULL;

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (10, 'feedback_observer');
