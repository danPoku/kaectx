// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"strconv"

	"github.com/danPoku/kaectx/internal/store"
)

// runProject handles `kae project ...`: `list`, `merge`, and `exclude` /
// `include`, which turn ingest off and on for a project. The list exists so
// the ids the others need are discoverable.
func runProject(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kae project list | merge <from-id> <into-id> | exclude <id> [--forget] | include <id>")
	}

	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "list":
		rows, err := db.Query(`SELECT p.id, p.name || CASE WHEN p.excluded_at IS NOT NULL THEN ' (excluded)' ELSE '' END,
				COALESCE(p.git_remote, '-'),
				(SELECT count(*) FROM sessions s WHERE s.project_id = p.id),
				COALESCE((SELECT group_concat(path, ', ') FROM project_paths pp WHERE pp.project_id = p.id), '')
			FROM projects p ORDER BY p.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Printf("%-4s %-20s %-32s %-8s %s\n", "ID", "NAME", "REMOTE", "SESSIONS", "PATHS")
		for rows.Next() {
			var id, sessions int64
			var name, remote, paths string
			if err := rows.Scan(&id, &name, &remote, &sessions, &paths); err != nil {
				return err
			}
			fmt.Printf("%-4d %-20s %-32s %-8d %s\n", id, name, remote, sessions, paths)
		}
		return rows.Err()

	case "exclude":
		return projectExclude(os.Stdout, os.Stdin, db, args[1:], true)

	case "include":
		return projectExclude(os.Stdout, os.Stdin, db, args[1:], false)

	case "merge":
		if len(args) != 3 {
			return fmt.Errorf("usage: kae project merge <from-id> <into-id>")
		}
		from, err1 := strconv.ParseInt(args[1], 10, 64)
		into, err2 := strconv.ParseInt(args[2], 10, 64)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("project ids must be integers (see `kae project list`)")
		}
		if err := store.MergeProjects(db, from, into); err != nil {
			return err
		}
		fmt.Printf("merged project %d into %d; run `kae embed` to re-embed moved chunks\n", from, into)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (want list, merge, exclude or include)", args[0])
}
