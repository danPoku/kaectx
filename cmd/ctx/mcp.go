// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/mcpserver"
)

// runMCP serves ctx's tools over stdio. It's meant to be launched by a
// coding agent (Claude Code, Codex, ...) as a local subprocess, from within
// the project the agent is working on — mcpserver.New resolves that project
// from this process's own working directory, same as `ctx search` does.
//
// The server is also given the agents' log directories, so it can catch up
// on unread logs itself when no `ctx daemon` is running; see
// mcpserver.Options.
func runMCP(args []string) error {
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	client := embed.NewOllamaClient(embed.DefaultBaseURL, embed.DefaultModel)
	srv, err := mcpserver.NewWithOptions(db, cwd, client, mcpserver.Options{
		Roots: ingestRoots(home), LockPath: ingestLockPath(home),
	})
	if err != nil {
		return err
	}

	return srv.Run(context.Background(), &sdkmcp.StdioTransport{})
}
