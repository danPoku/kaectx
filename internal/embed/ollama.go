// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

// Package embed generates and stores chunk embeddings for semantic search.
// Embeddings are produced locally via Ollama — code never leaves the
// machine, matching CLAUDE.md's non-negotiable on where model calls happen.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Dims must match migrations/002_vectors.sql's chunk_vectors.embedding
// column (FLOAT[768]) — nomic-embed-text's native output size. Changing
// embedding models means a new column width and a full re-embed, per that
// migration's own comment; this constant is the one place that width is
// asserted in Go.
const Dims = 768

// DefaultBaseURL and DefaultModel are what every kae subcommand uses unless
// told otherwise — Ollama's standard local port, and the model
// migrations/002_vectors.sql's FLOAT[768] column was sized for.
const (
	DefaultBaseURL = "http://localhost:11434"
	DefaultModel   = "nomic-embed-text"
)

// Embedder produces a vector for one chunk of text. An interface so the
// embed worker can be tested against a fake without a live Ollama, and so a
// different model/provider could be swapped in later without touching
// worker.go.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Model identifies which model produced the vector, stored in
	// chunks.embedding_model so a future model switch can find and re-embed
	// everything embedded under the old one.
	Model() string
}

// OllamaClient calls a local Ollama server's /api/embed endpoint (the
// current, batch-capable embeddings API — /api/embeddings is Ollama's
// older, deprecated single-text endpoint).
type OllamaClient struct {
	baseURL string
	model   string
	http    *http.Client
}

// NewOllamaClient builds a client against baseURL (e.g.
// "http://localhost:11434") using model (e.g. "nomic-embed-text"). It
// doesn't contact the server — a bad baseURL or a missing model only
// surfaces on the first Embed call.
func NewOllamaClient(baseURL, model string) *OllamaClient {
	return &OllamaClient{baseURL: baseURL, model: model, http: &http.Client{}}
}

func (c *OllamaClient) Model() string { return c.model }

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// Embed calls Ollama for a single text's embedding. It sends `input` as a
// single string rather than batching multiple chunks into one request —
// simpler error attribution (which chunk failed) at the cost of one HTTP
// round trip per chunk, an acceptable trade for a background worker that
// isn't latency-sensitive.
func (c *OllamaClient) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: text})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w (is `ollama serve` running at %s?)", err, c.baseURL)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read response: %w", err)
	}

	var er embedResponse
	if err := json.Unmarshal(respBody, &er); err != nil {
		return nil, fmt.Errorf("ollama: decode response (status %d): %w\nbody: %s", resp.StatusCode, err, respBody)
	}
	if resp.StatusCode != http.StatusOK {
		msg := er.Error
		if msg == "" {
			msg = string(respBody)
		}
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, msg)
	}
	if len(er.Embeddings) != 1 {
		return nil, fmt.Errorf("ollama: expected 1 embedding, got %d", len(er.Embeddings))
	}
	vec := er.Embeddings[0]
	if len(vec) != Dims {
		return nil, fmt.Errorf("ollama: model %q returned a %d-dim vector, want %d (does chunk_vectors need a new column width?)", c.model, len(vec), Dims)
	}
	return vec, nil
}
