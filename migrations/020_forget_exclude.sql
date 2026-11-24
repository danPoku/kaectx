-- Migration 020: let a person remove a session for good, and keep a
-- repository out of the catalogue.
--
-- kae reads everything the agents write. Until now nothing could be taken
-- back: a session with something private in it stayed searchable, and a
-- repository could not be kept out. People will not trust a memory they
-- cannot correct.
--
-- Deleting a session's rows is not enough on its own, because the agent's
-- log file is still on disk and the next ingest would read it straight back
-- in. forgotten_sessions is the "do not readmit" list at the door: ingest
-- checks it before storing a line, so a forgotten session stays forgotten
-- after `kae ingest`, a daemon rescan, or the log file being rewritten.
--
-- It is keyed by session id ('<agent>:<native_id>'), not by log file path: a
-- file can be moved or rewritten, and one file can hold lines of a session
-- that also has lines elsewhere (a subagent transcript), but the id the agent
-- gave the session does not change.
CREATE TABLE IF NOT EXISTS forgotten_sessions (
    session_id    TEXT PRIMARY KEY,
    agent         TEXT NOT NULL,
    forgotten_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
) STRICT;

-- NULL means the project is ingested as usual. A time means it was excluded
-- then: ingest stores nothing more for it, neither new sessions nor new lines
-- of sessions it already holds. What was stored before stays until it is
-- forgotten; excluding is "stop collecting", not "erase".
ALTER TABLE projects ADD COLUMN excluded_at TEXT;

-- 1 when ingest passed over lines of this log file because their project was
-- excluded. Ingest still advances its bookmark past such lines (otherwise it
-- would re-read the file on every run), so if the project is included again
-- kae needs to know which files to go back over. Including a project sends
-- every flagged file back to the start; see store.SetProjectExcluded.
ALTER TABLE sources ADD COLUMN skipped_excluded INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (20, 'forget_exclude');
