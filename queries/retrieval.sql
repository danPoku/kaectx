-- =============================================================================
-- Named queries — one per MCP tool / CLI subcommand.
-- Format is sqlc-compatible (-- name: X :many), so Go code can be generated
-- from these, but they're plain SQL you can paste into the sqlite3 shell.
--
-- Scoping rule used everywhere:
--   :project_id  -> required for project scope; the app omits it for cross-project
--   :agent       -> NULL = cross-agent (everyone's files), 'codex' = within-agent
-- =============================================================================


-- name: SearchKeyword :many
-- BM25 keyword search. Great for exact things: error strings, function names,
-- ticket ids. FTS5's `rank` is lower-is-better, so ASC is the right order.
-- snippet() returns a ~24-token window with the hit bracketed — the
-- highlighted line on the index card, not the whole card.
SELECT c.id                                       AS chunk_id,
       c.session_id,
       s.agent,
       s.started_at,
       snippet(chunks_fts, 0, '[', ']', ' … ', 24) AS snippet,
       chunks_fts.rank                             AS bm25,
       c.first_seq,
       c.last_seq,
       COALESCE((SELECT MIN(m.created_at) FROM messages m
                 WHERE m.session_id = c.session_id
                   AND m.seq BETWEEN c.first_seq AND c.last_seq),
                s.started_at)
                                                   AS chunk_at   -- when THIS turn happened; started_at is the session's start
  FROM chunks_fts
  JOIN chunks   c ON c.id = chunks_fts.rowid
  JOIN sessions s ON s.id = c.session_id
 WHERE chunks_fts MATCH :query
   AND s.project_id = :project_id
   AND (:agent IS NULL OR s.agent = :agent)
 ORDER BY chunks_fts.rank
 LIMIT :limit;


-- name: SearchHybrid :many
-- Reciprocal Rank Fusion of keyword + semantic results.
--
-- Analogy: two assessors each rank the same 50 claims. Their raw scores are on
-- different scales (BM25 vs cosine distance), so we don't average scores —
-- we average *positions*. A claim ranked 1st by one assessor and 3rd by the
-- other beats one ranked 1st by only one of them. The constant 60 damps the
-- advantage of being exactly 1st vs 2nd; it's the standard value from the
-- original RRF paper and rarely worth tuning.
--
-- NOTE: vec0 wants its filters as plain equality constraints, so the app
-- builds two variants of the `sem` CTE: with and without `AND agent = :agent`.
-- This file shows the cross-agent, project-scoped variant.
WITH kw AS (
    SELECT c.id AS chunk_id,
           row_number() OVER (ORDER BY chunks_fts.rank) AS r
      FROM chunks_fts
      JOIN chunks   c ON c.id = chunks_fts.rowid
      JOIN sessions s ON s.id = c.session_id
     WHERE chunks_fts MATCH :query
       AND s.project_id = :project_id
     ORDER BY chunks_fts.rank
     LIMIT 50
),
sem AS (
    -- KNN always returns its k nearest rows, however unrelated, so a query
    -- with one true match would drag 49 strangers into the fusion. The outer
    -- filter drops anything farther than :max_distance (cosine distance;
    -- see search.MaxSemanticDistance) so only real semantic neighbours count.
    SELECT chunk_id,
           row_number() OVER (ORDER BY distance) AS r
      FROM (SELECT chunk_id, distance
              FROM chunk_vectors
             WHERE embedding MATCH :query_embedding   -- JSON array or float32 blob
               AND k = 50
               AND project_id = :project_id)
     WHERE distance <= :max_distance
),
fused AS (
    SELECT chunk_id, SUM(1.0 / (60 + r)) AS score
      FROM (SELECT chunk_id, r FROM kw UNION ALL SELECT chunk_id, r FROM sem)
     GROUP BY chunk_id
)
SELECT f.chunk_id,
       f.score,
       c.session_id,
       s.agent,
       s.title,
       s.started_at,
       c.text AS preview,                          -- full chunk text; search.centerSnippet cuts the hit-centred preview in Go
       c.first_seq,
       c.last_seq,
       COALESCE((SELECT MIN(m.created_at) FROM messages m
                 WHERE m.session_id = c.session_id
                   AND m.seq BETWEEN c.first_seq AND c.last_seq),
                s.started_at)
                              AS chunk_at
  FROM fused f
  JOIN chunks   c ON c.id = f.chunk_id
  JOIN sessions s ON s.id = c.session_id
 ORDER BY f.score DESC
 LIMIT :limit;


-- name: SearchHybridByAgent :many
-- Same fusion as SearchHybrid, scoped to one agent. vec0's `agent` metadata
-- column has to be filtered as a literal equality constraint inside the KNN
-- query itself — it can't be expressed as ":agent IS NULL OR ..." the way a
-- normal b-tree index column can — so an agent-scoped call needs its own
-- copy of the `sem` CTE rather than one shared query. The `kw` CTE is
-- filtered too, for consistency with SearchKeyword's own agent scoping.
WITH kw AS (
    SELECT c.id AS chunk_id,
           row_number() OVER (ORDER BY chunks_fts.rank) AS r
      FROM chunks_fts
      JOIN chunks   c ON c.id = chunks_fts.rowid
      JOIN sessions s ON s.id = c.session_id
     WHERE chunks_fts MATCH :query
       AND s.project_id = :project_id
       AND s.agent = :agent
     ORDER BY chunks_fts.rank
     LIMIT 50
),
sem AS (
    -- Same distance floor as SearchHybrid; see the note there.
    SELECT chunk_id,
           row_number() OVER (ORDER BY distance) AS r
      FROM (SELECT chunk_id, distance
              FROM chunk_vectors
             WHERE embedding MATCH :query_embedding   -- JSON array or float32 blob
               AND k = 50
               AND project_id = :project_id
               AND agent = :agent)
     WHERE distance <= :max_distance
),
fused AS (
    SELECT chunk_id, SUM(1.0 / (60 + r)) AS score
      FROM (SELECT chunk_id, r FROM kw UNION ALL SELECT chunk_id, r FROM sem)
     GROUP BY chunk_id
)
SELECT f.chunk_id,
       f.score,
       c.session_id,
       s.agent,
       s.title,
       s.started_at,
       c.text AS preview,                          -- see SearchHybrid
       c.first_seq,
       c.last_seq,
       COALESCE((SELECT MIN(m.created_at) FROM messages m
                 WHERE m.session_id = c.session_id
                   AND m.seq BETWEEN c.first_seq AND c.last_seq),
                s.started_at)
                              AS chunk_at
  FROM fused f
  JOIN chunks   c ON c.id = f.chunk_id
  JOIN sessions s ON s.id = c.session_id
 ORDER BY f.score DESC
 LIMIT :limit;


-- name: RecentSessions :many
SELECT *
  FROM v_session_overview
 WHERE project_id = :project_id
   AND (:agent IS NULL OR agent = :agent)
 ORDER BY started_at DESC
 LIMIT :limit;


-- name: SessionsTouchingFile :many
-- "Before I edit this file, who has been here before and what did they do?"
SELECT s.id, s.agent, s.title, s.started_at,
       group_concat(DISTINCT ft.action) AS actions,
       COUNT(*)                          AS touches
  FROM file_touches ft
  JOIN sessions s ON s.id = ft.session_id
 WHERE ft.project_id = :project_id
   AND ft.path = :path
 GROUP BY s.id
 ORDER BY s.started_at DESC
 LIMIT :limit;


-- name: ExplainFile :many
-- File-centred context briefing. This stays intentionally compact: callers
-- get the sessions that touched the file, what happened, the known git span,
-- and one chunk id to open for the conversational "why".
--
-- Ranking: sessions that changed the file (anything but a read) come before
-- sessions that only looked at it, then newest first — "who edited this" is
-- the question, and a stray Read shouldn't outrank the session that rewrote it.
--
-- The chunk is anchored to the touch itself: the chunk whose seq range holds
-- the most relevant touching message (latest change, else latest read). If
-- that message isn't chunked yet (an unsettled tail turn), fall back to the
-- nearest earlier chunk, then to the session's first chunk.
WITH touched AS (
    SELECT s.id,
           s.agent,
           s.title,
           s.started_at,
           s.git_branch,
           s.starting_commit,
           s.ending_commit,
           s.commit_source,
           group_concat(DISTINCT ft.action) AS actions,
           COUNT(*) AS touches,
           SUM(ft.action <> 'read') AS changes,
           MAX(ft.created_at) AS last_touch_at,
           (SELECT m.seq
              FROM file_touches f2
              JOIN messages m ON m.id = f2.message_id
             WHERE f2.session_id = s.id
               AND f2.project_id = :project_id
               AND f2.path = :path
             ORDER BY (f2.action <> 'read') DESC, f2.created_at DESC, f2.id DESC
             LIMIT 1) AS anchor_seq
      FROM file_touches ft
      JOIN sessions s ON s.id = ft.session_id
     WHERE ft.project_id = :project_id
       AND ft.path = :path
     GROUP BY s.id
     ORDER BY (SUM(ft.action <> 'read') > 0) DESC,
              COALESCE(MAX(ft.created_at), s.started_at) DESC,
              s.id
     LIMIT :limit
),
picked AS (
    SELECT t.*,
           COALESCE(
               (SELECT c.id FROM chunks c
                 WHERE c.session_id = t.id
                   AND t.anchor_seq BETWEEN c.first_seq AND c.last_seq),
               (SELECT c.id FROM chunks c
                 WHERE c.session_id = t.id
                   AND c.last_seq <= t.anchor_seq
                 ORDER BY c.last_seq DESC
                 LIMIT 1),
               (SELECT c.id FROM chunks c
                 WHERE c.session_id = t.id
                 ORDER BY c.first_seq
                 LIMIT 1)
           ) AS chunk_id
      FROM touched t
)
SELECT p.id,
       p.agent,
       p.title,
       p.started_at,
       p.git_branch,
       p.starting_commit,
       p.ending_commit,
       p.commit_source,
       p.actions,
       p.touches,
       p.changes,
       p.last_touch_at,
       p.chunk_id,
       c.text AS preview
  FROM picked p
  LEFT JOIN chunks c ON c.id = p.chunk_id
 ORDER BY (p.changes > 0) DESC,
          COALESCE(p.last_touch_at, p.started_at) DESC,
          p.id;


-- name: GetSessionMessages :many
-- Paged transcript read. The app caps (to_seq - from_seq) and the total
-- characters returned, so an agent can't swallow a 2,000-message session in
-- one call. Conversation text only unless :include_tools = 1: tool calls and
-- especially tool results are the bulk of a transcript's bytes and rarely the
-- part an agent is looking for.
SELECT seq, role, kind, tool_name, content, created_at
  FROM messages
 WHERE session_id = :session_id
   AND seq BETWEEN :from_seq AND :to_seq
   AND kind <> 'thinking'                          -- reasoning traces rarely help retrieval
   AND (:include_tools = 1 OR kind = 'text')
 ORDER BY seq;


-- name: FirstMessageSeqAfter :one
-- Resume point for paging: the first message past :after_seq that
-- GetSessionMessages would return under the same :include_tools setting.
SELECT MIN(seq)
  FROM messages
 WHERE session_id = :session_id
   AND seq > :after_seq
   AND kind <> 'thinking'
   AND (:include_tools = 1 OR kind = 'text');


-- name: GetChunk :one
-- One chunk (a user turn + the assistant's reply, ~900 tokens) by id, scoped
-- to the caller's project so a chunk id from another project reads as absent.
SELECT c.id, c.session_id, s.agent, c.first_seq, c.last_seq, c.text,
       COALESCE((SELECT MIN(m.created_at) FROM messages m
                 WHERE m.session_id = c.session_id
                   AND m.seq BETWEEN c.first_seq AND c.last_seq),
                s.started_at)
                              AS chunk_at
  FROM chunks c
  JOIN sessions s ON s.id = c.session_id
 WHERE c.id = :chunk_id
   AND s.project_id = :project_id;


-- name: SearchNotes :many
-- Live notes only (superseded ones are history, not guidance). Global notes
-- (project_id IS NULL) are included in every project's results.
SELECT n.id, n.kind, n.title, n.body, n.tags, n.agent, n.created_at
  FROM notes_fts
  JOIN notes n ON n.id = notes_fts.rowid
 WHERE notes_fts MATCH :query
   AND n.superseded_by IS NULL
   AND (n.project_id = :project_id OR n.project_id IS NULL)
 ORDER BY notes_fts.rank
 LIMIT :limit;


-- name: PendingEmbeddings :many
-- The embed worker's to-do list. Chunks of a session with no resolved project
-- are excluded here rather than skipped by the worker: chunk_vectors.project_id
-- is a partition key and cannot be NULL, and if they were selected they would
-- fill the LIMIT window, so a batch made only of them would end the drain
-- while embeddable chunks queued behind them were never reached.
SELECT c.id, c.text, s.project_id, s.agent, c.session_id
  FROM chunks c
  JOIN sessions s ON s.id = c.session_id
 WHERE c.embedded_at IS NULL
   AND s.project_id IS NOT NULL
 ORDER BY c.id
 LIMIT :limit;
