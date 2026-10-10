// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestOllamaClientLive is the one test in this package that talks to a real
// Ollama instance instead of a mock — a permanent regression check that the
// assumed wire format (request shape, "embeddings" field, dimension count)
// stays correct against the actual API, not just what ollama_test.go's
// mocks were told to return. It skips rather than fails when Ollama isn't
// reachable at the default URL (CI, a machine without it installed) — the
// mocked tests already cover the client's logic either way.
func TestOllamaClientLive(t *testing.T) {
	if os.Getenv("CTX_LIVE_OLLAMA") != "1" {
		t.Skip("set CTX_LIVE_OLLAMA=1 to run the live Ollama integration test")
	}
	probe, err := http.Get(DefaultBaseURL + "/api/tags")
	if err != nil || probe.StatusCode != http.StatusOK {
		t.Skipf("no live Ollama at %s, skipping: %v", DefaultBaseURL, err)
	}
	probe.Body.Close()

	c := NewOllamaClient(DefaultBaseURL, DefaultModel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vec, err := c.Embed(ctx, "how do I open a sqlite database in WAL mode")
	if err != nil {
		t.Fatalf("Embed against live Ollama: %v (is %q pulled? `ollama pull %s`)", err, DefaultModel, DefaultModel)
	}
	if len(vec) != Dims {
		t.Fatalf("len(vec) = %d, want %d", len(vec), Dims)
	}

	var nonZero int
	for _, f := range vec {
		if f != 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Error("embedding is all zeros — looks like a stub/error response, not a real embedding")
	}
}
