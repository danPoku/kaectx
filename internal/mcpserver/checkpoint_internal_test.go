// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"testing"
	"time"
)

func TestActiveDuration(t *testing.T) {
	at := func(minutes int) time.Time {
		return time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
	}
	const limit = 30 * time.Minute
	cases := []struct {
		name  string
		times []time.Time
		want  time.Duration
	}{
		{"no messages", nil, 0},
		{"one message has no gap to measure", []time.Time{at(0)}, 0},
		{"steady work", []time.Time{at(0), at(10), at(25)}, 25 * time.Minute},
		{"a gap at the limit counts", []time.Time{at(0), at(30)}, 30 * time.Minute},
		{"a gap over the limit is a break", []time.Time{at(0), at(31)}, 0},
		{"work either side of a break", []time.Time{at(0), at(5), at(24 * 60), at(24*60 + 7)}, 12 * time.Minute},
		{"a clock that runs backwards adds nothing", []time.Time{at(10), at(0), at(5)}, 5 * time.Minute},
		{"identical timestamps add nothing", []time.Time{at(0), at(0), at(0)}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := activeDuration(c.times, limit); got != c.want {
				t.Errorf("activeDuration = %s, want %s", got, c.want)
			}
		})
	}
}
