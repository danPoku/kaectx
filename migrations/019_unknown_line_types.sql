-- Migration 019: count the log lines an adapter skipped because it did not
-- recognise their type.
--
-- Agent log formats are undocumented and change without notice. Until now an
-- adapter that met a new line type skipped it in silence, so a format change
-- showed up only as "kae seems to remember less lately". This table is the
-- post room's tally of envelopes it could not sort: not their contents, just
-- how many, of what kind, from which sender, and since when. `kae doctor`
-- reads it, and so does the MCP `index` warning.
--
-- One row per source file, session, line type and agent version. Keeping the
-- source makes the count correctable: when a log file is rewritten and
-- re-read from the start, its rows are deleted and counted again, instead of
-- every line being counted twice. Keeping the session ties a type to a
-- project, so an agent is warned only about its own project's logs.
--
-- Nothing is backfilled. Lines skipped before this migration were not
-- recorded anywhere; the counts start with the next ingest.
CREATE TABLE IF NOT EXISTS unknown_line_types (
    source_id      INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    agent          TEXT    NOT NULL,
    -- '<agent>:<native_id>' as in sessions.id, or '' when the line named no
    -- session. Not a foreign key: the line can be read before the session's
    -- first storable line creates its row.
    session_id     TEXT    NOT NULL DEFAULT '',
    -- The type as the agent wrote it. A nested type is written with its
    -- parent, e.g. 'response_item/function_call' or 'content/document'.
    line_type      TEXT    NOT NULL,
    -- The agent version that wrote the lines, '' when the log does not say.
    agent_version  TEXT    NOT NULL DEFAULT '',
    line_count     INTEGER NOT NULL CHECK (line_count > 0),
    -- Times of the first and last such line: the log line's own timestamp
    -- when it has one, otherwise when kae read it.
    first_seen_at  TEXT    NOT NULL,
    last_seen_at   TEXT    NOT NULL,
    PRIMARY KEY (source_id, session_id, line_type, agent_version)
) STRICT;

CREATE INDEX IF NOT EXISTS unknown_line_types_last_seen ON unknown_line_types(last_seen_at);

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (19, 'unknown_line_types');
