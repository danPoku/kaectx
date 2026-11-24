// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/brief"
)

type getBriefArgs struct {
	Budget int `json:"budget,omitempty" jsonschema:"largest size of the brief in tokens, default 600"`
}

type getBriefResult struct {
	// Text is the brief, empty when the project has no history yet.
	Text string `json:"text"`
	// Tokens is the estimated size of Text; Budget is the limit it was held to.
	Tokens int `json:"tokens"`
	Budget int `json:"budget"`
	// HandoffNoteID is the handoff note the brief included, if any. Reading
	// it here is recorded as that handoff being picked up.
	HandoffNoteID int64 `json:"handoff_note_id,omitempty"`
}

func (s *server) registerBrief(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_brief",
		Description: "A short brief of past work in this project, from every agent: the latest handoff note, the most recent sessions with the files they changed, and the files changed most in the past week. " +
			"Call it once at the start of a session, before searching, unless a brief was already given to you when the session started. " +
			"It is held to a token budget (default 600) and is empty for a project with no history. Each line carries the call that opens its source.",
	}, s.getBrief)
}

// getBrief is the MCP counterpart of `kae brief --hook`: the same text, for
// an agent with no session-start hook to deliver it. Calling it is a
// delivery, so the handoff it includes is recorded as picked up.
func (s *server) getBrief(_ context.Context, _ *mcp.CallToolRequest, args getBriefArgs) (*mcp.CallToolResult, getBriefResult, error) {
	b, err := brief.Build(s.db, s.projectID, time.Now())
	if err != nil {
		return nil, getBriefResult{}, err
	}
	r := b.Render(args.Budget)
	if err := brief.RecordDelivery(s.db, r.HandoffNoteID); err != nil {
		return nil, getBriefResult{}, err
	}
	return nil, getBriefResult{Text: r.Text, Tokens: r.Tokens, Budget: r.Budget, HandoffNoteID: r.HandoffNoteID}, nil
}
