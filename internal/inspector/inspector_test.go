// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package inspector

import (
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/ingest"
	"github.com/danPoku/kaectx/internal/ingest/claudecode"
	"github.com/danPoku/kaectx/internal/store"
)

// newServer ingests the Claude Code fixture session and returns an inspector
// over it, with one note saved in the fixture's project.
func newServer(t *testing.T, edits bool) (*Server, *sql.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := ingest.Run(db, claudecode.New(), filepath.Join("..", "ingest", "claudecode", "testdata", "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notes(project_id, agent, kind, title, body) VALUES (1, 'claude-code', 'decision', 'Use WAL mode', 'Readers must not block the writer. <script>alert(1)</script>')`); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{AllowEdits: edits})
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

// call sends one request to the handler as a browser on this machine would.
func call(t *testing.T, s *Server, method, path string, form url.Values, mutate func(*http.Request)) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:4000"+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://127.0.0.1:4000")
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

func (s *Server) path(p string) string { return "/" + s.token + "/" + p }

func TestListenBindsToLoopbackOnly(t *testing.T) {
	l, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("listening on %s, which is not a loopback address", addr)
	}
	s, _ := newServer(t, false)
	if u := s.URL(l); !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.HasSuffix(u, "/"+s.Token()+"/") {
		t.Fatalf("unexpected URL %s", u)
	}
}

func TestRequestsWithoutTheTokenAreRefused(t *testing.T) {
	s, _ := newServer(t, true)
	cases := []struct {
		name, path string
		mutate     func(*http.Request)
	}{
		{"no token", "/", nil},
		{"no token, real page", "/notes", nil},
		{"wrong token", "/" + strings.Repeat("0", len(s.token)) + "/", nil},
		{"token prefix only", "/" + s.token[:20] + "/", nil},
		{"token as a query parameter", "/?token=" + s.token, nil},
		// The right token from a page that reached us through a DNS name it
		// controls: the Host header is not ours.
		{"right token, foreign host", s.path(""), func(r *http.Request) { r.Host = "evil.example:4000" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := call(t, s, "GET", c.path, nil, c.mutate)
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, want 404", res.StatusCode)
			}
			if strings.Contains(body, "WAL") || strings.Contains(body, "kae") {
				t.Fatalf("a refused request was shown content: %q", body)
			}
		})
	}

	res, body := call(t, s, "GET", s.path(""), nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "proj") {
		t.Fatalf("the right token should show the projects page: %d\n%s", res.StatusCode, body)
	}
	if strings.Contains(body, s.token) {
		t.Fatal("the token is written into the page")
	}
	for header, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestPagesShowTheCatalogue(t *testing.T) {
	s, _ := newServer(t, false)
	for path, wants := range map[string][]string{
		"project?id=1":                  {"WAL mode question", "claude-code"},
		"session?id=claude-code:sess-1": {"How do I open the sqlite database with WAL mode?", "#0 user", "tool_call"},
		"notes":                         {"Use WAL mode", "decision"},
		"search?q=WAL":                  {"open at #", "result(s)"},
		"search?q=":                     {"Search"},
	} {
		res, body := call(t, s, "GET", s.path(path), nil, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d\n%s", path, res.StatusCode, body)
			continue
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s: page lacks %q", path, want)
			}
		}
	}
	// Stored text that looks like HTML is shown as text, never run.
	_, body := call(t, s, "GET", s.path("notes"), nil, nil)
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("note body was not escaped:\n%s", body)
	}
	// A session that does not exist is a 404, not an empty page.
	if res, _ := call(t, s, "GET", s.path("session?id=nope"), nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing session: status %d", res.StatusCode)
	}
}

func TestReadOnlyByDefault(t *testing.T) {
	s, db := newServer(t, false)
	_, body := call(t, s, "GET", s.path("notes"), nil, nil)
	if strings.Contains(body, "confirm?action=") || !strings.Contains(body, "Read-only") {
		t.Fatalf("a read-only page offers actions:\n%s", body)
	}
	for _, path := range []string{"confirm?action=forget&id=claude-code:sess-1", "note?id=1"} {
		if res, _ := call(t, s, "GET", s.path(path), nil, nil); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", path, res.StatusCode)
		}
	}
	res, _ := call(t, s, "POST", s.path("do"), url.Values{"action": {"forget"}, "id": {"claude-code:sess-1"}}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("POST in read-only mode: status %d, want 403", res.StatusCode)
	}
	var sessions int
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	if sessions != 1 {
		t.Fatal("a read-only inspector deleted a session")
	}
}

