// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/danPoku/kaectx/internal/brief"
	"github.com/danPoku/kaectx/internal/store"
)

// runBrief handles `kae brief`: print the short summary of the current
// project's past work that an agent gets at session start.
//
// With --hook it is being run by an agent's session-start hook, and two
// things change. The delivery is recorded, so a handoff given this way
// counts as picked up. And nothing may go wrong loudly: a hook that fails
// or prints an error would greet every new session with it, so any problem
// ends in silence and a zero exit, and the agent simply starts without a
// brief.
func runBrief(args []string) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	budget := fs.Int("budget", brief.DefaultBudget, "largest size of the brief, in tokens")
	hook := fs.Bool("hook", false, "run as an agent's session-start hook: record the delivery, and never fail or print an error")
	if *hook {
		fs.SetOutput(io.Discard)
	}
	if err := fs.Parse(args); err != nil {
		if hookRequested(args) {
			return nil
		}
		return err
	}
	err := printBrief(os.Stdout, *budget, *hook)
	if *hook {
		return nil
	}
	return err
}

// hookRequested reports whether --hook was among args, for the case where
// flag parsing itself failed and the parsed value cannot be trusted.
func hookRequested(args []string) bool {
	for _, a := range args {
		if a == "--hook" || a == "-hook" {
			return true
		}
	}
	return false
}

func printBrief(w io.Writer, budget int, record bool) error {
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	rendered, err := briefFor(db, cwd, budget, record)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, rendered.Text)
	return err
}

// briefFor renders the brief for the project that contains cwd. A folder
// kae has never seen gets an empty brief; looking must not create a project
// row for every directory an agent is started in.
func briefFor(db *sql.DB, cwd string, budget int, record bool) (brief.Rendered, error) {
	projectID, found, err := store.LookupProject(db, cwd)
	if err != nil || !found {
		return brief.Rendered{Budget: budget}, err
	}
	b, err := brief.Build(db, projectID, time.Now())
	if err != nil {
		return brief.Rendered{}, err
	}
	rendered := b.Render(budget)
	if record {
		if err := brief.RecordDelivery(db, rendered.HandoffNoteID); err != nil {
			return rendered, err
		}
	}
	return rendered, nil
}
