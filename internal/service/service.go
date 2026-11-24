// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package service installs `kae daemon` as a per-user background service, so
// the index stays current without a terminal left open.
//
// Each system has its own way to say "start this when I log in":
//
//   - Linux: a systemd user unit, ~/.config/systemd/user/kae-daemon.service.
//   - macOS: a launchd agent, ~/Library/LaunchAgents/<label>.plist.
//   - Windows: a value under the user's Run registry key. A scheduled task
//     with a logon trigger was the first choice, but creating one needs
//     administrator rights (schtasks answers "Access is denied" to a normal
//     user), and kae setup must not need them. The Run key is the per-user
//     equivalent and needs none.
//
// None of the three needs root, and all of them run the daemon as the user,
// which matters: the daemon reads that user's agent logs and writes that
// user's ~/.kae.
//
// The operating system is a parameter rather than runtime.GOOS so that every
// branch is tested on whichever system runs the tests.
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danPoku/kaectx/internal/agentcfg"
)

const (
	// UnitName and LegacyUnitName are the systemd units, now and under the
	// tool's old name.
	UnitName       = "kae-daemon.service"
	LegacyUnitName = "ctx-daemon.service"
	// Label is the launchd agent's label, in reverse-DNS form as launchd
	// expects.
	Label = "io.github.danpoku.kae.daemon"
	// RunValue is the name of the value under the Windows Run key.
	RunValue = "kae-daemon"

	runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// Change is the same record agent registration uses, so setup can report
// both in one list.
type Change = agentcfg.Change

const agent = "daemon"

// Options says what to install and where.
type Options struct {
	// GOOS is "linux", "darwin" or "windows".
	GOOS string
	// Home is the user's home directory.
	Home string
	// Exe is the absolute path of the kae binary the service should run.
	Exe string
	// UID is the user's numeric id, which launchd's commands address.
	UID int
	// Run runs systemctl, launchctl and reg.
	Run agentcfg.Runner
	// DryRun reports what would change without changing it.
	DryRun bool
}

// Supported reports whether Install knows how to set up a service on goos.
func Supported(goos string) bool {
	return goos == "linux" || goos == "darwin" || goos == "windows"
}

// Installed reports whether the service is already set up to run exactly
// o.Exe. Setup uses it to decide whether there is anything to offer.
func Installed(o Options) bool {
	switch o.GOOS {
	case "linux":
		return fileIs(unitPath(o.Home), unit(o.Exe))
	case "darwin":
		return fileIs(plistPath(o.Home), plist(o.Home, o.Exe))
	case "windows":
		out, err := o.Run.Run("reg", "query", runKey, "/v", RunValue)
		return err == nil && strings.Contains(out, o.Exe)
	}
	return false
}

// Install sets the daemon up as a user service and starts it. It is safe to
// repeat: when the service already runs o.Exe it changes nothing.
func Install(o Options) ([]Change, error) {
	if Installed(o) {
		return nil, nil
	}
	switch o.GOOS {
	case "linux":
		return installSystemd(o)
	case "darwin":
		return installLaunchd(o)
	case "windows":
		return installRunKey(o)
	}
	return []Change{{Agent: agent, Manual: true,
		Detail: fmt.Sprintf("no service setup for %s; run `kae daemon` yourself to keep the index current", o.GOOS)}}, nil
}

func fileIs(path, content string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == content
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

// ---- Linux: systemd user unit ----

func unitDir(home string) string  { return filepath.Join(home, ".config", "systemd", "user") }
func unitPath(home string) string { return filepath.Join(unitDir(home), UnitName) }

func unit(exe string) string {
	// The path is quoted because systemd splits ExecStart on spaces.
	return `[Unit]
Description=kae daemon: keeps the coding-agent session index current

[Service]
ExecStart="` + exe + `" daemon
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`
}

func installSystemd(o Options) ([]Change, error) {
	path := unitPath(o.Home)
	if _, err := o.Run.LookPath("systemctl"); err != nil {
		// WSL without systemd, a container, or another init system.
		return []Change{{Agent: agent, Manual: true,
			Detail: "systemctl was not found, so no service was installed; run `kae daemon` yourself, or start it from your shell profile"}}, nil
	}
	var changes []Change

	// A unit left from the old name would start a second daemon. The two
	// would not corrupt anything (the ingest lock lets only one work), but
	// one would wait on the other for ever, so the old one is retired here.
	legacy := filepath.Join(unitDir(o.Home), LegacyUnitName)
	if _, err := os.Stat(legacy); err == nil {
		if !o.DryRun {
			// Best effort: the unit may already be stopped or disabled.
			_, _ = o.Run.Run("systemctl", "--user", "disable", "--now", LegacyUnitName)
			if err := os.Remove(legacy); err != nil {
				return nil, err
			}
		}
		changes = append(changes, Change{Agent: agent, Detail: "stopped and removed the old " + LegacyUnitName})
	}

	if !o.DryRun {
		if err := writeFile(path, unit(o.Exe)); err != nil {
			return nil, err
		}
		if out, err := o.Run.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return changes, fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(out))
		}
		if out, err := o.Run.Run("systemctl", "--user", "enable", UnitName); err != nil {
			return changes, fmt.Errorf("systemctl --user enable: %w: %s", err, strings.TrimSpace(out))
		}
		// restart rather than start, so a unit whose binary path changed
		// picks the new one up.
		if out, err := o.Run.Run("systemctl", "--user", "restart", UnitName); err != nil {
			return changes, fmt.Errorf("systemctl --user restart: %w: %s", err, strings.TrimSpace(out))
		}
	}
	return append(changes, Change{Agent: agent,
		Detail: fmt.Sprintf("installed and started the systemd user service %s (%s)", UnitName, path)}), nil
}

