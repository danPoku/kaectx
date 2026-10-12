<!--
Title: a Conventional Commit subject, e.g. "fix: redact Codex session titles".
Branch: <prefix>/<issue>-<slug>, targeting master. See CONTRIBUTING.md.
-->

## What and why

<!-- If the reader can only read one paragraph, this is the one. -->

Closes #

## How it was tested

- [ ] `go vet -tags sqlite_fts5 ./...` and `go test -count=1 -tags sqlite_fts5 ./...` pass locally
- [ ] Checked by hand against a real catalogue (say what you ran):

## Compatibility

<!-- Delete the lines that don't apply. Any of these makes the next release a minor bump. -->

- Schema: new migration `0NN_...`. Upgrade from the last release tested: yes / no. Needs `ctx repair ...` afterwards: yes / no.
- MCP: tools or output fields added, renamed or removed:
- CLI: commands or flags added, renamed or removed:
- Adapter: new fixture in `testdata/` from agent version:

## Docs

- [ ] README updated if a command, flag, tool or limitation changed
