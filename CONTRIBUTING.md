# Contributing to kaectx

This file covers the branch model, pull requests and releases. Building, testing
and the code layout are in the [README](README.md#development); the decisions
that are already made are in [CLAUDE.md](CLAUDE.md).

## Branch model

- `master` is the default branch and the only long-lived one. It holds the
  newest reviewed code, released or not. **It changes only by merging a pull
  request.**
- Releases are tags (`v0.2.0`) on `master`. `go install ...@latest` resolves the
  newest tag, not the tip of `master`, so merged-but-unreleased work never
  reaches a user who installs normally.
- Everything else is a short-lived branch cut from `master` and deleted once
  its PR merges.

There is no `dev` or `staging` branch. Those exist in projects that deploy a
branch to a server; kaectx ships as a tag that each user builds, so a second
integration branch would add a promotion step that guards nothing.

### Branch names

`<prefix>/<issue>-<slug>`, for example `fix/12-codex-title-redaction`. Several
issues delivered together list them all: `feature/14-15-pi-adapter`. Work with no
issue behind it drops the number: `chore/branching-hygiene`.

| Prefix | For |
| --- | --- |
| `feature/` | New capability: a command, MCP tool, adapter, migration. |
| `fix/` | A bug in released or merged behaviour. |
| `hotfix/` | A fix that needs its own patch release straight away (see [Releasing](#releasing)). |
| `chore/` | Build, CI, tooling, refactors, dependency bumps. |
| `docs/` | README, CONTRIBUTING, comments only. |
| `release/vX.Y.Z` | The version bump and release-notes PR for one release. |

### The guard

`.githooks/pre-push` refuses, before anything leaves your machine:

- any push to `master`,
- a `v*` tag that doesn't point at a commit already on `origin/master`,
- moving or deleting a `v*` tag that has been pushed,
- a new branch whose name doesn't follow the table above.

Enable it once per clone (worktrees share it):

```
git config core.hooksPath .githooks
```

Never bypass it with `--no-verify`. If it refuses something you believe is
right, the rule is wrong; change it in a PR. GitHub branch rules on `master`
enforce the same thing server-side for clones without the hook.

## Day to day

```
git switch master
git pull --ff-only
git switch -c feature/<issue>-<slug>
# ...commit...
git push -u origin feature/<issue>-<slug>
gh pr create --base master
```

When `master` moves while your branch is open, merge it in
(`git merge origin/master`) rather than rebasing a branch someone may already
be reviewing.

After the PR merges, delete the branch on both sides and return to `master`:

```
git switch master
git pull --ff-only
git branch -d feature/<issue>-<slug>
git fetch --prune
```

### Commits

Subjects follow [Conventional Commits](https://www.conventionalcommits.org),
as the history already does: `feat:`, `fix:`, `docs:`, `build:`, `chore:`,
`test:`, `refactor:`. An optional issue scope reads well in the log:
`fix(#12): redact Codex session titles`. The subject says what changes for the
user; the body says why, and what the reader would otherwise have to rediscover.

## Pull requests

Target `master`. CI (`.github/workflows/test.yml`) runs the test, vet and build
gate on Linux and on native Windows; both must pass before merging.

Merge with an ordinary **merge commit**. Squashing throws away the
`feat:`/`fix:` commits that release notes are written from, and rebasing
rewrites commits a reviewer already read.

The PR template asks for the rest. Before requesting review, run the same gate
CI runs:

```
go vet -tags sqlite_fts5 ./...
go test -count=1 -tags sqlite_fts5 ./...
```

On native Windows, `.\scripts\install-windows.ps1 -RunTests` runs it with the
MSYS2 compiler.

Say in the PR body which issues it closes (`Closes #12`). GitHub closes them on
merge, which for this repo is the right moment: merged work is what the next
tag ships.

### Changes that need extra care

- **Schema.** Never edit a migration that has shipped in a tag. Add a new one,
  and say in the PR how an existing database is upgraded and whether a
  `ctx repair` command is needed afterwards. Test both a fresh `ctx init` and
  an upgrade from the previous release's database.
- **MCP tools.** Tool names, arguments and output fields are what connected
  agents depend on. Call out any rename or removal in the PR; it makes the next
  release a minor bump.
- **Adapters.** Add a real, redacted fixture to the adapter's `testdata/` before
  changing its parsing.

## Releasing

Versions follow [Semantic Versioning](https://semver.org) as it applies to
0.x: while the major version is 0, a **minor** bump (`0.2.0` → `0.3.0`) marks a
schema migration, an MCP tool or output change, or a removed command or flag.
A **patch** bump (`0.2.0` → `0.2.1`) is fixes only.

1. Cut `release/vX.Y.Z` from `master`. In it, bump the version in
   `internal/mcpserver/server.go` and the release status and `@vX.Y.Z` pin in
   the README. Open a PR; its description is the draft release notes.
2. Merge the PR once CI passes.
3. Tag the merge commit and push the tag (the hook checks it is on `master`):

   ```
   git switch master
   git pull --ff-only
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

4. Publish the GitHub release from the tag, with notes written for someone
   upgrading from the previous version: upgrade steps first, then what changed.

   ```
   gh release create vX.Y.Z --title vX.Y.Z --notes-file notes.md
   ```

5. After the tag, set the MCP server version back to `X.Y.Z+dev` in a
   follow-up PR, so builds from `master` don't claim to be the release.

A tag is permanent: the Go module proxy caches each version the first time
anyone asks for it. If a release is broken, fix it on `master` and tag the next
patch; never move or delete the tag.

A **hotfix** is a `hotfix/<issue>-<slug>` PR into `master`, merged as soon as
CI passes and followed straight away by a patch release (steps 1–5). If `master` already holds work not ready to
ship, revert it on `master` through a PR first; there is no way to release one
commit past the others.