// ---- macOS: launchd agent ----

func plistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func plist(home, exe string) string {
	log := filepath.Join(home, ".kae", "daemon.log")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(log) + `</string>
</dict>
</plist>
`
}

func installLaunchd(o Options) ([]Change, error) {
	path := plistPath(o.Home)
	domain := fmt.Sprintf("gui/%d", o.UID)
	if !o.DryRun {
		// Unload an earlier version of the agent first; launchd refuses to
		// load a label it already has. An error here only means there was
		// nothing loaded.
		_, _ = o.Run.Run("launchctl", "bootout", domain+"/"+Label)
		if err := writeFile(path, plist(o.Home, o.Exe)); err != nil {
			return nil, err
		}
		if out, err := o.Run.Run("launchctl", "bootstrap", domain, path); err != nil {
			return nil, fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(out))
		}
	}
	return []Change{{Agent: agent,
		Detail: fmt.Sprintf("installed and started the launchd agent %s (%s)", Label, path)}}, nil
}

// ---- Windows: per-user Run key ----

// runCommand is the command line stored in the registry. kae is a console
// program, and Windows opens a console window for one started at logon;
// `conhost.exe --headless` runs it with no window.
func runCommand(exe string) string {
	return `conhost.exe --headless "` + exe + `" daemon`
}

func installRunKey(o Options) ([]Change, error) {
	if !o.DryRun {
		if out, err := o.Run.Run("reg", "add", runKey, "/v", RunValue, "/t", "REG_SZ", "/d", runCommand(o.Exe), "/f"); err != nil {
			return nil, fmt.Errorf("reg add: %w: %s", err, strings.TrimSpace(out))
		}
		// The Run key only takes effect at the next logon, so start one now.
		// If a daemon is already running, the new one waits on the ingest
		// lock and takes over when the old one stops.
		if err := o.Run.Start("conhost.exe", "--headless", o.Exe, "daemon"); err != nil {
			return nil, fmt.Errorf("start the daemon: %w", err)
		}
	}
	return []Change{{Agent: agent,
		Detail: fmt.Sprintf(`set the daemon to start at logon (%s\%s) and started it`, runKey, RunValue)}}, nil
}

// RemoveHint says how to undo Install on goos, for the README and for
// setup's closing message.
func RemoveHint(goos string) string {
	switch goos {
	case "linux":
		return "systemctl --user disable --now " + UnitName
	case "darwin":
		return "launchctl bootout gui/$(id -u)/" + Label
	case "windows":
		return `reg delete "` + runKey + `" /v ` + RunValue + ` /f`
	}
	return ""
}
