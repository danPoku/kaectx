// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

func TestVersionString(t *testing.T) {
	old := Version
	defer func() { Version = old }()

	// A release binary reports the version stamped in at build time.
	Version = "v1.2.3"
	if got := versionString(); !strings.HasPrefix(got, "kae v1.2.3 ") {
		t.Fatalf("stamped build: %q", got)
	}
	// An unstamped build never claims a release; under `go test` there is no
	// module version either, so it must say dev.
	Version = ""
	if got := versionString(); !strings.HasPrefix(got, "kae dev ") {
		t.Fatalf("unstamped build: %q", got)
	}
}
