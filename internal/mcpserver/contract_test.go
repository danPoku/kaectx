// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danPoku/kaectx/internal/mcpserver"
	"github.com/danPoku/kaectx/internal/store"
)

var updateContract = flag.Bool("update", false, "record the MCP tools' current shape in testdata/contract.json")

const contractFile = "testdata/contract.json"

// toolShape is one tool as an agent sees it: its name and the JSON Schemas
// of what it takes and what it returns.
type toolShape struct {
	Name   string `json:"name"`
	Input  any    `json:"input"`
	Output any    `json:"output"`
}

// contract is the recorded shape of every tool at one contract version.
type contract struct {
	ContractVersion string      `json:"contract_version"`
	Tools           []toolShape `json:"tools"`
}

// withoutDescriptions drops every "description" from a schema. The contract
// is the shape of the data, not the wording that explains it: a clearer
// sentence for a field must not look like a breaking change. Tool names,
// field names, types and which fields are required all remain.
func withoutDescriptions(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "description" {
				continue
			}
			out[k] = withoutDescriptions(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = withoutDescriptions(val)
		}
		return out
	}
	return v
}

// normalise turns a schema into plain maps and slices, so two schemas that
// say the same thing compare equal however they were built or ordered.
func normalise(t *testing.T, schema any) any {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return withoutDescriptions(v)
}

// currentContract asks a running server for its tools, as a client would.
func currentContract(t *testing.T) contract {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	cs := serverAt(t, db, "/home/dev/proj")
	res, err := cs.ListTools(context.Background(), &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	c := contract{ContractVersion: mcpserver.ContractVersion}
	for _, tool := range res.Tools {
		c.Tools = append(c.Tools, toolShape{Name: tool.Name, Input: normalise(t, tool.InputSchema), Output: normalise(t, tool.OutputSchema)})
	}
	sort.Slice(c.Tools, func(i, j int) bool { return c.Tools[i].Name < c.Tools[j].Name })
	return c
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestContract fails when a tool's name, arguments or result fields differ
// from the recorded contract. Changing them is allowed; doing it without
// raising ContractVersion and recording the new shape is not.
func TestContract(t *testing.T) {
	now := currentContract(t)
	if len(now.Tools) == 0 {
		t.Fatal("the server listed no tools")
	}
	for _, tool := range now.Tools {
		if tool.Output == nil {
			t.Errorf("tool %s has no output schema, so its result shape is not under contract", tool.Name)
		}
	}

	recorded, err := os.ReadFile(contractFile)
	if err != nil && !*updateContract {
		t.Fatalf("read %s: %v (run with -update to create it)", contractFile, err)
	}
	var old contract
	if len(recorded) > 0 {
		if err := json.Unmarshal(recorded, &old); err != nil {
			t.Fatalf("parse %s: %v", contractFile, err)
		}
	}
	sameShape := bytes.Equal(encode(t, old.Tools), encode(t, now.Tools))
	sameVersion := old.ContractVersion == now.ContractVersion

	if *updateContract {
		// The one thing -update will not do is hide a change: a new shape
		// under the old version number is exactly the unannounced change
		// this test exists to stop.
		if len(recorded) > 0 && !sameShape && sameVersion {
			t.Fatalf("the tools changed but ContractVersion is still %s. Raise it in contract.go first (minor for an addition, major for a removal or rename), then run -update again.\n%s",
				now.ContractVersion, describeChange(old, now))
		}
		if err := os.MkdirAll(filepath.Dir(contractFile), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(contractFile, encode(t, now), 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded contract %s with %d tools in %s", now.ContractVersion, len(now.Tools), contractFile)
		return
	}

	if !sameShape {
		t.Fatalf("the MCP tools no longer match the recorded contract %s.\n%s\nIf the change is intended: raise ContractVersion in contract.go (minor for an addition, major for a removal or rename), run\n  go test -tags sqlite_fts5 ./internal/mcpserver -run TestContract -update\nand describe the change in the pull request. See CONTRIBUTING.md, \"The MCP contract\".",
			old.ContractVersion, describeChange(old, now))
	}
	if !sameVersion {
		t.Fatalf("ContractVersion is %s but %s records %s for the same tools. Run the test with -update, or put the constant back.",
			now.ContractVersion, contractFile, old.ContractVersion)
	}
}

// describeChange names the tools that were added, removed or changed, so a
// failure says where to look instead of only that something differs.
func describeChange(old, now contract) string {
	before := map[string]toolShape{}
	for _, tool := range old.Tools {
		before[tool.Name] = tool
	}
	var lines []string
	for _, tool := range now.Tools {
		prev, existed := before[tool.Name]
		delete(before, tool.Name)
		switch {
		case !existed:
			lines = append(lines, "  added:   "+tool.Name)
		case !sameJSON(prev.Input, tool.Input) && !sameJSON(prev.Output, tool.Output):
			lines = append(lines, "  changed: "+tool.Name+" (arguments and result)")
		case !sameJSON(prev.Input, tool.Input):
			lines = append(lines, "  changed: "+tool.Name+" (arguments)")
		case !sameJSON(prev.Output, tool.Output):
			lines = append(lines, "  changed: "+tool.Name+" (result)")
		}
	}
	for name := range before {
		lines = append(lines, "  removed: "+name)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// The version an agent is told must be the one the code declares.
func TestContractVersionIsAnnouncedToAgents(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "kae.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	cs := serverAt(t, db, "/home/dev/proj")
	instructions := cs.InitializeResult().Instructions
	if want := "Tool contract version: " + mcpserver.ContractVersion + "."; !strings.Contains(instructions, want) {
		t.Fatalf("server instructions do not carry %q:\n%s", want, instructions)
	}
}

// The checks describeChange and the description filter make are the ones the
// contract depends on, so they are tested directly.
func TestContractComparison(t *testing.T) {
	schema := func(fields ...string) any {
		props := map[string]any{}
		for _, f := range fields {
			props[f] = map[string]any{"type": "string", "description": "words about " + f}
		}
		return map[string]any{"type": "object", "properties": props}
	}
	old := contract{ContractVersion: "0.1", Tools: []toolShape{
		{Name: "get_chunk", Input: schema("chunk_id"), Output: schema("text")},
		{Name: "save_note", Input: schema("title"), Output: schema("id")},
		{Name: "old_tool", Input: schema(), Output: schema()},
	}}
	now := contract{ContractVersion: "0.1", Tools: []toolShape{
		{Name: "get_chunk", Input: schema("chunk_id"), Output: schema("text", "agent")},
		{Name: "save_note", Input: schema("title"), Output: schema("id")},
		{Name: "get_brief", Input: schema(), Output: schema("text")},
	}}
	want := "  added:   get_brief\n  changed: get_chunk (result)\n  removed: old_tool"
	if got := describeChange(old, now); got != want {
		t.Errorf("describeChange:\n%s\nwant:\n%s", got, want)
	}

	// Rewording a description is not a contract change; renaming a field is.
	reworded := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "description": "clearer words"}}}
	if !sameJSON(withoutDescriptions(schema("text")), withoutDescriptions(reworded)) {
		t.Error("a reworded description was treated as a change of shape")
	}
	if sameJSON(withoutDescriptions(schema("text")), withoutDescriptions(schema("body"))) {
		t.Error("a renamed field was not treated as a change of shape")
	}
}
