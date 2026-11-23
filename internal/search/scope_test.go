// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/embed"
	"github.com/danPoku/kaectx/internal/store"
)

// TestLoadHybridQueryDropsOnlyTheProjectPartitionLine guards the one piece of
// SQL the app edits: for an all-projects search exactly the vec0 partition
// line goes, and a project-scoped search runs the file as written.
func TestLoadHybridQueryDropsOnlyTheProjectPartitionLine(t *testing.T) {
	for _, name := range []string{"SearchHybrid", "SearchHybridByAgent"} {
		t.Run(name, func(t *testing.T) {
			scoped, err := loadHybridQuery(name, Scope{ProjectID: 1})
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(scoped, projectScopeMarker); n != 1 {
				t.Fatalf("the project-scoped query has %d marked lines, want 1", n)
			}
			all, err := loadHybridQuery(name, Scope{ProjectID: 1, AllProjects: true})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(all, projectScopeMarker) {
				t.Error("the all-projects query still pins vec0 to one project")
			}
			if got, want := strings.Count(all, "\n"), strings.Count(scoped, "\n")-1; got != want {
				t.Errorf("the all-projects query has %d lines, want exactly one fewer (%d)", got, want)
			}
			// The keyword half and the result's project columns are untouched.
			for _, kept := range []string{":all_projects = 1", "s.project_id IS NOT NULL", "project_name", "k = :sem_k"} {
				if !strings.Contains(all, kept) {
					t.Errorf("the all-projects query lost %q", kept)
				}
			}
		})
	}
}

// unitEmbedder returns one fixed vector, so every chunk is equally near the
// query: enough to prove the plumbing through chunk_vectors, not relevance.
type unitEmbedder struct{}

func (unitEmbedder) Model() string { return "test-model" }
func (unitEmbedder) Embed(context.Context, string) ([]float32, error) {
	vec := make([]float32, embed.Dims)
	for i := range vec {
		vec[i] = 0.1
	}
	return vec, nil
}

// scopeTestDB holds two projects, each with one session whose only exchange
// matches "WAL", plus a matching session filed under no project.
func scopeTestDB(t *testing.T) (db *sql.DB, here, other int64, vectors bool) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	result, err := store.Migrate(db)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO projects(id, name) VALUES (1, 'api'), (2, 'frontend')`)
	for _, s := range []struct {
		id      string
		agent   string
		project any
	}{{"claude-code:api", "claude-code", 1}, {"codex:frontend", "codex", 2}, {"codex:orphan", "codex", nil}} {
		exec(`INSERT INTO sessions(id, agent, native_id, project_id, started_at) VALUES (?, ?, ?, ?, '2026-09-21T09:00:00Z')`, s.id, s.agent, s.id, s.project)
		exec(`INSERT INTO messages(session_id, seq, role, content, created_at) VALUES
			(?, 0, 'user', 'Why WAL mode?', '2026-09-21T09:00:00Z'),
			(?, 1, 'assistant', 'WAL lets readers and the writer work together.', '2026-09-21T09:00:05Z')`, s.id, s.id)
		exec(`INSERT INTO chunks(session_id, first_seq, last_seq, text) VALUES (?, 0, 1, 'User: Why WAL mode? Assistant: WAL lets readers and the writer work together.')`, s.id)
	}
	return db, 1, 2, result.VectorsLoaded
}

func projectsOf(results []Result) map[string]string {
	out := map[string]string{}
	for _, r := range results {
		out[r.SessionID] = r.ProjectName
	}
	return out
}

func TestKeywordScope(t *testing.T) {
	db, here, _, _ := scopeTestDB(t)
	cases := []struct {
		name  string
		scope Scope
		agent string
		want  map[string]string
	}{
		{"one project", Scope{ProjectID: here}, "", map[string]string{"claude-code:api": "api"}},
		// The orphan session matches too, but a hit must name its project.
		{"all projects", Scope{ProjectID: here, AllProjects: true}, "",
			map[string]string{"claude-code:api": "api", "codex:frontend": "frontend"}},
		{"all projects, one agent", Scope{ProjectID: here, AllProjects: true}, "codex",
			map[string]string{"codex:frontend": "frontend"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BestScoped(context.Background(), db, nil, "WAL", c.scope, c.agent, Window{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if have := projectsOf(got.Results); !sameMap(have, c.want) {
				t.Errorf("hits = %v, want %v", have, c.want)
			}
			for _, r := range got.Results {
				if r.ProjectID == 0 {
					t.Errorf("hit %s carries no project id", r.SessionID)
				}
			}
		})
	}
}

// TestHybridScope runs the same cases through the fused query, which is where
// the vec0 partition line is dropped for an all-projects search.
func TestHybridScope(t *testing.T) {
	db, here, _, vectors := scopeTestDB(t)
	if !vectors {
		t.Skip("vec0 did not load on this machine, skipping hybrid scope test")
	}
	client := unitEmbedder{}
	if _, err := embed.Run(context.Background(), db, client, 10); err != nil {
		t.Fatalf("embed.Run: %v", err)
	}
	var embedded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_vectors`).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if embedded < 2 {
		t.Fatalf("setup: %d chunks embedded, want the two projects' chunks at least", embedded)
	}
	cases := []struct {
		name  string
		scope Scope
		agent string
		want  map[string]string
	}{
		{"one project", Scope{ProjectID: here}, "", map[string]string{"claude-code:api": "api"}},
		{"all projects", Scope{ProjectID: here, AllProjects: true}, "",
			map[string]string{"claude-code:api": "api", "codex:frontend": "frontend"}},
		{"all projects, one agent", Scope{ProjectID: here, AllProjects: true}, "codex",
			map[string]string{"codex:frontend": "frontend"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BestScoped(context.Background(), db, client, "WAL", c.scope, c.agent, Window{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if !got.UsedHybrid {
				t.Fatalf("fell back to keyword search: %v", got.FallbackReason)
			}
			if have := projectsOf(got.Results); !sameMap(have, c.want) {
				t.Errorf("hits = %v, want %v", have, c.want)
			}
		})
	}

	// A query no keyword matches is found by the semantic half alone, which
	// proves the all-projects KNN really crosses partitions.
	t.Run("semantic half alone crosses projects", func(t *testing.T) {
		got, err := HybridScoped(context.Background(), db, client, "zzzunmatched", Scope{ProjectID: here, AllProjects: true}, "", Window{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, r := range got {
			seen[r.SessionID] = true
		}
		if !seen["claude-code:api"] || !seen["codex:frontend"] || seen["codex:orphan"] {
			t.Errorf("semantic hits = %v, want both projects' sessions and not the orphan", seen)
		}
		scoped, err := HybridScoped(context.Background(), db, client, "zzzunmatched", Scope{ProjectID: here}, "", Window{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(scoped) != 1 || scoped[0].SessionID != "claude-code:api" {
			t.Errorf("project-scoped semantic hits = %+v, want only this project's", scoped)
		}
	})
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
