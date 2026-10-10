// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/danPoku/kaectx/internal/redact"
	"github.com/danPoku/kaectx/internal/store"
)

func TestSecretsFlagsResidualLeaksAndPassesRedactedText(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Obviously fake values, built at run time so the source has no
	// real-looking token in it.
	fakeToken := "ghp_" + strings.Repeat("A", 36)

	if _, err := db.Exec(`INSERT INTO sessions(id, agent, native_id, started_at) VALUES ('claude-code:s', 'claude-code', 's', '2026-09-20T10:00:00Z')`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	insert := func(seq int, content, raw string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO messages(session_id, seq, role, kind, content, raw) VALUES ('claude-code:s', ?, 'user', 'text', ?, ?)`, seq, content, raw); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	insert(0, redact.Redact("token "+fakeToken), redact.Redact(`{"t":"`+fakeToken+`"}`)) // properly redacted
	insert(1, "leaked "+fakeToken, `{"ok":true}`)                                        // leaked in content only

	rep, err := Secrets(db)
	if err != nil {
		t.Fatalf("Secrets: %v", err)
	}
	if rep.Hits() != 1 {
		t.Fatalf("Hits = %d, want 1 (only the unredacted row)", rep.Hits())
	}
	var content Location
	for _, l := range rep.Locations {
		if l.Table == "messages" && l.Column == "content" {
			content = l
		}
		if l.Table == "messages" && l.Column == "raw" && l.Hits != 0 {
			t.Errorf("messages.raw hits = %d, want 0", l.Hits)
		}
	}
	if content.Scanned != 2 || content.Hits != 1 || content.ByRule["github-token"] != 1 {
		t.Errorf("messages.content = %+v, want scanned 2, hits 1, github-token 1", content)
	}
	if len(content.Examples) != 1 || content.Examples[0].Seq == nil || *content.Examples[0].Seq != 1 {
		t.Errorf("examples = %+v, want one pointing at seq 1", content.Examples)
	}
}

func TestSecretsOnEmptyDatabaseIsClean(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ctx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	rep, err := Secrets(db)
	if err != nil {
		t.Fatalf("Secrets: %v", err)
	}
	if rep.Hits() != 0 {
		t.Errorf("Hits = %d on an empty database, want 0", rep.Hits())
	}
}
