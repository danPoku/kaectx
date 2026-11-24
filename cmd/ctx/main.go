// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Command ctx is the old name of kae, kept for one minor release so that
// scripts, services and agent registrations written for `ctx` keep working
// while they are moved over. It runs exactly what kae runs, after saying on
// stderr that the name is going away. It will be removed in the next minor
// release.
package main

import (
	"os"

	"github.com/danPoku/kaectx/internal/cli"
)

func main() {
	cli.WarnDeprecatedAlias(os.Stderr)
	cli.Main()
}
