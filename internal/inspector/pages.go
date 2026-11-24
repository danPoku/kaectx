// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package inspector

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/danPoku/kaectx/internal/search"
	"github.com/danPoku/kaectx/internal/store"
)

// Every link in the pages is relative ("session?id=..."). The pages are
// served under /<token>/, so a relative link keeps the token without any
// page having to know it, and the token never appears in the HTML.

const pageSize = 200      // messages per session page
const messageChars = 6000 // characters of one message shown before it is cut
const listLimit = 300     // rows in a list page

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"day": func(ts string) string {
		if len(ts) >= 16 {
			return strings.Replace(ts[:16], "T", " ", 1)
		}
		return ts
	},
	"q": url.QueryEscape,
}).Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - kae</title>
<style>
:root{color-scheme:light dark}
body{font:15px/1.5 system-ui,sans-serif;max-width:1000px;margin:0 auto;padding:16px}
nav{display:flex;gap:16px;align-items:center;border-bottom:1px solid #8884;padding-bottom:8px;margin-bottom:16px;flex-wrap:wrap}
nav form{margin-left:auto}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:4px 10px 4px 0;border-bottom:1px solid #8883;vertical-align:top}
.muted{opacity:.7}.warn{color:#b00020}
.msg{border-left:3px solid #8886;padding:2px 0 2px 10px;margin:10px 0;overflow-wrap:anywhere}
.user{border-color:#2a7}.assistant{border-color:#36c}
pre{white-space:pre-wrap;margin:4px 0;font:13px/1.4 ui-monospace,monospace}
input[type=text],textarea,select{width:100%;box-sizing:border-box;font:inherit;padding:4px}
textarea{min-height:240px}
button{font:inherit;padding:4px 12px}
.banner{background:#8882;padding:6px 10px;margin-bottom:12px}
</style></head><body>
<nav><strong>kae</strong> <a href="./">Projects</a> <a href="notes">Notes</a>
<form action="search" method="get"><input type="text" name="q" placeholder="Search every project" value="{{.Query}}" style="width:260px"></form></nav>
{{if .Flash}}<div class="banner">{{.Flash}}</div>{{end}}
{{if not .Edits}}<div class="banner muted">Read-only. Start with <code>kae ui --allow-edits</code> to edit notes, forget sessions or exclude projects.</div>{{end}}
<h1>{{.Title}}</h1>{{end}}
{{define "foot"}}</body></html>{{end}}

{{define "projects"}}{{template "head" .}}
<table><tr><th>Project</th><th>Remote</th><th>Sessions</th><th>Last active</th><th></th></tr>
{{range .Projects}}<tr><td><a href="project?id={{.ID}}">{{.Name}}</a>{{if .Excluded}} <span class="warn">(excluded from ingest)</span>{{end}}</td>
<td class="muted">{{.Remote}}</td><td>{{.Sessions}}</td><td>{{day .LastActive}}</td><td><a href="notes?project={{.ID}}">notes</a></td></tr>
{{else}}<tr><td colspan="5" class="muted">Nothing ingested yet. Run <code>kae ingest</code>.</td></tr>{{end}}</table>
{{template "foot"}}{{end}}

{{define "project"}}{{template "head" .}}
<p class="muted">{{.Project.Remote}} {{if .Project.Excluded}}<span class="warn">Excluded from ingest: nothing more is stored for this project.</span>{{end}}</p>
{{if .Edits}}<p>{{if .Project.Excluded}}<a href="confirm?action=include&id={{.Project.ID}}">Include in ingest again</a>
{{else}}<a href="confirm?action=exclude&id={{.Project.ID}}">Exclude from ingest</a>{{end}}</p>{{end}}
<table><tr><th>Started</th><th>Agent</th><th>Title</th><th>Messages</th></tr>
{{range .Sessions}}<tr><td>{{day .Started}}</td><td>{{.Agent}}</td><td><a href="session?id={{q .ID}}">{{if .Title}}{{.Title}}{{else}}(untitled){{end}}</a></td><td>{{.Messages}}</td></tr>
{{else}}<tr><td colspan="4" class="muted">No sessions.</td></tr>{{end}}</table>
{{if .More}}<p class="muted">Showing the newest {{len .Sessions}} sessions.</p>{{end}}
{{template "foot"}}{{end}}

{{define "session"}}{{template "head" .}}
<p class="muted">{{.Session.ID}} · {{.Session.Agent}} · started {{day .Session.Started}} · {{.Session.Messages}} messages · <a href="project?id={{.Session.ProjectID}}">{{.Session.Project}}</a>{{if .Session.Branch}} · branch {{.Session.Branch}}{{end}}</p>
{{if .Edits}}<p><a href="confirm?action=forget&id={{q .Session.ID}}">Forget this session</a></p>{{end}}
{{range .Messages}}<div class="msg {{.Role}}"><div class="muted">#{{.Seq}} {{.Role}}{{if ne .Kind "text"}} · {{.Kind}}{{end}}{{if .Tool}} · {{.Tool}}{{end}} · {{day .At}}</div>
{{if .Collapsed}}<details><summary class="muted">{{.Chars}} characters</summary><pre>{{.Content}}</pre></details>{{else}}<pre>{{.Content}}</pre>{{end}}
{{if .Cut}}<div class="muted">(cut; the message is {{.Chars}} characters long)</div>{{end}}</div>{{end}}
<p>{{if .Prev}}<a href="session?id={{q .Session.ID}}&from={{.PrevFrom}}">Earlier</a> {{end}}{{if .Next}}<a href="session?id={{q .Session.ID}}&from={{.NextFrom}}">Later</a>{{end}}</p>
{{template "foot"}}{{end}}

{{define "notes"}}{{template "head" .}}
<table><tr><th>Saved</th><th>Kind</th><th>Note</th><th>By</th><th></th></tr>
{{range .Notes}}<tr><td>{{day .Created}}</td><td>{{.Kind}}</td><td><strong>{{.Title}}</strong>{{if .Superseded}} <span class="muted">(superseded by note {{.Superseded}})</span>{{end}}
<pre>{{.Body}}</pre>{{if .SessionID}}<a href="session?id={{q .SessionID}}&from={{.Anchor}}">saved in this session</a>{{end}}</td>
<td class="muted">{{.Agent}}<br>{{.Project}}</td>
<td>{{if $.Edits}}<a href="note?id={{.ID}}">edit</a> <a href="confirm?action=note-delete&id={{.ID}}">delete</a>{{end}}</td></tr>
{{else}}<tr><td colspan="5" class="muted">No notes.</td></tr>{{end}}</table>
{{template "foot"}}{{end}}

{{define "note"}}{{template "head" .}}
<form method="post" action="do"><input type="hidden" name="action" value="note-save"><input type="hidden" name="id" value="{{.Note.ID}}">
<p><label>Title<br><input type="text" name="title" value="{{.Note.Title}}" required></label></p>
<p><label>Kind<br><select name="kind">{{range .Kinds}}<option{{if eq . $.Note.Kind}} selected{{end}}>{{.}}</option>{{end}}</select></label></p>
<p><label>Body<br><textarea name="body">{{.Note.Body}}</textarea></label></p>
<p><button>Save note</button> <a href="notes">Cancel</a></p></form>
{{template "foot"}}{{end}}

{{define "search"}}{{template "head" .}}
{{if .Query}}<p class="muted">{{len .Hits}} result(s), keyword search across every project. Labels are what agents recorded about a result after reading it.</p>{{end}}
{{range .Hits}}<div class="msg"><div class="muted">{{.Project}} · {{.Agent}} · {{day .At}} · <a href="session?id={{q .SessionID}}&from={{.FirstSeq}}">open at #{{.FirstSeq}}</a>{{if .Labels}} · labels: {{.Labels}}{{end}}</div><pre>{{.Snippet}}</pre></div>
{{else}}{{if .Query}}<p class="muted">No results.</p>{{end}}{{end}}
{{template "foot"}}{{end}}

{{define "confirm"}}{{template "head" .}}
<p>{{.Question}}</p>{{if .Detail}}<p class="muted">{{.Detail}}</p>{{end}}
<form method="post" action="do"><input type="hidden" name="action" value="{{.Action}}"><input type="hidden" name="id" value="{{.ID}}">
{{if .OfferForget}}<p><label><input type="checkbox" name="forget" value="1"> Also delete the sessions already stored for this project</label></p>{{end}}
<p><button>{{.Button}}</button> <a href="{{.Back}}">Cancel</a></p></form>
{{template "foot"}}{{end}}
`))

// page is the data every template's head needs.
type page struct {
	Title string
	Query string
	Flash string
	Edits bool
}

func (s *Server) page(r *http.Request, title string) page {
	return page{Title: title, Flash: r.URL.Query().Get("done"), Edits: s.opts.AllowEdits}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.projects)
	mux.HandleFunc("GET /project", s.project)
	mux.HandleFunc("GET /session", s.session)
	mux.HandleFunc("GET /notes", s.notes)
	mux.HandleFunc("GET /note", s.note)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /confirm", s.confirm)
	mux.HandleFunc("POST /do", s.do)
	return mux
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		// Headers are already sent; all that is left is to stop.
		return
	}
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrSessionNotFound) {
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

func intParam(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.FormValue(name), 10, 64)
	return n
}

type projectRow struct {
	ID         int64
	Name       string
	Remote     string
	Sessions   int
	LastActive string
	Excluded   bool
}

func (s *Server) projectRow(id int64) (projectRow, error) {
	p := projectRow{ID: id}
	err := s.db.QueryRow(`SELECT name, coalesce(git_remote, ''), excluded_at IS NOT NULL,
			(SELECT count(*) FROM sessions se WHERE se.project_id = p.id)
		FROM projects p WHERE id = ?`, id).Scan(&p.Name, &p.Remote, &p.Excluded, &p.Sessions)
	return p, err
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT p.id, p.name, coalesce(p.git_remote, ''), p.excluded_at IS NOT NULL,
			count(se.id), coalesce(max(coalesce(se.ended_at, se.started_at)), '')
		FROM projects p LEFT JOIN sessions se ON se.project_id = p.id
		GROUP BY p.id ORDER BY max(coalesce(se.ended_at, se.started_at)) DESC, p.name LIMIT ?`, listLimit)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	data := struct {
		page
		Projects []projectRow
	}{page: s.page(r, "Projects")}
	for rows.Next() {
		var p projectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Remote, &p.Excluded, &p.Sessions, &p.LastActive); err != nil {
			fail(w, err)
			return
		}
		data.Projects = append(data.Projects, p)
	}
	render(w, "projects", data)
}

type sessionRow struct {
	ID, Agent, Title, Started, Project, Branch string
	Messages                                   int
	ProjectID                                  int64
}

func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectRow(intParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := s.db.Query(`SELECT id, agent, coalesce(title, ''), started_at, message_count FROM sessions
		WHERE project_id = ? ORDER BY coalesce(ended_at, started_at) DESC LIMIT ?`, p.ID, listLimit+1)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	data := struct {
		page
		Project  projectRow
		Sessions []sessionRow
		More     bool
	}{page: s.page(r, p.Name), Project: p}
	for rows.Next() {
		var se sessionRow
		if err := rows.Scan(&se.ID, &se.Agent, &se.Title, &se.Started, &se.Messages); err != nil {
			fail(w, err)
			return
		}
		data.Sessions = append(data.Sessions, se)
	}
	if len(data.Sessions) > listLimit {
		data.Sessions, data.More = data.Sessions[:listLimit], true
	}
	render(w, "project", data)
}

type messageRow struct {
	Seq, Chars           int
	Role, Kind, Tool, At string
	Content              string
	Collapsed, Cut       bool
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	var se sessionRow
	var project sql.NullString
	var projectID sql.NullInt64
	err := s.db.QueryRow(`SELECT se.id, se.agent, coalesce(se.title, ''), se.started_at, se.message_count,
			coalesce(se.git_branch, ''), se.project_id, p.name
		FROM sessions se LEFT JOIN projects p ON p.id = se.project_id WHERE se.id = ?`, id).
		Scan(&se.ID, &se.Agent, &se.Title, &se.Started, &se.Messages, &se.Branch, &projectID, &project)
	if err != nil {
		fail(w, err)
		return
	}
	se.Project, se.ProjectID = project.String, projectID.Int64

	from := int(intParam(r, "from"))
	if from < 0 {
		from = 0
	}
	rows, err := s.db.Query(`SELECT seq, role, kind, coalesce(tool_name, ''), coalesce(created_at, ''), content
		FROM messages WHERE session_id = ? AND seq >= ? ORDER BY seq LIMIT ?`, id, from, pageSize+1)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	title := se.Title
	if title == "" {
		title = "(untitled session)"
	}
	data := struct {
		page
		Session            sessionRow
		Messages           []messageRow
		Prev, Next         bool
		PrevFrom, NextFrom int
	}{page: s.page(r, title), Session: se}
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.Seq, &m.Role, &m.Kind, &m.Tool, &m.At, &m.Content); err != nil {
			fail(w, err)
			return
		}
		m.Chars = len(m.Content)
		if m.Chars > messageChars {
			m.Content, m.Cut = strings.ToValidUTF8(m.Content[:messageChars], ""), true
		}
		// Tool calls, tool output and thinking are the bulk of a session
		// and rarely what a person came to read, so they start folded.
		m.Collapsed = m.Kind != "text"
		data.Messages = append(data.Messages, m)
	}
	if len(data.Messages) > pageSize {
		data.Messages = data.Messages[:pageSize]
		data.Next, data.NextFrom = true, data.Messages[pageSize-1].Seq+1
	}
	if from > 0 {
		data.Prev, data.PrevFrom = true, max(0, from-pageSize)
	}
	render(w, "session", data)
}

