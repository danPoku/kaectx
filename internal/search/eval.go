// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"database/sql"
)

// Rerank applies the opt-in risk-aware policy to results in place, exactly as
// a risk-aware search does, and records nothing. It exists for read-only
// evaluation (scripts/evalset): measuring risk-aware search through
// BestTracked would write a telemetry row for every test query, and a
// yardstick that changes what it measures is no yardstick. On failure the
// results are left as they were.
func Rerank(ctx context.Context, db *sql.DB, projectID int64, results []Result) error {
	return applyGovernance(ctx, db, projectID, results)
}
