// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/brief"
	"github.com/danPoku/kaectx/internal/handoff"
)

type getHandoffArgs struct{}

type getHandoffResult struct {
	// Found is false when the project has no handoff; every other field is
	// then empty.
	Found  bool   `json:"found"`
	NoteID int64  `json:"note_id,omitempty"`
	Title  string `json:"title,omitempty"`
	// Text is the whole handoff, ready to read.
	Text      string `json:"text,omitempty"`
	Agent     string `json:"agent,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	// SessionID and AnchorSeq say where the handoff was saved, when known,
	// and NextStep is the call that opens that conversation.
	SessionID string `json:"session_id,omitempty"`
	AnchorSeq *int   `json:"anchor_seq,omitempty"`
	NextStep  string `json:"next_step,omitempty"`
}

func (s *server) registerHandoff(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_handoff",
		Description: "The newest handoff note for this project, whichever agent or command wrote it: what the last session was asked to do, where it stopped, and the files it changed. " +
			"Call it when the user asks to continue, resume or pick up earlier work. found is false when no handoff has been saved. " +
			"next_step opens the conversation the handoff came from. Reading a handoff here is recorded as it being picked up.",
	}, s.getHandoff)
}

// getHandoff is the MCP counterpart of `kae resume`.
func (s *server) getHandoff(_ context.Context, _ *mcp.CallToolRequest, _ getHandoffArgs) (*mcp.CallToolResult, getHandoffResult, error) {
	h, err := brief.LatestHandoff(s.db, s.projectID)
	if err != nil {
		return nil, getHandoffResult{}, err
	}
	if h == nil {
		return nil, getHandoffResult{}, nil
	}
	out := getHandoffResult{
		Found: true, NoteID: h.NoteID, Title: h.Title, Text: handoff.Format(h), Agent: h.Agent, CreatedAt: h.CreatedAt,
	}
	if h.HasSource {
		seq := h.AnchorSeq
		out.SessionID, out.AnchorSeq = h.SessionID, &seq
		out.NextStep = fmt.Sprintf("get_session(session_id=%q, from_seq=%d)", h.SessionID, h.AnchorSeq)
	}
	if err := brief.RecordDelivery(s.db, h.NoteID); err != nil {
		return nil, getHandoffResult{}, err
	}
	return nil, out, nil
}
