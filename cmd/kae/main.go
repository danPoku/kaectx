// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command kae is the single binary for the local multi-agent context store:
// ingesting agent sessions, indexing them, and serving them back over MCP
// and the CLI. The command line itself lives in internal/cli.
package main

import "github.com/danPoku/kaectx/internal/cli"

func main() {
	cli.Main()
}