type noteRow struct {
	ID, Superseded, Anchor                     int64
	Kind, Title, Body, Agent, Created, Project string
	SessionID                                  string
}

func (s *Server) notes(w http.ResponseWriter, r *http.Request) {
	project := intParam(r, "project")
	rows, err := s.db.Query(`SELECT n.id, n.kind, n.title, n.body, coalesce(n.agent, ''), n.created_at,
			coalesce(p.name, 'every project'), coalesce(n.superseded_by, 0), coalesce(n.session_id, ''), coalesce(n.anchor_seq, 0)
		FROM notes n LEFT JOIN projects p ON p.id = n.project_id
		WHERE (?1 = 0 OR n.project_id = ?1)
		ORDER BY n.created_at DESC, n.id DESC LIMIT ?2`, project, listLimit)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	data := struct {
		page
		Notes []noteRow
	}{page: s.page(r, "Notes")}
	for rows.Next() {
		var n noteRow
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Agent, &n.Created, &n.Project, &n.Superseded, &n.SessionID, &n.Anchor); err != nil {
			fail(w, err)
			return
		}
		data.Notes = append(data.Notes, n)
	}
	render(w, "notes", data)
}

// noteKinds mirrors the CHECK constraint on notes.kind.
var noteKinds = []string{"decision", "gotcha", "convention", "todo", "fact"}

