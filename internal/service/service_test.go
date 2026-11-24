// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorder is a Runner that records what it was asked to run. noSystemctl
// makes LookPath fail for systemctl, as on WSL without systemd.
type recorder struct {
	noSystemctl bool
	calls       []string
}

func (r *recorder) LookPath(name string) (string, error) {
	if name == "systemctl" && r.noSystemctl {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}

func (r *recorder) Run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "reg" && args[0] == "query" {
		return "", errors.New("value not found")
	}
	return "", nil
}

func (r *recorder) Start(name string, args ...string) error {
	r.calls = append(r.calls, "start "+name+" "+strings.Join(args, " "))
	return nil
}

func TestSystemdReplacesTheOldUnit(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(unitDir(home), LegacyUnitName)
	if err := writeFile(legacy, "[Service]\nExecStart=%h/go/bin/ctx daemon\n"); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	o := Options{GOOS: "linux", Home: home, Exe: "/home/a/.local/bin/kae", Run: r}

	changes, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("want the old unit removed and the new one installed, got %+v", changes)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Fatal("the old unit file is still there")
	}
	want := []string{
		"systemctl --user disable --now ctx-daemon.service",
		"systemctl --user daemon-reload",
		"systemctl --user enable kae-daemon.service",
		"systemctl --user restart kae-daemon.service",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}
	if !Installed(o) {
		t.Fatal("Installed should be true after Install")
	}

	// Same binary: nothing to do. A different binary path: reinstall.
	r.calls = nil
	if changes, _ := Install(o); len(changes) != 0 || len(r.calls) != 0 {
		t.Fatalf("repeat install changed something: %+v %v", changes, r.calls)
	}
	o.Exe = "/opt/homebrew/bin/kae"
	if Installed(o) {
		t.Fatal("a unit that runs another path is not this install")
	}
}

func TestSystemdMissingIsReportedNotAnError(t *testing.T) {
	home := t.TempDir()
	changes, err := Install(Options{GOOS: "linux", Home: home, Exe: "/opt/kae", Run: &recorder{noSystemctl: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Manual {
		t.Fatalf("want one instruction for the user, got %+v", changes)
	}
	if _, err := os.Stat(unitPath(home)); err == nil {
		t.Fatal("a unit file was written although systemd is not there to run it")
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		home := t.TempDir()
		r := &recorder{}
		changes, err := Install(Options{GOOS: goos, Home: home, Exe: "/opt/kae", Run: r, DryRun: true})
		if err != nil || len(changes) != 1 {
			t.Fatalf("%s: got %+v, %v", goos, changes, err)
		}
		for _, c := range r.calls {
			if !strings.HasPrefix(c, "reg query") {
				t.Fatalf("%s: dry run ran %q", goos, c)
			}
		}
		if entries, _ := os.ReadDir(home); len(entries) != 0 {
			t.Fatalf("%s: dry run wrote files: %v", goos, entries)
		}
	}
}

func TestLaunchdPlistEscapesThePath(t *testing.T) {
	got := plist("/Users/a&b", "/Users/a&b/bin/kae")
	if !strings.Contains(got, "<string>/Users/a&amp;b/bin/kae</string>") || strings.Contains(got, "a&b") {
		t.Fatalf("path not escaped for XML:\n%s", got)
	}
}

func TestUnsupportedSystem(t *testing.T) {
	if Supported("plan9") {
		t.Fatal("plan9 should not be supported")
	}
	changes, err := Install(Options{GOOS: "plan9", Run: &recorder{}})
	if err != nil || len(changes) != 1 || !changes[0].Manual {
		t.Fatalf("got %+v, %v", changes, err)
	}
}
