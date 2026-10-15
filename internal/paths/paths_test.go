// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package paths_test

import (
	"testing"

	"github.com/danPoku/kaectx/internal/paths"
)

func TestPortableAbsolutePaths(t *testing.T) {
	for _, p := range []string{"/home/dev/x", `C:\Users\dev\x`, `C:/Users/dev/x`, `\\server\share\x`} {
		if !paths.IsAbs(p) {
			t.Errorf("IsAbs(%q) = false", p)
		}
	}
	if paths.IsAbs("internal/store/x.go") {
		t.Error("relative path reported absolute")
	}
}

func TestRelIfWithinAcrossPathStyles(t *testing.T) {
	cases := []struct{ base, target, want string }{
		{"/home/dev/repo", "/home/dev/repo/a.go", "a.go"},
		{`C:\Users\Dev\repo`, `c:\users\dev\repo\pkg\a.go`, "pkg/a.go"},
		{`\\server\share\repo`, `\\server\share\repo\a.go`, "a.go"},
	}
	for _, c := range cases {
		if got, ok := paths.RelIfWithin(c.base, c.target); !ok || got != c.want {
			t.Errorf("RelIfWithin(%q,%q) = %q,%v; want %q,true", c.base, c.target, got, ok, c.want)
		}
	}
	for _, c := range [][2]string{{"/repo", "/elsewhere/a"}, {`C:\repo`, `D:\repo\a`}, {"/repo", `C:\repo\a`}} {
		if got, ok := paths.RelIfWithin(c[0], c[1]); ok {
			t.Errorf("RelIfWithin(%q,%q) = %q,true; want outside", c[0], c[1], got)
		}
	}
}

func TestKeyMatchesEverySpellingOfOneFolder(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"windows", `C:\Users\kojog\NIORA`, "c:/users/kojog/niora"},
		{"lowercase drive", `c:\Users\kojog\NIORA`, "c:/users/kojog/niora"},
		{"forward slashes", `C:/Users/kojog/NIORA/`, "c:/users/kojog/niora"},
		{"wsl mount", "/mnt/c/Users/kojog/NIORA", "c:/users/kojog/niora"},
		{"wsl mount upper", "/mnt/C/Users/kojog/NIORA", "c:/users/kojog/niora"},
		{"drive root", `C:\`, "c:"},
		{"wsl drive root", "/mnt/c", "c:"},
		{"unc", `\\Server\Share\Repo`, "//server/share/repo"},
		{"posix keeps case", "/home/Dev/repo", "/home/Dev/repo"},
		{"not a mount", "/mnt/data/repo", "/mnt/data/repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paths.Key(c.in); got != c.want {
				t.Errorf("Key(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		k, root string
		want    bool
	}{
		{"c:/repo", "c:/repo", true},
		{"c:/repo/pkg", "c:/repo", true},
		{"c:/repo-two", "c:/repo", false},
		{"c:/repo", "c:", true},
		{"/home/dev/repo", "/home/dev", true},
	} {
		if got := paths.Within(c.k, c.root); got != c.want {
			t.Errorf("Within(%q, %q) = %v, want %v", c.k, c.root, got, c.want)
		}
	}
}

func TestCleanSlashPreservesRootKindsAndDot(t *testing.T) {
	for input, want := range map[string]string{
		`\\Server\Share\repo\a.go`: "//Server/Share/repo/a.go",
		"/etc/hosts":               "/etc/hosts",
		".":                        ".",
	} {
		if got := paths.CleanSlash(input); got != want {
			t.Errorf("CleanSlash(%q) = %q, want %q", input, got, want)
		}
	}
}
