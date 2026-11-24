// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/danPoku/kaectx/internal/explain"
	"github.com/danPoku/kaectx/internal/store"
)

var explainFlagsWithValue = map[string]bool{"limit": true}

func runExplain(args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	limit := fs.Int("limit", explain.DefaultLimit, "max sessions")
	if err := fs.Parse(reorderFlagsFirst(args, explainFlagsWithValue)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kae explain <project-relative-path> [--limit N]")
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
	projectID, err := store.ResolveProject(db, cwd)
	if err != nil {
		return fmt.Errorf("resolve project for %s: %w", cwd, err)
	}

	out, err := explain.File(db, projectID, explain.ResolvePath(cwd, fs.Arg(0)), *limit)
	if err != nil {
		return err
	}
	printExplain(out)
	return nil
}

func printExplain(out explain.Result) {
	if len(out.Sessions) == 0 {
		fmt.Printf("No sessions found touching %s\n", out.Path)
		return
	}
	fmt.Printf("%s\n\n", out.Path)
	for _, s := range out.Sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("- [%s] %s  %s\n", s.Agent, s.StartedAt, title)
		fmt.Printf("  session: %s\n", s.SessionID)
		if len(s.Actions) > 0 {
			fmt.Printf("  actions: %s (%d touch", strings.Join(s.Actions, ","), s.Touches)
			if s.Touches != 1 {
				fmt.Print("es")
			}
			fmt.Println(")")
		}
		if s.GitBranch != "" || s.StartingCommit != "" || s.EndingCommit != "" {
			fmt.Println(gitLine(s))
		}
		if s.ChunkID != nil {
			fmt.Printf("  chunk: %d\n", *s.ChunkID)
		}
		if s.Preview != "" {
			fmt.Printf("  %s\n", s.Preview)
		}
		fmt.Println()
	}
}

func commitRange(start, end string) string {
	if start == "" && end == "" {
		return ""
	}
	if start == end || end == "" {
		return shortCommit(start)
	}
	if start == "" {
		return shortCommit(end)
	}
	return shortCommit(start) + ".." + shortCommit(end)
}

func shortCommit(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// gitLine renders a session's branch and commit span, flagging spans that are
// only a best guess so nobody mistakes them for a recorded fact.
func gitLine(s explain.Session) string {
	line := "  git: " + strings.TrimSpace(s.GitBranch+" "+commitRange(s.StartingCommit, s.EndingCommit))
	switch s.CommitSource {
	case store.SourceHead:
		line += " (approximate: branch unknown, read from current HEAD history)"
	case store.SourceUnverified:
		line += " (unverified: repository no longer available)"
	}
	return line
}
