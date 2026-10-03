# Rodney (devskale fork) — Agent Instructions

Fork of simonw/rodney on branch `skale`. Distributed as release binaries
consumed by the [skale-skills](https://github.com/devskale/skale-skills)
`rodney` skill (installer: `skills/rodney/install.sh`).

## Build & test policy — on request only ("auf Zuruf")

**Never build, test, or release proactively.** No background builds, no
auto-triggered checks, no "while we're at it" runs. Only when explicitly
asked ("bau", "test", "release", "validate").

- **Build**: `go build -o ~/.local/bin/rodney .` (or cross-compile per target
  with `GOOS`/`GOARCH`, see `scripts/release.sh`)
- **Test**: `go test ./...` (~98s — the full suite, local, no CI)
- **Release**: `bash scripts/release.sh <version>` — cross-compiles all 4
  targets, tars, creates the GitHub release via `gh`, uploads assets

## No GitHub Actions

Actions are **disabled** in this fork (zero CI minutes — the account quota is
scarce). Workflows live in `.github/workflows-disabled/` for reference; do not
re-enable them. There is no CI safety net:

- run `go test ./...` locally before pushing (when asked to)
- the skill-level integration tests live in skale-skills
  (`tests/rodney/test.sh`), not here

## Conventions

- `main_test.go` must be updated in the same change as `main.go` — a signature
  change without its test update broke the build once (extractScopeArgs).
- Version lives in `main.go` (`var version`), set via `-ldflags` for releases.
- Upstream: `origin` (simonw/rodney). This fork: `fork` remote, branch `skale`.
  Rebase on upstream occasionally; keep fork-specific changes minimal and
  clearly marked.
