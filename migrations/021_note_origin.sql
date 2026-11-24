-- Migration 021: let a note come from a file in the repository, and let a
-- local note be shared as one.
--
-- Each machine keeps its own catalogue, so a decision recorded on a laptop is
-- unknown on the server and to every teammate. `kae export-memory` writes
-- chosen notes to .kae/memory/*.md in the repository, where they travel with
-- the code and go through review; ingest reads those files back in on every
-- machine that has the checkout.
--
-- origin says which of the two a row is:
--   'local'  written on this machine (save_note, kae handoff). The default,
--            and what every existing row is.
--   'repo'   read from a file in a checkout. kae owns these rows: it rewrites
--            them when the file changes and deletes them when the file goes.
--
-- share_id is the stable identity written into the exported file. It is what
-- stops the note being counted twice on the machine that exported it: a file
-- whose id belongs to a local note there is that note, not a new one. On any
-- other machine it becomes a 'repo' row carrying the same id, so later edits
-- to the file find the row to update. Like the reference number on a
-- circulated memo: the author's copy and the copies in everyone else's files
-- are recognisably the same memo.
ALTER TABLE notes ADD COLUMN origin TEXT NOT NULL DEFAULT 'local'
    CHECK (origin IN ('local', 'repo'));
ALTER TABLE notes ADD COLUMN share_id TEXT;

-- One row per shared note, per origin, per project.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notes_share
    ON notes(project_id, share_id, origin) WHERE share_id IS NOT NULL;

INSERT OR IGNORE INTO schema_migrations(version, name)
VALUES (21, 'note_origin');
