// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package paths handles paths recorded by agents on a different operating
// system from the ctx process reading them. Session history can outlive a WSL
// to Windows move, so filepath alone is not sufficient.
package paths

import (
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var windowsVolume = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
var wslVolume = regexp.MustCompile(`^/mnt/([A-Za-z])(?:/|$)`)

// IsAbs recognizes native, POSIX, drive-letter, and UNC absolute paths.
func IsAbs(p string) bool {
	p = strings.TrimSpace(p)
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || windowsVolume.MatchString(p) || strings.HasPrefix(p, `\\`)
}

// CleanSlash returns the stable slash-separated form used in the database.
// path.Clean is deliberately used after separator normalization so its result
// does not depend on the host OS.
func CleanSlash(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if p == "" {
		return ""
	}
	unc := strings.HasPrefix(p, "//")
	clean := path.Clean(p)
	if unc && !strings.HasPrefix(clean, "//") {
		clean = "/" + clean
	}
	return strings.TrimPrefix(clean, "./")
}

// ToHost maps the path convention shared by Windows and WSL to the current
// host. It returns false for paths without a safe, mechanical mapping (for
// example /home on Windows or UNC on Linux).
func ToHost(p string) (string, bool) {
	clean := CleanSlash(p)
	if runtime.GOOS == "windows" {
		m := wslVolume.FindStringSubmatch(clean)
		if m == nil {
			return "", false
		}
		rest := strings.TrimPrefix(clean[len(m[0]):], "/")
		mapped := strings.ToUpper(m[1]) + `:\`
		if rest != "" {
			mapped += filepath.FromSlash(rest)
		}
		return mapped, true
	}
	if windowsVolume.MatchString(clean) {
		drive := strings.ToLower(clean[:1])
		rest := strings.TrimPrefix(clean[2:], "/")
		return path.Join("/mnt", drive, rest), true
	}
	return "", false
}

var driveLetter = regexp.MustCompile(`^[A-Za-z]:(?:/|$)`)

// Key is the spelling-independent identity of a folder, for comparing paths
// that agents logged in different conventions. Like a postcode that ignores
// whether the address was written in capitals, it maps every spelling of one
// folder to one string:
//   - separators become "/" and the path is cleaned;
//   - a WSL mount (/mnt/c/x) becomes the drive it mounts (c:/x);
//   - a drive-letter or UNC path is lowercased, because Windows treats
//     C:\Repo and c:\repo as the same folder.
//
// POSIX paths keep their case: /home/Dev and /home/dev are different folders.
// Key is for comparison only; store the path as the agent logged it.
func Key(p string) string {
	k := CleanSlash(p)
	if m := wslVolume.FindStringSubmatch(k); m != nil {
		rest := strings.TrimPrefix(k[len(m[0]):], "/")
		k = m[1] + ":"
		if rest != "" {
			k += "/" + rest
		}
	}
	if driveLetter.MatchString(k) || strings.HasPrefix(k, "//") {
		k = strings.ToLower(k)
	}
	return k
}

// Within reports whether key k is the folder root or inside it, both being
// results of Key.
func Within(k, root string) bool {
	return k == root || strings.HasPrefix(k, strings.TrimSuffix(root, "/")+"/")
}

// RelIfWithin lexically rebases target under base using portable separators.
// It is intended for foreign or missing paths, where filepath.Rel cannot be
// used. Windows drive and UNC comparisons are case-insensitive.
func RelIfWithin(base, target string) (string, bool) {
	b := CleanSlash(base)
	t := CleanSlash(target)
	if b == "" || t == "" || !samePathStyle(b, t) {
		return "", false
	}
	compareBase, compareTarget := b, t
	if isWindowsStyle(b) {
		compareBase, compareTarget = strings.ToLower(b), strings.ToLower(t)
	}
	compareBase = strings.TrimSuffix(compareBase, "/")
	if compareTarget == compareBase {
		return ".", true
	}
	prefix := compareBase + "/"
	if !strings.HasPrefix(compareTarget, prefix) {
		return "", false
	}
	return t[len(prefix):], true
}

func samePathStyle(a, b string) bool {
	return isWindowsStyle(a) == isWindowsStyle(b) && strings.HasPrefix(a, "/") == strings.HasPrefix(b, "/")
}

func isWindowsStyle(p string) bool {
	return windowsVolume.MatchString(p) || strings.HasPrefix(p, "//")
}
