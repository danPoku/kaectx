# ctx — local multi-agent context store

A single Go binary that ingests chat sessions from every coding agent on this
machine (Claude Code, Codex CLI, Pi, BB) into one SQLite database, and serves
them back to any agent via MCP and a CLI — so agents can retrieve context from
their own past sessions ("within-agent") or each other's ("cross-agent").

Mental model: an insurer's head office. Branches (agents) keep filing claims
their own way; head office collects, standardises, indexes, and answers queries.

## Environment
- Linux (including WSL Ubuntu) is the primary development and documentation
  environment. Native Windows is also fully supported and tested.
- Linux/WSL DB lives at `~/.ctx/ctx.db` — NEVER under `/mnt/c` (SQLite locking
  across the Windows boundary is unreliable). Native Windows uses
  `%USERPROFILE%\.ctx\ctx.db`; the two catalogues are independent.
- Repo may also be opened in GitHub Codespaces: identify projects by normalised
  git remote, never by absolute path.

## Decisions already made (don't relitigate without asking)
- Language: Go. Single binary, subcommands: `ctx daemon`, `ctx mcp`, `ctx search`, etc.
- Storage: SQLite, WAL mode, STRICT tables, JSON in TEXT columns with json_valid checks.
- Keyword search: FTS5 over `chunks` (external content, porter + unicode61, `_` as tokenchar).
- Semantic search: sqlite-vec `vec0` table `chunk_vectors`, 768 dims, cosine,
  `project_id` partition key, `agent` metadata column.
- Ranking: Reciprocal Rank Fusion (k=60) of FTS + vector results over the SAME unit (chunks).
- Retrieval unit (chunk): one user turn + the assistant's reply, tool noise trimmed.
- MCP: official SDK `github.com/modelcontextprotocol/go-sdk`, stdio transport.
- Embeddings: local only, via Ollama HTTP API (`nomic-embed-text`). Code never leaves the machine.
- Queries: `queries/*.sql` in sqlc format (`-- name: X :many`) are the source of truth.
- SQLite driver: start with `mattn/go-sqlite3` (`-tags sqlite_fts5`) + sqlite-vec cgo
  bindings. Evaluate `ncruces/go-sqlite3` only if cgo becomes a real problem.

## Branch workflow
Full rules in `CONTRIBUTING.md`. The parts an agent must never get wrong:
- `master` changes only by merging a PR on GitHub. Never commit to `master`
  locally and push it, whoever asks; `.githooks/pre-push` refuses it
  (`git config core.hooksPath .githooks` once per clone; never `--no-verify`).
- Cut every branch from an up-to-date `master`, named `<prefix>/<issue>-<slug>`
  with prefix `feature/`, `fix/`, `hotfix/`, `chore/`, `docs/`, or `release/vX.Y.Z`.
- PRs merge with a merge commit, never squash. Delete the branch after merge.
- Releases are `v*` tags on `master`, pushed only after the `release/` PR merges.
  A pushed tag is never moved or deleted.

## Schema
Already written and tested — see `migrations/001_core.sql`, `migrations/002_vectors.sql`,
`queries/retrieval.sql`. Tables: projects, project_paths, sources, sessions, messages,
chunks (+ chunks_fts), file_touches, notes (+ notes_fts), chunk_vectors.
Migration 002 must be optional: if sqlite-vec fails to load, keyword search still works.

## Non-negotiables
- **Redact before storing.** Tool outputs contain API keys and `.env` contents. Run a
  redaction pass (common key patterns, `KEY=value` lines from env files, high-entropy
  tokens) on `content` AND `raw` before insert. Test it.
- **Idempotent ingest.** Track byte offset + first-4KB hash per source file in `sources`.
  Re-running ingest must never duplicate rows (`UNIQUE(session_id, seq)` backs this up).
- **Never crash on a bad line.** Log to `sources.last_error`, skip, continue.
- **Progressive disclosure.** MCP tools return summaries/snippets by default; full
  transcripts only through paged `get_session(id, from_seq, to_seq)` with a size cap.
- **Discover log formats from real files.** Don't assume agent log schemas from memory —
  inspect actual files under `~/.claude/projects/`, `~/.codex/sessions/`, and wherever
  Pi and BB write, and write adapters + fixture tests from real (redacted) samples.

## Code style
- Heavy comments. Explain *why*, and use realistic analogies where a concept is non-obvious.
- Small packages: `internal/store`, `internal/ingest/<agent>`, `internal/redact`,
  `internal/embed`, `internal/search`, `internal/mcpserver`, `cmd/ctx`.
- Table-driven tests. Each ingest adapter gets fixture files in `testdata/`.

## Milestones (build in this order, each one working end-to-end before the next)
1. `ctx init` — create `~/.ctx/`, open DB with pragmas, run migrations.
2. Claude Code adapter + redaction + `ctx ingest` (one-shot) + `ctx search` (FTS only).
3. Codex adapter. Verify cross-agent keyword search works.
4. `ctx mcp` exposing: search_context, recent_sessions, get_session,
   sessions_touching, explain_file, save_note, search_notes. Register it with Claude Code and Codex.
5. Embedding worker + hybrid search.
6. `ctx daemon` (fsnotify tailing), then Pi and BB adapters.
7. Session summaries via a cheap model (optional, off by default).

## Connection pragmas (set on every connection)
journal_mode=WAL, foreign_keys=ON, busy_timeout=5000, synchronous=NORMAL
