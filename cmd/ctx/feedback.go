// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/danPoku/kaectx/internal/feedback"
	"github.com/danPoku/kaectx/internal/governance"
	"github.com/danPoku/kaectx/internal/store"
)

const feedbackUsage = "usage: ctx feedback <run_id> <rank> <outcome> [--note TEXT]\n  outcome: " +
	"helpful | unhelpful | stale | incorrect | superseded"

// runFeedback labels one result of an earlier `ctx search`, the CLI's
// counterpart to the record_retrieval_outcome MCP tool. Both write through
// feedback.Record.
func runFeedback(args []string) error {
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	projectID, err := store.ResolveProject(db, cwd)
	if err != nil {
		return fmt.Errorf("resolve project for %s: %w", cwd, err)
	}
	return feedbackIn(db, projectID, args, os.Stdout)
}

// feedbackIn is runFeedback against a given database and project, so it can
// be tested without a home directory or working tree.
func feedbackIn(db *sql.DB, projectID int64, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("feedback", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	note := fs.String("note", "", "optional explanation; secrets are redacted before storage")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"note": true})); err != nil {
		return fmt.Errorf("%v\n%s", err, feedbackUsage)
	}
	if fs.NArg() != 3 {
		return fmt.Errorf("%s", feedbackUsage)
	}
	runID, rankArg, outcome := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	rank, err := strconv.Atoi(rankArg)
	if err != nil || rank <= 0 {
		return fmt.Errorf("rank must be a positive number, not %q", rankArg)
	}
	got, err := feedback.Record(db, projectID, runID, rank, outcome, *note)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "recorded %s for %s #%d\n", outcome, runID, rank)
	if got.AssessErr != nil {
		// Stored regardless; say so rather than fail and invite a retry.
		fmt.Fprintf(os.Stderr, "ctx feedback: warning: outcome recorded, but re-assessing the evidence failed: %v\n", got.AssessErr)
		return nil
	}
	a := got.Assessment
	if a.Neutral {
		fmt.Fprintf(w, "evidence: %d outcome(s), still neutral until %d\n", a.Observations, governance.MinCredibleObservations)
	} else {
		fmt.Fprintf(w, "evidence: %d outcome(s), reliability %.3f\n", a.Observations, a.Reliability)
	}
	return nil
}

// labelHint heads a recorded search's output: its run ID and how to label a
// hit with it.
func labelHint(runID string) string {
	return fmt.Sprintf("run %s  (label a hit: ctx feedback %s <rank> <outcome>)", runID, runID)
}
