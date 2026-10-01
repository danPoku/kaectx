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
}

// New builds the ctx MCP server and registers every tool. cwd is the
// directory the `ctx mcp` process was launched from. embedClient backs
// search_context's semantic half — callers construct it explicitly (rather
// than New picking a default internally) so tests can inject a fake instead
// of depending on whatever Ollama happens to be reachable at
// embed.DefaultBaseURL on the machine running them.
func New(db *sql.DB, cwd string, embedClient embed.Embedder) (*mcp.Server, error) {
	projectID, err := store.ResolveProject(db, cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve project for %s: %w", cwd, err)
	}
	s := &server{db: db, projectID: projectID, cwd: cwd, embedClient: embedClient}

	srv := mcp.NewServer(&mcp.Implementation{Name: "ctx", Version: "0.1.3+dev"}, nil)
	s.registerSearch(srv)
	s.registerChunk(srv)
	s.registerSessions(srv)
	s.registerNotes(srv)
	s.registerExplain(srv)
	s.registerFeedback(srv)
	return srv, nil
}
