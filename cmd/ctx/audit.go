// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/danPoku/kaectx/internal/audit"
)

// errAuditFindings makes `ctx audit-secrets` exit non-zero when it found
// something, so it can gate a script or CI job. main prints the message.
var errAuditFindings = errors.New("residual secrets found in stored text")

// runAuditSecrets handles `ctx audit-secrets`: re-scan everything already in
// the database with today's redaction rules and report what would still be
// redacted. It only reports row locations and rule names, never the matched
// text, and it changes nothing.
func runAuditSecrets(args []string) error {
	fs := flag.NewFlagSet("audit-secrets", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	rep, err := audit.Secrets(db)
	if err != nil {
		return err
	}

	fmt.Printf("%-10s %-8s %10s %8s  %s\n", "TABLE", "COLUMN", "SCANNED", "HITS", "RULES")
	for _, l := range rep.Locations {
		fmt.Printf("%-10s %-8s %10d %8d  %v\n", l.Table, l.Column, l.Scanned, l.Hits, l.RuleNames())
	}
	if rep.Hits() == 0 {
		fmt.Println("\nclean: nothing stored would be changed by the current redaction rules")
		return nil
	}

	fmt.Println("\nexamples (row locations only, matched text is never printed):")
	for _, l := range rep.Locations {
		for _, ex := range l.Examples {
			seq := ""
			if ex.Seq != nil {
				seq = fmt.Sprintf(" seq=%d", *ex.Seq)
			}
			fmt.Printf("  %s.%s id=%d session=%s%s rules=%v\n", l.Table, l.Column, ex.ID, ex.SessionID, seq, ex.Rules)
		}
	}
	return errAuditFindings
}
