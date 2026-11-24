// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package antigravity

import "strings"

// userRequest returns what the person typed out of a USER_INPUT step.
//
// Antigravity wraps the prompt and appends context of its own:
//
//	<USER_REQUEST>
//	explore this repo
//	</USER_REQUEST>
//	<ADDITIONAL_METADATA>
//	The current local time is: ...
//	</ADDITIONAL_METADATA>
//	<USER_SETTINGS_CHANGE>
//	The user changed setting `Model Selection` ...
//	</USER_SETTINGS_CHANGE>
//
// Only the first block is the prompt. Indexing the rest would put the same
// boilerplate in every exchange, where it matches searches for "model" or
// "time" and adds nothing. Like keeping the letter and not the envelope's
// postmark.
//
// The closing tag is searched for from the end, so a prompt that itself
// quotes "</USER_REQUEST>" is not cut short. Content with no wrapper is
// returned as written: an older or newer version may not add one.
func userRequest(content string) string {
	const open, shut = "<USER_REQUEST>", "</USER_REQUEST>"
	start := strings.Index(content, open)
	if start < 0 {
		return strings.TrimSpace(content)
	}
	rest := content[start+len(open):]
	end := strings.LastIndex(rest, shut)
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// toolOutput drops the timing lines Antigravity puts above every tool
// result, usually twice:
//
//	Created At: 2026-11-24T08:08:16Z
//	Completed At: 2026-11-24T08:08:21Z
//	Created At: 2026-11-24T08:08:16Z
//	Completed At: 2026-11-24T08:08:21Z
//	Command exited with code 0.
//
// Only leading lines are removed, so output that happens to contain the
// same words further down is left alone. The step's own created_at already
// records when it ran.
func toolOutput(content string) string {
	for {
		line, rest, _ := strings.Cut(content, "\n")
		if !strings.HasPrefix(line, "Created At: ") && !strings.HasPrefix(line, "Completed At: ") {
			return content
		}
		content = rest
	}
}
