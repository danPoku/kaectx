// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"

	"github.com/danPoku/kaectx/internal/repair"
	"github.com/danPoku/kaectx/internal/store"
)

const repairUsage = "usage: ctx repair git|touches|messages [--dry-run]"

func runRepair(args []string) error {
	if len(args) == 0 || (args[0] != "git" && args[0] != "touches" && args[0] != "messages") {
		return fmt.Errorf(repairUsage)
	}
	kind := args[0]
	fs := flag.NewFlagSet("repair "+kind, flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf(repairUsage)
	}

	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	if kind == "messages" {
		res, err := repair.Messages(db, *dryRun)
		if err != nil {
			return err
		}
		fmt.Print(formatMessages(res, *dryRun))
		return nil
	}

	git := store.NewGitResolver()
	if kind == "touches" {
		res, err := repair.Touches(db, git, *dryRun)
		if err != nil {
			return err
		}
		fmt.Print(formatTouches(res, *dryRun))
		return nil
	}
	res, err := repair.Git(db, git, *dryRun)
	if err != nil {
		return err
	}
	fmt.Print(formatRepair(res, *dryRun))
	return nil
}

func formatMessages(r repair.MessagesResult, dryRun bool) string {
	verb := "changed"
	if dryRun {
		verb = "would change"
	}
	out := fmt.Sprintf("re-read %d user/system messages from their raw log lines\n", r.Examined)
	out += fmt.Sprintf("messages: %s %d (%d reclassified as user turn or injected event, %d with recovered prompt text)\n",
		verb, r.Reclassed+r.Recovered, r.Reclassed, r.Recovered)
	if r.Expanded > 0 {
		out += fmt.Sprintf("          plus %d injected events %s to keep all their blocks (no effect on search)\n", r.Expanded, verb)
	}
	if r.Unparseable > 0 {
		out += fmt.Sprintf("          %d had nothing to re-read (empty prompts, or a raw line the adapter rejects); left as they were\n", r.Unparseable)
	}
	out += fmt.Sprintf("chunks:   rebuilt for %d sessions; new chunks are embedded by `ctx daemon` or `ctx embed`\n", r.Sessions)
	out += fmt.Sprintf("titles:   %s %d\n", verb, r.Titled)
	if dryRun {
		out += "dry run: nothing was written\n"
	}
	return out
}

func formatTouches(r repair.TouchesResult, dryRun bool) string {
	verb := "added"
	if dryRun {
		verb = "would add"
	}
	out := fmt.Sprintf("found %d apply_patch calls with no file records\n", r.Messages)
	out += fmt.Sprintf("touches:  %s %d file records", verb, r.Added)
	if r.Empty > 0 {
		out += fmt.Sprintf("; %d calls named no files", r.Empty)
	}
	out += "\n"
	if r.Pending > 0 {
		out += fmt.Sprintf("          %d records are in directories that no longer exist; `ctx repair git` will settle their paths if the checkout returns\n", r.Pending)
	}
	if dryRun {
		out += "dry run: nothing was written\n"
	}
	return out
}

func formatRepair(r repair.Result, dryRun bool) string {
	verb := "updated"
	if dryRun {
		verb = "would update"
	}
	out := fmt.Sprintf("examined %d sessions with a working directory\n", r.Sessions)
	out += fmt.Sprintf("commits:  %s %d sessions; %d could not be resolved and were left as they were\n",
		verb, r.CommitsUpdated, r.CommitsUnresolved)
	if r.CommitsUnverified > 0 {
		out += fmt.Sprintf("          %d of those still carry a commit from an older version and are now marked unverified\n", r.CommitsUnverified)
	}
	out += fmt.Sprintf("paths:    %s %d file paths to be repo-root-relative (%d rows now anchored)\n",
		verb, r.TouchesRewritten, r.TouchesRooted)
	if r.TouchesPending > 0 {
		out += fmt.Sprintf("          %d rows skipped because their directory no longer exists; they are retried on the next run\n", r.TouchesPending)
	}
	if dryRun {
		out += "dry run: nothing was written\n"
	}
	return out
}
