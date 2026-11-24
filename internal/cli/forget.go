// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/danPoku/kaectx/internal/store"
)

// runForget handles `kae forget <session>`: remove a session and everything
// stored from it, for good.
//
// It cannot be undone from inside kae (the agent's own log file is left
// alone, but kae will not read that session from it again), so it asks
// first unless --yes is given.
func runForget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"yes": false})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kae forget <session-id> [--yes]")
	}
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	return forgetSession(os.Stdout, os.Stdin, db, fs.Arg(0), *yes)
}

func forgetSession(w io.Writer, in io.Reader, db *sql.DB, typed string, yes bool) error {
	id, err := store.FindSession(db, typed)
	if err != nil {
		return err
	}
	var title sql.NullString
	var started string
	var messages int
	if err := db.QueryRow(`SELECT title, started_at, message_count FROM sessions WHERE id = ?`, id).Scan(&title, &started, &messages); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s  %s  %d messages\n  %s\n", id, started, messages, title.String)
	if !yes && !confirmOn(w, in, "Forget this session? Its messages, search entries and file records are deleted, and kae will not ingest it again.") {
		fmt.Fprintln(w, "nothing was changed")
		return nil
	}
	got, err := store.ForgetSession(db, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "forgot %s: removed %d messages, %d search entries and %d file records. It will not be ingested again.\n",
		got.SessionID, got.Messages, got.Chunks, got.Touches)
	return nil
}

// confirmOn asks a yes/no question on w and reads the answer from in. No
// answer (a closed stdin, as in a script) is no: nothing is deleted on
// silence.
func confirmOn(w io.Writer, in io.Reader, question string) bool {
	fmt.Fprintf(w, "%s [y/N] ", question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" {
		fmt.Fprintln(w)
	}
	return answer == "y" || answer == "yes"
}

// projectExclude handles `kae project exclude|include <id>`.
func projectExclude(w io.Writer, in io.Reader, db *sql.DB, args []string, exclude bool) error {
	verb := "include"
	if exclude {
		verb = "exclude"
	}
	fs := flag.NewFlagSet("project "+verb, flag.ContinueOnError)
	forget := fs.Bool("forget", false, "with exclude: also delete the sessions already stored for the project")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"forget": false, "yes": false})); err != nil {
		return err
	}
	usage := fmt.Errorf("usage: kae project %s <project-id>%s", verb, map[bool]string{true: " [--forget] [--yes]", false: ""}[exclude])
	if fs.NArg() != 1 || (*forget && !exclude) {
		return usage
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("project ids are integers (see `kae project list`)")
	}
	var name string
	var sessions int
	if err := db.QueryRow(`SELECT name, (SELECT count(*) FROM sessions s WHERE s.project_id = p.id) FROM projects p WHERE id = ?`, id).
		Scan(&name, &sessions); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("project %d not found (see `kae project list`)", id)
		}
		return err
	}

	if *forget && sessions > 0 && !*yes &&
		!confirmOn(w, in, fmt.Sprintf("Exclude %q and delete the %d session(s) already stored for it?", name, sessions)) {
		fmt.Fprintln(w, "nothing was changed")
		return nil
	}
	changed, err := store.SetProjectExcluded(db, id, exclude)
	if err != nil {
		return err
	}
	switch {
	case exclude && changed:
		fmt.Fprintf(w, "excluded %q (project %d): nothing more will be ingested for it\n", name, id)
	case exclude:
		fmt.Fprintf(w, "%q (project %d) was already excluded\n", name, id)
	case changed:
		fmt.Fprintf(w, "included %q (project %d): run `kae ingest` to read what was skipped while it was excluded\n", name, id)
	default:
		fmt.Fprintf(w, "%q (project %d) was not excluded\n", name, id)
	}
	if !*forget {
		if exclude && sessions > 0 {
			fmt.Fprintf(w, "The %d session(s) already stored are kept; add --forget to delete them.\n", sessions)
		}
		return nil
	}
	n, err := store.ForgetProjectSessions(db, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "forgot %d session(s) of %q\n", n, name)
	return nil
}
