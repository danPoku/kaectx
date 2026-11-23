-- Migration 016: record note searches, and which note a handoff was saved as.
--
-- A checkpoint exists so that a later session can pick up where this one left
-- off. Until now ctx could count handoffs saved but not handoffs read: only
-- search_context left a record, and a checkpoint row did not say which note
-- held its handoff. Like a left-luggage office that issues tickets but never
-- logs a collection, it could not tell a bag that was fetched from one that
-- was abandoned.
--
-- note_retrievals is that collection log: one row per note a search_notes
-- call returned, in rank order. A call that returned nothing writes no rows.
-- It is deliberately small: no query text, no project (the note carries its
-- project), no labels. Rows go when their note does.
CREATE TABLE IF NOT EXISTS note_retrievals (
    run_id      TEXT    NOT NULL,                 -- one per search_notes call
    rank        INTEGER NOT NULL CHECK (rank >= 1),
    note_id     INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (run_id, rank)
) STRICT;

-- "Was this note returned after it was saved?" looks rows up by note and time.
CREATE INDEX IF NOT EXISTS idx_note_retrievals_note_time
    ON note_retrievals(note_id, created_at);

-- The note a saved_handoff outcome was saved as. NULL for every other outcome,
-- for handoffs recorded before this column existed, and when the agent did
-- not say. If the note is deleted the outcome stands; only the link goes.
ALTER TABLE checkpoint_observations
    ADD COLUMN note_id INTEGER REFERENCES notes(id) ON DELETE SET NULL;

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (16, 'note_retrievals');
