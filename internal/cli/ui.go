// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/danPoku/kaectx/internal/inspector"
)

// runUI handles `kae ui`: serve the local inspector until interrupted.
//
// It prints one URL, which contains the access token for this run. The
// address cannot be chosen: the server listens on 127.0.0.1 only, and there
// is deliberately no flag to change that. Reaching it from another machine
// is what an SSH tunnel is for.
func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	port := fs.Int("port", 0, "port to listen on, on 127.0.0.1 (default: any free port)")
	allowEdits := fs.Bool("allow-edits", false, "allow editing and deleting notes, forgetting sessions and excluding projects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, _, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	srv, err := inspector.New(db, inspector.Options{AllowEdits: *allowEdits})
	if err != nil {
		return err
	}
	listener, err := inspector.Listen(*port)
	if err != nil {
		return err
	}
	mode := "read-only"
	if *allowEdits {
		mode = "edits allowed"
	}
	fmt.Printf("kae ui (%s): open %s\n", mode, srv.URL(listener))
	fmt.Println("The link holds this run's access token; anyone who has it can read your sessions. Press Ctrl+C to stop.")

	// Closing the listener on Ctrl+C makes Serve return, so the deferred
	// db.Close runs instead of the process being killed mid-request.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		listener.Close()
	}()
	return srv.Serve(listener)
}
