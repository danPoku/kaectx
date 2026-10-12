// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package mcpserver exposes ctx's retrieval and note-taking tools over MCP,
// so an agent can pull its own (or another agent's) past context mid-session
// instead of re-deriving it. Every tool response is a summary or a capped
// page — progressive disclosure — never a full unbounded transcript dump,
// so a single tool call can't swallow an agent's whole context window.
package mcpserver

import (
	"database/sql"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/store"
)

// server holds what every tool handler needs. projectID is resolved once,
// from the directory `ctx mcp` was launched in — the same assumption
// `ctx search` makes: a local stdio MCP server is spawned by the calling
// agent from within the project it's working on, so the server's own
// working directory IS the project.
type server struct {
	db          *sql.DB
	projectID   int64
	cwd         string
	embedClient embed.Embedder
	catchUp     *catchUp // nil unless Options.Roots was set
}

// Options configures the parts of the server that reach outside the
// database. The zero value is a server that only ever reads what is already
// indexed, which is what tests want.
type Options struct {
	// Roots are the agents' log directories. When set, the server catches
	// up on them in the background at startup, reports index freshness in
	// search_context and recent_sessions, and catches up again whenever a
	// search finds unread logs.
	Roots []ingest.Root
	// LockPath is the file that keeps concurrent ctx processes from
	// catching up at the same time; see ingest.CatchUp. Required with Roots.
	LockPath string
}

// New builds the ctx MCP server and registers every tool. cwd is the
// directory the `ctx mcp` process was launched from. embedClient backs
// search_context's semantic half — callers construct it explicitly (rather
// than New picking a default internally) so tests can inject a fake instead
// of depending on whatever Ollama happens to be reachable at
// embed.DefaultBaseURL on the machine running them.
func New(db *sql.DB, cwd string, embedClient embed.Embedder) (*mcp.Server, error) {
	return NewWithOptions(db, cwd, embedClient, Options{})
}

// NewWithOptions is New plus Options; see Options for what each one enables.
func NewWithOptions(db *sql.DB, cwd string, embedClient embed.Embedder, opts Options) (*mcp.Server, error) {
	projectID, err := store.ResolveProject(db, cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve project for %s: %w", cwd, err)
	}
	s := &server{db: db, projectID: projectID, cwd: cwd, embedClient: embedClient}
	if len(opts.Roots) > 0 {
		s.catchUp = &catchUp{roots: opts.Roots, lockPath: opts.LockPath}
		// Catch up at once rather than on the first search: the agent's
		// first question is usually its most important one.
		s.startCatchUp()
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "ctx", Version: "0.2.0+dev"}, &mcp.ServerOptions{
		Instructions: "Use ctx to recover prior work without loading whole transcripts. Use search_context then get_chunk for the smallest useful context: each hit's next_step is the exact get_chunk call, with the run_id and rank that record the exact open. When retrieved context influences an answer or action, call record_retrieval_outcome with that run ID and rank and an honest outcome; an open alone is never a helpful label. At natural milestones call feedback_status and checkpoint_status. If checkpoint_status recommends a checkpoint, tell the user and offer to save a handoff before continuing in a fresh session; never end or replace the session without the user's choice, and record that choice with record_checkpoint_outcome.",
	})
	s.registerSearch(srv)
	s.registerChunk(srv)
	s.registerSessions(srv)
	s.registerNotes(srv)
	s.registerExplain(srv)
	s.registerFeedback(srv)
	s.registerCheckpoint(srv)
	return srv, nil
}