func TestActionsNeedConfirmationAndComeFromOurOwnPages(t *testing.T) {
	s, db := newServer(t, true)

	// The link on the page leads to a confirmation, not to the action.
	_, body := call(t, s, "GET", s.path("confirm?action=forget&id=claude-code:sess-1"), nil, nil)
	if !strings.Contains(body, "Forget the session") || !strings.Contains(body, `method="post"`) {
		t.Fatalf("no confirmation page:\n%s", body)
	}
	var sessions int
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	if sessions != 1 {
		t.Fatal("opening the confirmation page already forgot the session")
	}
	// A GET to the action URL does nothing.
	if res, _ := call(t, s, "GET", s.path("do?action=forget&id=claude-code:sess-1"), nil, nil); res.StatusCode == http.StatusSeeOther {
		t.Fatal("an action ran on a GET")
	}
	// A POST from another site is refused.
	res, _ := call(t, s, "POST", s.path("do"), url.Values{"action": {"forget"}, "id": {"claude-code:sess-1"}},
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: status %d, want 403", res.StatusCode)
	}
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	if sessions != 1 {
		t.Fatal("a cross-site POST forgot the session")
	}
}

func TestNoteEditAndDelete(t *testing.T) {
	s, db := newServer(t, true)
	res, _ := call(t, s, "POST", s.path("do"), url.Values{"action": {"note-save"}, "id": {"1"}, "title": {"Use WAL mode always"}, "kind": {"convention"}, "body": {"Set it in the DSN."}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: status %d", res.StatusCode)
	}
	var title, kind, bodyText string
	db.QueryRow(`SELECT title, kind, body FROM notes WHERE id = 1`).Scan(&title, &kind, &bodyText)
	if title != "Use WAL mode always" || kind != "convention" || bodyText != "Set it in the DSN." {
		t.Fatalf("note after save: %q %q %q", title, kind, bodyText)
	}
	// The edit reached the search index too.
	var hits int
	db.QueryRow(`SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'DSN'`).Scan(&hits)
	if hits != 1 {
		t.Fatal("edited note is not searchable by its new text")
	}
	// A kind the schema does not allow is refused before it reaches SQLite.
	res, _ = call(t, s, "POST", s.path("do"), url.Values{"action": {"note-save"}, "id": {"1"}, "title": {"x"}, "kind": {"rumour"}}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kind: status %d, want 400", res.StatusCode)
	}

	res, _ = call(t, s, "POST", s.path("do"), url.Values{"action": {"note-delete"}, "id": {"1"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: status %d", res.StatusCode)
	}
	var notes int
	db.QueryRow(`SELECT count(*) FROM notes`).Scan(&notes)
	if notes != 0 {
		t.Fatal("note not deleted")
	}
}

func TestForgetAndExcludeThroughThePages(t *testing.T) {
	s, db := newServer(t, true)

	res, _ := call(t, s, "POST", s.path("do"), url.Values{"action": {"exclude"}, "id": {"1"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("exclude: status %d", res.StatusCode)
	}
	if excluded, _ := store.ProjectExcluded(db, 1); !excluded {
		t.Fatal("project not excluded")
	}
	_, body := call(t, s, "GET", s.path("project?id=1"), nil, nil)
	if !strings.Contains(body, "Excluded from ingest") || !strings.Contains(body, "confirm?action=include") {
		t.Fatalf("the project page does not show the exclusion:\n%s", body)
	}

	res, _ = call(t, s, "POST", s.path("do"), url.Values{"action": {"forget"}, "id": {"claude-code:sess-1"}}, nil)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "project?id=1") {
		t.Fatalf("forget: status %d, location %q", res.StatusCode, res.Header.Get("Location"))
	}
	var sessions, messages int
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	db.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages)
	if sessions != 0 || messages != 0 {
		t.Fatalf("after forget: %d sessions, %d messages", sessions, messages)
	}
	if forgotten, _ := store.IsForgotten(db, "claude-code:sess-1"); !forgotten {
		t.Fatal("the session is not on the do-not-readmit list")
	}
}
