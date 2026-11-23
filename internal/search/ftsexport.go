// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

// SanitizeFTS5Query is the query rewrite every search applies before FTS5
// sees the text. It is exported for read-only measurement (scripts/slowsearch)
// that runs the search SQL stage by stage: a tool that timed a differently
// prepared query would be timing a different search.
func SanitizeFTS5Query(query string) string { return sanitizeFTS5Query(query) }
