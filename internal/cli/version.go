// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"runtime"
	"runtime/debug"
)

// Version is stamped into release binaries by the release workflow:
//
//	-ldflags "-X github.com/danPoku/kaectx/internal/cli.Version=v0.3.0"
//
// It is empty in a `go install` or local build, where the Go toolchain's own
// build information is used instead.
var Version = ""

// versionString says which kae this is. A release binary knows from the
// stamp above. A `go install ...@v0.3.0` build knows from the module version
// Go records. A build from a clone has neither, and says "dev" rather than
// claim a release it is not.
func versionString() string {
	v := Version
	if v == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	if v == "" {
		v = "dev"
	}
	return "kae " + v + " " + runtime.GOOS + "/" + runtime.GOARCH
}
