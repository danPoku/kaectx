// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/danPoku/kaectx/internal/doctor"
	"github.com/danPoku/kaectx/internal/feedback"
	"github.com/danPoku/kaectx/internal/store"
)

func runDoctor(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: ctx doctor")
	}
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	projectID, ok, err := store.LookupProject(db, cwd)
	if err != nil {
		return fmt.Errorf("look up project for %s: %w", cwd, err)
	}
	if !ok {
		return fmt.Errorf("no ctx project found for %s; ingest a session from this directory first", cwd)
	}
	report, err := doctor.Collect(context.Background(), db, projectID)
	if err != nil {
		return err
	}
	fmt.Print(formatDoctor(report))
	return nil
}

func formatDoctor(r doctor.Report) string {
	lastIngest := "never"
	if r.LastIngestedAt.Valid {
		lastIngest = r.LastIngestedAt.String
	}
	return fmt.Sprintf(`ctx doctor: %s

ingestion
  sources (all projects): %d (%d with errors)
  last ingested (all projects): %s
  sessions/messages/chunks: %d / %d / %d

embeddings
  pending: %d
  unassigned chunks (all projects): %d

retrieval (last 7 days)
  runs: %d (%d zero-result, %d hybrid, %d risk-aware)
  opened: %d (%d exact, %d inferred; %d labelled, %d awaiting outcome)

outcomes (retained evidence)
  observations: %d
  coverage: %d / %d evidence keys (%.1f%%)
  credible / still neutral: %d / %d

search outcomes (last 7 days)
  %s
  reasons: %s
`, r.ProjectName, r.Sources, r.SourcesWithErrors, lastIngest,
		r.Sessions, r.Messages, r.Chunks, r.PendingEmbeddings, r.UnassignedChunks,
		r.RecentRetrievals, r.RecentZeroResultRuns, r.RecentHybridRuns, r.RecentPolicyRuns,
		r.RecentOpenedResults, r.RecentOpenedResults-r.RecentInferredOpens, r.RecentInferredOpens,
		r.RecentLabelledOpens, r.RecentOpenedResults-r.RecentLabelledOpens,
		r.Outcomes, r.ObservedEvidence, r.RetrievedEvidence, r.OutcomeCoverage(),
		r.CredibleEvidence, r.NeutralObservedEvidence(),
		formatCounts(feedback.SearchOutcomes, r.SearchOutcomes),
		formatCounts(feedback.SearchReasons, r.SearchReasons))
}

// formatCounts prints counts in the fixed order of keys, e.g.
// "helpful 3, partial 1, unhelpful 0", so the line reads the same every run.
func formatCounts(keys []string, counts map[string]int) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, counts[k])
	}
	return strings.Join(parts, ", ")
}
