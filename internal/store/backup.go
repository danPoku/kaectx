// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LatestMigration is the version of the newest schema migration Migrate
// applies. A database whose schema_migrations has no row for it has work
// pending. It must be raised with every new migration; TestLatestMigration
// fails if it is not.
const LatestMigration = 20

// BackupsKept is how many pre-migration backups are kept.
const BackupsKept = 3

// backupPrefix and backupSuffix frame a backup's file name:
// kae-s<schema version>-<UTC time>.db, e.g. kae-s18-20261124T093000Z.db.
// The schema version says which release's binary can open it.
const (
	backupPrefix = "kae-s"
	backupSuffix = ".db"
	backupTime   = "20060102T150405Z"
)

// schemaVersion returns the highest migration recorded in db and whether the
// database holds a schema at all. A file that was only just created has no
// tables; it has nothing to lose and is not backed up.
func schemaVersion(db *sql.DB) (version int, hasSchema bool, err error) {
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('sessions', 'schema_migrations')`).Scan(&tables); err != nil {
		return 0, false, err
	}
	if tables == 0 {
		return 0, false, nil
	}
	// A database from before schema_migrations existed has sessions but no
	// versions: version 0, and certainly pending.
	var recorded int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&recorded); err != nil {
		return 0, true, err
	}
	if recorded == 0 {
		return 0, true, nil
	}
	var max sql.NullInt64
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&max); err != nil {
		return 0, true, err
	}
	return int(max.Int64), true, nil
}

// BackupIfPending copies the database into dir when Migrate would change its
// schema, and returns the copy's path ("" when no backup was needed).
//
// A migration rewrites tables in place, and the 0.x series promises breaking
// schema changes. A bad one used to be unrecoverable. This is the photograph
// taken of the room before the builders start: cheap, taken every time, and
// only looked at if something goes wrong. Restoring is copying the file back
// over kae.db with the older binary; there is deliberately no restore
// command to get wrong.
//
// The copy is made with VACUUM INTO, which writes a complete, consistent
// database file in one read transaction: it includes what is still in the
// write-ahead log, and it is safe while other kae processes have the
// database open. Only the newest BackupsKept backups are kept.
//
// Call it after Open and before Migrate. If the backup cannot be written the
// caller should not migrate: a migration without the backup that was
// supposed to precede it is the unprotected case this exists to remove.
func BackupIfPending(db *sql.DB, dir string, now time.Time) (string, error) {
	version, hasSchema, err := schemaVersion(db)
	if err != nil {
		return "", fmt.Errorf("read schema version: %w", err)
	}
	if !hasSchema || version >= LatestMigration {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	name := fmt.Sprintf("%s%d-%s%s", backupPrefix, version, now.UTC().Format(backupTime), backupSuffix)
	path := filepath.Join(dir, name)
	// VACUUM INTO refuses to overwrite. Two kae processes starting in the
	// same second (an agent's MCP server and the daemon, after an upgrade)
	// would pick the same name; the second finds the first's backup of the
	// same schema already there, which is all it wanted.
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if _, err := db.Exec(`VACUUM INTO ?`, path); err != nil {
		os.Remove(path) // a partial file must not be mistaken for a backup
		return "", fmt.Errorf("write backup %s: %w", path, err)
	}
	// The backup holds the same private transcripts as the database.
	_ = os.Chmod(path, 0600)
	if err := pruneBackups(dir); err != nil {
		return path, fmt.Errorf("remove old backups: %w", err)
	}
	return path, nil
}

// Backup describes one backup file.
type Backup struct {
	Path string
	// SchemaVersion is the migration the database was at when copied.
	SchemaVersion int
	// At is when the copy was made.
	At   time.Time
	Size int64
}

// ListBackups returns the backups in dir, newest first. A missing folder is
// an empty list, not an error.
func ListBackups(dir string) ([]Backup, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Backup
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, backupSuffix) {
			continue
		}
		// kae-s<version>-<time>.db; anything else in the folder is not ours
		// and is neither listed nor ever deleted.
		version, stamp, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(name, backupPrefix), backupSuffix), "-")
		if !ok {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(version, "%d", &v); err != nil {
			continue
		}
		at, err := time.Parse(backupTime, stamp)
		if err != nil {
			continue
		}
		b := Backup{Path: filepath.Join(dir, name), SchemaVersion: v, At: at}
		if info, err := e.Info(); err == nil {
			b.Size = info.Size()
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Path > out[j].Path
	})
	return out, nil
}

func pruneBackups(dir string) error {
	backups, err := ListBackups(dir)
	if err != nil {
		return err
	}
	for _, b := range backups[min(len(backups), BackupsKept):] {
		if err := os.Remove(b.Path); err != nil {
			return err
		}
	}
	return nil
}
