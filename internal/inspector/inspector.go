// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package inspector serves a local web page for seeing what kae holds and
// correcting it: browse projects, sessions and notes, search, and (when
// edits are allowed) edit or delete a note, forget a session, or exclude a
// project from ingest.
//
// The catalogue holds redacted but still private transcripts, so the server
// is built like a strongroom with one door:
//
//   - It listens on the loopback address only. Nothing on the network can
//     reach it.
//   - Every URL starts with a random token made for this run and printed
//     once to the terminal. A request without it gets a plain 404. Other
//     programs and other users on the same machine can reach a loopback
//     port; the token is what they do not have.
//   - Requests whose Host header is not a loopback name are refused, which
//     stops a web page from reaching the server by pointing a DNS name it
//     controls at 127.0.0.1.
//   - It is read-only unless started with edits allowed, and every change
//     is a POST behind a confirmation page.
//
// Pages are rendered on the server with html/template, which escapes every
// value, so transcript text that happens to contain HTML is shown as text.
// There is no JavaScript.
package inspector

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Options configures a Server.
type Options struct {
	// AllowEdits enables the actions that change the catalogue. Without it
	// the pages show no buttons and every POST is refused.
	AllowEdits bool
}

// Server is the inspector. Create one with New.
type Server struct {
	db    *sql.DB
	opts  Options
	token string
}

// New makes a Server over db with a fresh random token.
func New(db *sql.DB, opts Options) (*Server, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	return &Server{db: db, opts: opts, token: hex.EncodeToString(buf)}, nil
}

// Token is the secret path segment every URL must start with.
func (s *Server) Token() string { return s.token }

// Listen opens the listening socket. The address is always loopback; only
// the port is the caller's choice (0 picks a free one). Binding is separated
// from serving so the caller can print the real URL before the first
// request.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
}

// URL is the address to open for a server listening on l.
func (s *Server) URL(l net.Listener) string {
	return "http://" + l.Addr().String() + "/" + s.token + "/"
}

// Serve answers requests on l until it is closed.
func (s *Server) Serve(l net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// loopbackHost reports whether a Host header names this machine's loopback
// interface, with or without a port.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// Handler is the whole inspector as an http.Handler: the security checks,
// then the pages.
func (s *Server) Handler() http.Handler {
	pages := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same answer for every refusal, so a prober learns nothing
		// about whether it was the host, the token or the path.
		if !loopbackHost(r.Host) {
			http.NotFound(w, r)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		given, path, _ := strings.Cut(rest, "/")
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) != 1 {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost:
			if !s.opts.AllowEdits {
				http.Error(w, "this inspector is read-only; start it with --allow-edits to make changes", http.StatusForbidden)
				return
			}
			// A form on another site could be made to POST here if it
			// learned the URL. A browser says where a POST came from; one
			// that names another origin is refused.
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		h := w.Header()
		// The pages hold private text: keep them out of caches and out of
		// frames, send no referrer (it would carry the token), and allow
		// nothing but the page's own inline style.
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")

		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + path
		pages.ServeHTTP(w, r2)
	})
}