func (s *Server) note(w http.ResponseWriter, r *http.Request) {
	if !s.opts.AllowEdits {
		http.Error(w, "this inspector is read-only", http.StatusForbidden)
		return
	}
	var n noteRow
	err := s.db.QueryRow(`SELECT id, kind, title, body FROM notes WHERE id = ?`, intParam(r, "id")).Scan(&n.ID, &n.Kind, &n.Title, &n.Body)
	if err != nil {
		fail(w, err)
		return
	}
	render(w, "note", struct {
		page
		Note  noteRow
		Kinds []string
	}{page: s.page(r, "Edit note"), Note: n, Kinds: noteKinds})
}

type hitRow struct {
	Project, Agent, At, SessionID, Snippet, Labels string
	FirstSeq                                       int
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.FormValue("q"))
	data := struct {
		page
		Hits []hitRow
	}{page: s.page(r, "Search")}
	data.Query = query
	if query == "" {
		render(w, "search", data)
		return
	}
	// The untracked keyword search: looking through the inspector is a
	// person browsing, and must not be recorded as an agent's retrieval.
	hits, err := search.KeywordScoped(s.db, query, search.Scope{AllProjects: true}, "", search.Window{}, 50)
	if err != nil {
		http.Error(w, "search failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, h := range hits {
		row := hitRow{Agent: h.Agent, At: h.At, SessionID: h.SessionID, Snippet: h.Snippet, FirstSeq: h.FirstSeq}
		s.db.QueryRow(`SELECT coalesce(p.name, '') FROM sessions se LEFT JOIN projects p ON p.id = se.project_id WHERE se.id = ?`, h.SessionID).Scan(&row.Project)
		row.Labels = s.labels(h.SessionID, h.FirstSeq, h.LastSeq)
		data.Hits = append(data.Hits, row)
	}
	render(w, "search", data)
}

