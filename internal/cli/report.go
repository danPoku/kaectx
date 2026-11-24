// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/danPoku/kaectx/internal/report"
	"github.com/danPoku/kaectx/internal/store"
)

// runReport handles `kae report`: what kae did over a period, in counts, for
// the current project or for all of them.
func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	sinceFlag := fs.String("since", "7d", "how far back: a period (7d, 36h, 2w), a date (2026-11-17) or an RFC 3339 time")
	allProjects := fs.Bool("all-projects", false, "report on every project, not only the one containing the current directory")
	asJSON := fs.Bool("json", false, "print the report as JSON, for scripts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: kae report [--since D] [--all-projects] [--json]")
	}
	now := time.Now().UTC()
	since, err := report.ParseSince(*sinceFlag, now)
	if err != nil {
		return err
	}

	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	var projectID int64
	if !*allProjects {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve working directory: %w", err)
		}
		id, found, err := store.LookupProject(db, cwd)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no kae project found for %s; ingest a session from this directory first, or use --all-projects", cwd)
		}
		projectID = id
	}

	r, err := report.Build(db, projectID, since, now)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Print(report.Format(r))
	return nil
}
