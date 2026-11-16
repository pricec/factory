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
