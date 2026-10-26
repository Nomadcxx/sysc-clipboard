# Contributing

The clipboard history daemon: it keeps the history and answers over a socket. This document says

what a change has to clear and where the work is tracked.

The rules of the organisation are written down in the shell repository, in `AGENTS.md`.

## The commit gate

Every commit must pass these commands from the root of the tree:

```sh
gofmt -w . && test -z "$(gofmt -l .)"
go vet ./...
go test -race -count=1 ./...
go build ./...
```

A gate script must record each step's real exit code. One trailing exit code can belong to a harmless
command and report success on a failing suite.

## The tracker

`bd` is the only tracker. Not a markdown list of tasks, not a list in a document, not a list in a
session. A task that lives in a document drifts from the tree and is lost with the document.

- `bd ready` answers what can be worked on now, and `bd blocked` shows what is gated and by what.
- Claim work with `bd update <id> --status in_progress` and close it with
  `bd close <id> --reason "..."`.
- Record work you discover as `bd create "..." --deps discovered-from:<id>`.

The tracker of the organisation is in the shell repository, and one graph holds every blocker: a change here that is gated on another repository is modelled as an issue there.

## Where plans live

Plans, designs and handovers are not in this repository. They are gitignored, and they live in the
primary checkout of the repository they govern, under `docs/plans/`. A worktree does not have a copy:
read and write them at the primary checkout's path.

- The name is `YYYY-MM-DD-<topic>[-<kind>].md`.
- A plan is written before the work it governs starts.

## Releasing

The order is pull request, CI green, merge, tag. A tag cut on a side branch leaves the release commit
outside `main`, and CI never evaluates it.

Release notes are generated from the commit history: the release workflow creates the release with `gh release create --generate-notes`, and the notes of a release are the commits between the tags. There is no changelog file to maintain, because a changelog nobody maintains is worse than none: the documents of this organisation drifted once already.

## Before diagnosing a test timeout

Check the machine first: `uptime`, and look for processes an earlier run left behind. A goroutine dump
is the second tool, not the first.

## Check the tree you are changing

Confirm a constraint against `git show origin/main:AGENTS.md` before acting on it when the
working copy may lag. Verify every claim against the `origin/main` of the repository the
claim is about, never a stale local clone. A correction sourced from a checkout 26 commits behind made
a document more wrong.
