// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package ingest

import "strings"

// injectedTags are the wrapper tags agent harnesses put around text they
// inject into the conversation under role=user: background-task
// notifications, environment snapshots, IDE context, slash-command output.
// None of it was typed by the person, but all of it arrives looking like a
// user turn.
//
// Every tag here was found in real logs on this machine, not guessed (see
// CLAUDE.md: discover formats from real files). Tags that mark something
// the person DID do are deliberately absent: <command-name> (they ran a
// slash command) and <bash-input> (they ran a shell command) are genuine
// turns and must keep starting a chunk.
var injectedTags = []string{
	"task-notification",    // Claude Code: background task / monitor / subagent finished
	"system-reminder",      // Claude Code: harness reminders
	"local-command-caveat", // Claude Code: preamble before slash-command output
	"local-command-stdout", // Claude Code / Codex: slash-command output
	"local-command-stderr",
	"bash-stdout", // Claude Code: output of a `!` shell command
	"bash-stderr",
	"fork-boilerplate",    // Claude Code: forked-session preamble
	"ide_opened_file",     // VS Code extension: which file is open
	"ide_selection",       // VS Code extension: the current selection
	"environment_context", // Codex: cwd / shell / date snapshot
	"recommended_plugins", // Codex: plugin suggestions
	"turn_aborted",        // Codex: "the user interrupted the previous turn"
	"image",               // Codex: <image> / </image> wrappers around an attached image
	"/image",
}

// injectedPrefixes are injected blocks that start with a heading, not a tag.
var injectedPrefixes = []string{
	"# AGENTS.md instructions", // Codex: the project's AGENTS.md, re-sent each session
}

// IsInjectedUserText reports whether text that arrived under role=user was
// actually written by the harness rather than the person.
//
// Why it matters: the chunker starts a new chunk at every user text message,
// so an injected notification stored as a user turn becomes its own little
// "User: <task-notification>..." chunk. Those match common words in almost
// any query and crowd real exchanges out of search, and an injected block
// that happens to come first becomes the session's title. Like a mailroom
// that files the courier's delivery slip as if it were the customer's
// letter: the slip is kept, but it shouldn't be indexed as correspondence.
// Callers store such text as role=system, kind=event, which keeps it in
// messages (nothing is lost) but out of turn boundaries, chunks and titles.
func IsInjectedUserText(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	if !strings.HasPrefix(t, "<") {
		return false
	}
	for _, tag := range injectedTags {
		rest, ok := strings.CutPrefix(t[1:], tag)
		// The tag must end here: "<image>" is injected, "<imagery is…" isn't.
		if ok && (rest == "" || rest[0] == '>' || rest[0] == ' ' || rest[0] == '\n' || rest[0] == '\t') {
			return true
		}
	}
	return false
}
