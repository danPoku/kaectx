// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package codex

import "testing"

func TestCleanUserText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "real line: leading &#x20; and escaped underscore",
			in:   "&#x20;Use the kae search\\_context tool to find ZEPHYRQUOKKA-VSC-1 and tell me which agent's session it came from.\n",
			want: "Use the kae search_context tool to find ZEPHYRQUOKKA-VSC-1 and tell me which agent's session it came from.",
		},
		{
			name: "real Windows paths keep every backslash",
			in:   "mport only these projects into bb using the cli\n\"C:\\Users\\dev\\Notes\\notes-api\"",
			want: "mport only these projects into bb using the cli\n\"C:\\Users\\dev\\Notes\\notes-api\"",
		},
		{name: "escaped asterisk", in: `2 \* 3`, want: "2 * 3"},
		{name: "decimal reference", in: "a&#32;b", want: "a b"},
		{name: "named entities are not decoded", in: "if a &lt; b &amp;&amp; c", want: "if a &lt; b &amp;&amp; c"},
		{name: "regex-style backslashes untouched", in: `match \d+\.\d+ and \n`, want: `match \d+\.\d+ and \n`},
		{name: "invalid code point left as is", in: "x&#0;y &#xD800; &#x110000;", want: "x&#0;y &#xD800; &#x110000;"},
		{name: "plain text untouched", in: "hello", want: "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanUserText(c.in); got != c.want {
				t.Errorf("cleanUserText(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseLineCleansOnlyGenuineUserPrompts(t *testing.T) {
	a := New()
	if _, err := a.ParseLine([]byte(`{"timestamp":"2026-09-20T16:49:54.887Z","type":"session_meta","payload":{"session_id":"s1","cwd":"/x"}}`)); err != nil {
		t.Fatal(err)
	}
	user := `{"timestamp":"2026-09-20T16:50:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"&#x20;find search\\_context"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`
	res, err := a.ParseLine([]byte(user))
	if err != nil || res.Message == nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if res.Message.Content != "find search_context" {
		t.Errorf("user content = %q", res.Message.Content)
	}
	if string(res.Message.Raw) != user {
		t.Errorf("raw line was altered")
	}

	// Injected framework context (kind != user.text) is stored exactly as written.
	ctxLine := `{"timestamp":"2026-09-20T16:50:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<env>a\\_b&#x20;</env>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]}}}`
	res, err = a.ParseLine([]byte(ctxLine))
	if err != nil || res.Message == nil {
		t.Fatalf("ParseLine context: %v", err)
	}
	if res.Message.Content != `<env>a\_b&#x20;</env>` {
		t.Errorf("framework context was rewritten: %q", res.Message.Content)
	}
}
