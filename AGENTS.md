# factory

Distributed agent factory: a `factory` controller (web UI + orchestration)
directs `foreman` agents on worker machines; each foreman spawns one-shot
`worker` processes that run an agent for a single job - opencode, or the
Claude Code CLI for claude-* models (spec §8.4) - and report the result.

## Verify

Run `make ci` before finishing any change; it must pass.

## Style

- Format with gofumpt.
- Prefer table-driven tests.
- Stdlib-first: any new dependency must be justified in the PR.

## Contracts

All wire contracts live in `docs/spec.md` (normative). Do not invent or
change contracts in code without updating the spec.

## Targets

Linux only: amd64 and arm64.

Reproducible release builds: `make release-docker` (pinned golang image);
`make release` is the native path.

## Checkouts

- Builds never commit `dist/` (or a root `factory` binary); both are gitignored. `make release` regenerates `dist/`.
- Job worktrees are removed when the job reports.
- Never work in the operator checkout (`/home/price/temp/factory`); it holds uncommitted work. Use your own worktree.
- Finished work is always pushed before reporting.

## Host access

Before asking the operator for SSH or host access, call `list_foremen`
(its `hosts` view groups foremen by machine). If the target hostname or IP
is a foreman's host, pin a job to that foreman (`create_job foreman=`)
instead of asking for SSH (spec §6.7.1).

## Owning a change through to the merge

A job that changes code owns it until the merge is requested. Do not hand
"queue the merge" or any other follow-up to the planner.

1. Work on a branch in your own clone or worktree.
2. Before requesting the merge, merge `origin/master` into your branch
   (`git fetch origin && git merge origin/master`: merge, never rebase),
   resolve any conflicts, and re-run `make ci`. Then push the branch.
   Other jobs land while you work; this keeps the factory's merge from
   failing on a conflict or stale checks.
3. Call the `request_merge` tool yourself with the branch and the pushed
   head sha (`note` = PR title). If your branch needs another job's branch
   on master first, pass `after: ["that-branch"]`: the request waits for
   it.
4. Report done, with the branch, head and merge request id.

The factory merges one request at a time by `docs/merging.md` and tracks
the outcome on your task. If the merge fails on red checks, a conflict or
a moved head, it dispatches a fix job on the same branch (at most twice);
anything else goes to the operator. If you push again after requesting,
call `request_merge` again with the new head.

Jobs may declare `touches` (repo-relative file globs); the factory holds a
job whose `touches` overlap an active job's until that job's merge lands
(spec §6.14). Keep your edits inside the files you were told you touch.

Talk to the planner (`ask_parent`, or a failed report) only when you need
coordination with other jobs or a decision only the operator can make.
Decide everything else yourself.