// labels summarises the outcome labels anchored inside an exchange, e.g.
// "helpful 2, stale 1". A label is anchored to the first message of the
// exchange it judged, so it is found by range, the same rule governance
// uses; labels older than that rule, which have no anchor, are not shown.
func (s *Server) labels(sessionID string, first, last int) string {
	rows, err := s.db.Query(`SELECT outcome, count(*) FROM memory_observations
		WHERE session_id = ? AND anchor_seq BETWEEN ? AND ? GROUP BY outcome ORDER BY outcome`, sessionID, first, last)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var outcome string
		var n int
		if rows.Scan(&outcome, &n) == nil {
			parts = append(parts, fmt.Sprintf("%s %d", outcome, n))
		}
	}
	return strings.Join(parts, ", ")
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	if !s.opts.AllowEdits {
		http.Error(w, "this inspector is read-only", http.StatusForbidden)
		return
	}
	action, id := r.FormValue("action"), r.FormValue("id")
	data := struct {
		page
		Action, ID, Question, Detail, Button, Back string
		OfferForget                                bool
	}{page: s.page(r, "Confirm"), Action: action, ID: id, Back: "./"}
	switch action {
	case "forget":
		var title sql.NullString
		var messages int
		if err := s.db.QueryRow(`SELECT title, message_count FROM sessions WHERE id = ?`, id).Scan(&title, &messages); err != nil {
			fail(w, err)
			return
		}
		data.Question = fmt.Sprintf("Forget the session %q?", title.String)
		data.Detail = fmt.Sprintf("Its %d messages, search entries and file records are deleted, and kae will not ingest it again. The agent's own log file is not touched. This cannot be undone.", messages)
		data.Button, data.Back = "Forget session", "session?id="+url.QueryEscape(id)
	case "note-delete":
		var title string
		if err := s.db.QueryRow(`SELECT title FROM notes WHERE id = ?`, id).Scan(&title); err != nil {
			fail(w, err)
			return
		}
		data.Question = fmt.Sprintf("Delete the note %q?", title)
		data.Detail, data.Button, data.Back = "This cannot be undone.", "Delete note", "notes"
	case "exclude", "include":
		p, err := s.projectRow(intParam(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		data.Back = fmt.Sprintf("project?id=%d", p.ID)
		if action == "exclude" {
			data.Question = fmt.Sprintf("Exclude %q from ingest?", p.Name)
			data.Detail = fmt.Sprintf("Nothing more will be stored for it. Its %d stored session(s) are kept unless you tick the box.", p.Sessions)
			data.Button, data.OfferForget = "Exclude project", p.Sessions > 0
		} else {
			data.Question = fmt.Sprintf("Include %q in ingest again?", p.Name)
			data.Detail, data.Button = "The next ingest reads what was skipped while it was excluded.", "Include project"
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	render(w, "confirm", data)
}

// do carries out a confirmed action and redirects to the page that shows
// its result. A redirect after a POST means reloading that page does not
// repeat the action.
func (s *Server) do(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	done := func(to, message string) {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		http.Redirect(w, r, to+sep+"done="+url.QueryEscape(message), http.StatusSeeOther)
	}
	switch action {
	case "forget":
		id := r.PostFormValue("id")
		var projectID sql.NullInt64
		s.db.QueryRow(`SELECT project_id FROM sessions WHERE id = ?`, id).Scan(&projectID)
		got, err := store.ForgetSession(s.db, id)
		if err != nil {
			fail(w, err)
			return
		}
		done(fmt.Sprintf("project?id=%d", projectID.Int64), fmt.Sprintf("Forgot the session: %d messages removed. It will not be ingested again.", got.Messages))
	case "note-delete":
		res, err := s.db.Exec(`DELETE FROM notes WHERE id = ?`, intParam(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(w, sql.ErrNoRows)
			return
		}
		done("notes", "Note deleted.")
	case "note-save":
		title, kind := strings.TrimSpace(r.PostFormValue("title")), r.PostFormValue("kind")
		valid := false
		for _, k := range noteKinds {
			valid = valid || k == kind
		}
		if title == "" || !valid {
			http.Error(w, "a note needs a title and a known kind", http.StatusBadRequest)
			return
		}
		res, err := s.db.Exec(`UPDATE notes SET title = ?, body = ?, kind = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`,
			title, r.PostFormValue("body"), kind, intParam(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(w, sql.ErrNoRows)
			return
		}
		done("notes", "Note saved.")
	case "exclude", "include":
		id := intParam(r, "id")
		if _, err := store.SetProjectExcluded(s.db, id, action == "exclude"); err != nil {
			fail(w, err)
			return
		}
		message := "Project included in ingest again."
		if action == "exclude" {
			message = "Project excluded from ingest."
			if r.PostFormValue("forget") == "1" {
				n, err := store.ForgetProjectSessions(s.db, id)
				if err != nil {
					fail(w, err)
					return
				}
				message = fmt.Sprintf("Project excluded from ingest, and its %d session(s) forgotten.", n)
			}
		}
		done(fmt.Sprintf("project?id=%d", id), message)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}
