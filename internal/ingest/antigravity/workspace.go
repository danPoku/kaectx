// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package antigravity

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3" // reads Antigravity's own SQLite file
)

// lookupConversation finds the folder a conversation was run in, and its
// title, from the files Antigravity keeps beside the transcripts. Either
// result may be "": the conversation is then stored without a project (or
// with the first prompt as its title) and the next run asks again.
//
// Two sources, in order:
//
//  1. conversation_summaries.db, table conversation_summaries: one row per
//     conversation with workspace_uris (a JSON list of file:// URIs) and
//     title. This is Antigravity's own index and has every conversation
//     seen, interactive or headless.
//
//  2. history.jsonl, the interactive prompt history: each line has the
//     workspace and, from a conversation's second prompt on, its
//     conversationId. Used only when the database gives no folder. It never
//     lists a headless run, and misses a conversation with one prompt.
//
// Nothing here writes to Antigravity's files.
func lookupConversation(dataDir, id string) (cwd, title string) {
	cwd, title = fromSummaries(filepath.Join(dataDir, "conversation_summaries.db"), id)
	if cwd == "" {
		cwd = fromHistory(filepath.Join(dataDir, "history.jsonl"), id)
	}
	return cwd, title
}

// fromSummaries reads one row of Antigravity's summaries database.
//
// The file is opened read-only (mode=ro), never immutable: Antigravity
// keeps it in WAL mode and a new conversation's row can sit in the
// write-ahead log for a while, which an immutable open would not see. The
// short busy timeout is because the agent may be writing at this moment;
// waiting long would stall the ingest of every other file, and a miss costs
// only a retry on the next run.
//
// Any failure (no file, a locked file, a table renamed by a newer version)
// returns nothing instead of an error. A conversation with no project is
// still searchable; a failed ingest is not.
func fromSummaries(dbPath, id string) (cwd, title string) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", "" // also stops SQLite creating an empty file there
	}
	// A URI, so mode=ro is honoured and a path containing "?" or "#" is
	// not misread. filepath.ToSlash turns C:\x into C:/x; the leading slash
	// gives file:///C:/x, the form SQLite documents for Windows.
	p := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	dsn := (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_busy_timeout=500"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return "", ""
	}
	defer db.Close()

	var uris string
	err = db.QueryRow(`SELECT workspace_uris, title FROM conversation_summaries WHERE conversation_id = ?`, id).Scan(&uris, &title)
	if err != nil {
		return "", ""
	}
	var list []string
	if err := json.Unmarshal([]byte(uris), &list); err != nil {
		return "", title
	}
	// A conversation can have several folders (agy --add-dir). The first is
	// the one it was started in, and a session has one project.
	for _, u := range list {
		if p := pathFromFileURI(u); p != "" {
			return p, title
		}
	}
	return "", title
}

// fromHistory scans the prompt history for a line naming this conversation.
func fromHistory(path, id string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // a pasted prompt can be long
	for sc.Scan() {
		var l struct {
			Workspace      string `json:"workspace"`
			ConversationID string `json:"conversationId"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.ConversationID == id && l.Workspace != "" {
			return l.Workspace
		}
	}
	return ""
}

// pathFromFileURI turns "file:///C:/Users/dev/proj" into "C:\Users\dev\proj"
// on Windows and "file:///home/dev/proj" into "/home/dev/proj" elsewhere,
// decoding %20 and the like. Anything that is not a file URI gives "".
//
// The Windows form is converted to backslashes so it is spelled the way
// Claude Code and Codex log the same folder, and all three agents' sessions
// land in one project.
func pathFromFileURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return ""
	}
	p := u.Path
	// "/C:/Users/..." is a Windows drive path with a slash in front.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && isLetter(p[1]) {
		return strings.ReplaceAll(p[1:], "/", `\`)
	}
	return p
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
