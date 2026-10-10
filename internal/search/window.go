// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Window limits a search to turns that happened in a time range. The zero
// value means all time.
//
// Relevance alone answers "what is the best match ever", but questions like
// "what did we work on last week" need "the best match from last week".
// Without a window, a strong match from March outranks a weaker one from
// Tuesday every time, and the agent writing a weekly report wades through
// months of history to find the one week it asked about.
type Window struct {
	Since string // inclusive, 'YYYY-MM-DDTHH:MM:SS.000Z', or "" for no lower bound
	Until string // exclusive, same format, or "" for no upper bound
}

// windowLayout is how bounds are handed to SQLite, which compares them
// with julianday() (see queries/retrieval.sql). Agents log at millisecond
// precision, so that is the precision kept; see ceilMillis for rounding.
const windowLayout = "2006-01-02T15:04:05.000Z"

// ceilMillis rounds t up to the next whole millisecond. Rounding a bound
// down would let in a turn logged just before it (since=...00.0005 must
// not admit a turn at ...00.000); rounding up never admits an earlier
// turn, and for the exclusive until it only admits turns that really were
// before it, since logged times have no finer precision.
func ceilMillis(t time.Time) time.Time {
	if c := t.Truncate(time.Millisecond); !c.Equal(t) {
		return c.Add(time.Millisecond)
	}
	return t
}

// ParseWindow builds a Window from user input. Each bound may be a date
// (2026-09-28) or an RFC 3339 timestamp (2026-09-28T09:00:00Z, any offset);
// either may be empty. A date as until means through the end of that day,
// so since=2026-09-28 until=2026-10-04 covers both whole days, the way a
// person reads "September 28 to October 4". Dates are taken as UTC days.
func ParseWindow(since, until string) (Window, error) {
	var w Window
	if since != "" {
		t, _, err := parseBound(since)
		if err != nil {
			return w, fmt.Errorf("since: %w", err)
		}
		w.Since = ceilMillis(t).UTC().Format(windowLayout)
	}
	if until != "" {
		t, dateOnly, err := parseBound(until)
		if err != nil {
			return w, fmt.Errorf("until: %w", err)
		}
		if dateOnly {
			t = t.AddDate(0, 0, 1)
		}
		w.Until = ceilMillis(t).UTC().Format(windowLayout)
	}
	if w.Since != "" && w.Until != "" && w.Since >= w.Until {
		return w, fmt.Errorf("since (%s) must be before until (%s)", since, until)
	}
	return w, nil
}

func parseBound(s string) (t time.Time, dateOnly bool, err error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("%q is not a date (2026-09-28) or RFC 3339 timestamp (2026-09-28T09:00:00Z)", s)
}

// IsZero reports whether w places no limit at all.
func (w Window) IsZero() bool { return w.Since == "" && w.Until == "" }

// Args binds w as the :since/:until parameters every windowed query in
// queries/retrieval.sql takes. An empty bound binds NULL, which the SQL
// reads as "no limit".
func (w Window) Args() []any {
	return []any{sql.Named("since", nullIfEmpty(w.Since)), sql.Named("until", nullIfEmpty(w.Until))}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
