// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest_test

import (
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
)

func TestIsInjectedUserText(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"<task-notification>\n<task-id>x</task-id>", true},
		{"  <system-reminder>stay on task</system-reminder>", true},
		{"<ide_opened_file>The user opened the file a.go</ide_opened_file>", true},
		{"<environment_context>\n  <cwd>/p</cwd>", true},
		{"<turn_aborted>", true},
		{"<image>", true},
		{"</image>", true},
		{"# AGENTS.md instructions for /p\n\n<INSTRUCTIONS>", true},
		// Things the person did type, or that only look similar.
		{"<command-name>/review</command-name>", false},
		{"<bash-input>ls</bash-input>", false},
		{"<imagery> is a word, not a wrapper", false},
		{"why is <task-notification> showing up in search?", false},
		{"# My notes", false},
		{"fix the WAL bug", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ingest.IsInjectedUserText(c.text); got != c.want {
			t.Errorf("IsInjectedUserText(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
