// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sumsFile writes a SHA256SUMS with a distinct checksum per archive (the
// archive's index repeated), so a manifest that paired a URL with the wrong
// checksum would be caught.
func sumsFile(t *testing.T, skip string) (string, map[string]string) {
	t.Helper()
	want := map[string]string{}
	var b strings.Builder
	for i, a := range archives {
		if a == skip {
			continue
		}
		sum := strings.Repeat(string(rune('a'+i)), 64)
		want[a] = sum
		// Mix both spellings sha256sum produces.
		if i%2 == 0 {
			b.WriteString(sum + "  " + a + "\n")
		} else {
			b.WriteString(sum + " *" + a + "\n")
		}
	}
	path := filepath.Join(t.TempDir(), "SHA256SUMS")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path, want
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunWritesEveryManifest(t *testing.T) {
	sums, want := sumsFile(t, "")
	out := t.TempDir()
	if err := run("v0.3.0", sums, out); err != nil {
		t.Fatal(err)
	}
	const base = "https://github.com/danPoku/kaectx/releases/download/v0.3.0/"

	formula := read(t, filepath.Join(out, "Formula", "kae.rb"))
	for _, a := range archives[:4] {
		// Each URL must be followed directly by its own checksum.
		pair := `url "` + base + a + "\"\n      sha256 \"" + want[a] + `"`
		if !strings.Contains(formula, pair) {
			t.Errorf("formula lacks %s with its checksum:\n%s", a, formula)
		}
	}
	if !strings.Contains(formula, `version "0.3.0"`) || strings.Contains(formula, "windows") {
		t.Errorf("formula version or platforms wrong:\n%s", formula)
	}

	var scoop struct {
		Version      string
		Bin          string
		Architecture map[string]struct{ URL, Hash string }
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "bucket", "kae.json"))), &scoop); err != nil {
		t.Fatalf("scoop manifest is not valid JSON: %v", err)
	}
	zip := "kae_windows_amd64.zip"
	if scoop.Version != "0.3.0" || scoop.Bin != "kae.exe" ||
		scoop.Architecture["64bit"].URL != base+zip || scoop.Architecture["64bit"].Hash != want[zip] {
		t.Errorf("scoop manifest: %+v", scoop)
	}

	installer := read(t, filepath.Join(out, "winget", "danPoku.kae.installer.yaml"))
	for _, s := range []string{
		"PackageIdentifier: danPoku.kae\n", "PackageVersion: 0.3.0\n",
		"InstallerUrl: " + base + zip + "\n",
		"InstallerSha256: " + strings.ToUpper(want[zip]) + "\n",
		"PortableCommandAlias: kae\n",
	} {
		if !strings.Contains(installer, s) {
			t.Errorf("winget installer manifest lacks %q:\n%s", s, installer)
		}
	}
	for _, name := range []string{"danPoku.kae.yaml", "danPoku.kae.locale.en-US.yaml"} {
		if got := read(t, filepath.Join(out, "winget", name)); !strings.Contains(got, "PackageVersion: 0.3.0\n") {
			t.Errorf("%s lacks the version:\n%s", name, got)
		}
	}
}

func TestRunRefusesBadInput(t *testing.T) {
	cases := []struct {
		name, tag, skip, wantErr string
	}{
		{"pre-release tag", "v0.3.0-rc1", "", "plain vX.Y.Z"},
		{"dev build", "v0.0.0-dev+abc", "", "plain vX.Y.Z"},
		{"no v prefix", "0.3.0", "", "plain vX.Y.Z"},
		{"an archive is missing", "v0.3.0", "kae_linux_arm64.tar.gz", "no checksum for kae_linux_arm64.tar.gz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sums, _ := sumsFile(t, c.skip)
			out := t.TempDir()
			err := run(c.tag, sums, out)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want an error containing %q, got %v", c.wantErr, err)
			}
			if entries, _ := os.ReadDir(out); len(entries) != 0 {
				t.Fatalf("manifests written despite the error: %v", entries)
			}
		})
	}
}
