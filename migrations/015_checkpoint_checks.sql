-- Migration 015: store every checkpoint check, not only the recommendations.
--
-- Until now a row existed only when checkpoint_status recommended a handoff.
-- That is a clinic that files a record only for patients it sends to hospital:
-- it can count referrals, but it cannot say how the patients it sent home got
-- on, so it cannot tell whether its referral rule is any good. Allowing the
-- level 'healthy' gives the comparison group: sessions ctx said to carry on
-- with, and what the user then did.
--
-- SQLite cannot alter a CHECK constraint, so the table is rebuilt. Nothing
-- references checkpoint_observations, so dropping the old table breaks no
-- foreign key. Every existing row is copied as it stands.
--
-- Two column meanings shift with this migration, without a rename (a rename
-- would break any query written against the old names):
--   * recommended_at is when the session was first assessed at its current
--     standing; for a healthy row no recommendation was made.
--   * duration_minutes holds active minutes (gaps over 30 minutes between
--     messages are not counted) for rows written from now on. Older rows keep
--     the started-to-ended span they were written with.
CREATE TABLE checkpoint_observations_new (
    session_id        TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    level             TEXT NOT NULL CHECK (level IN
                          ('healthy','checkpoint_recommended','checkpoint_strongly_recommended')),
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

INSERT INTO checkpoint_observations_new
    (session_id, project_id, level, message_count, duration_minutes, files_changed,
     reasons, outcome, note, recommended_at, resolved_at)
SELECT session_id, project_id, level, message_count, duration_minutes, files_changed,
       reasons, outcome, note, recommended_at, resolved_at
  FROM checkpoint_observations;

DROP TABLE checkpoint_observations;
ALTER TABLE checkpoint_observations_new RENAME TO checkpoint_observations;

-- The pending-decision index now skips healthy rows: a session told to carry
-- on has no decision waiting.
CREATE INDEX IF NOT EXISTS idx_checkpoint_observations_project_pending
    ON checkpoint_observations(project_id, recommended_at DESC)
    WHERE outcome IS NULL AND level <> 'healthy';

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (15, 'checkpoint_checks');
