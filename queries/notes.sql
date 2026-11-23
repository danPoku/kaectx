-- =============================================================================
-- Named queries for writing notes — see queries/retrieval.sql for the
-- reading side (SearchNotes) and the format/scoping conventions.
-- =============================================================================

-- name: SaveNote :exec
INSERT INTO notes(project_id, session_id, agent, kind, title, body, tags)
VALUES (:project_id, :session_id, :agent, :kind, :title, :body, :tags);


-- name: NoteSupersedeState :one
-- What save_note needs to know about the note it is asked to retire: whose it
-- is, and whether something has already replaced it.
SELECT project_id, superseded_by
  FROM notes
 WHERE id = :id;


-- name: SupersedeNote :exec
-- Retire a live note by pointing it at its replacement. The superseded_by IS
-- NULL guard makes this a no-op on a note that was retired in the meantime;
-- the caller checks the affected-row count and rolls back if it is zero.
UPDATE notes
   SET superseded_by = :superseded_by,
       updated_at    = strftime('%Y-%m-%dT%H:%M:%SZ','now')
 WHERE id = :id
   AND project_id = :project_id
   AND superseded_by IS NULL;
