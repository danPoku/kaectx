// Copyright 2026 Dan Gyinaye Poku
// SPDX-License-Identifier: Apache-2.0

package mcpserver

// ContractVersion is the version of what kae's MCP tools accept and return:
// their names, their arguments and the fields of their results. Agent
// prompts and scripts are written against that shape, so it is versioned
// separately from the program, the way a socket standard is versioned
// separately from the appliances that plug into it.
//
// It is "major.minor":
//
//   - minor goes up when something is added: a tool, an optional argument,
//     a result field. Nothing written against the older shape breaks.
//   - major goes up when something is removed or renamed, or an argument
//     becomes required. That is only done after one release in which the
//     old form still works and is marked deprecated.
//
// While the major number is 0 the contract is not yet promised stable; 1.0
// of the contract ships with 1.0 of kae. CONTRIBUTING.md has the rules.
//
// The shape is recorded in testdata/contract.json. TestContract fails when
// the tools no longer match it, and refuses to record a new shape under the
// same version, so a change cannot ship unannounced. To change the contract:
// raise this constant, then run
//
//	go test -tags sqlite_fts5 ./internal/mcpserver -run TestContract -update
//
// and say what changed in the pull request.
const ContractVersion = "0.2"
