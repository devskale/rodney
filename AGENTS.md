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
re-enable them. The local guardrail replaces them — **depth ladder, fast by default**:

- `./scripts/check.sh` — fast tier: `go vet` + `go build` (~seconds)
- `./scripts/check.sh --full` — adds `go test` (~98s); run when asked ("test", "validate")
- `git push` runs the fast tier automatically (pre-push hook, `.githooks/`)
- `CHECK=1 git push` — full tier before pushing; `PUSH_SKIP_TESTS=1 git push` — WIP escape hatch
- `main_test.go` must be updated in the same change as `main.go` — the fast tier does
  NOT compile tests; the full tier catches it. Skill-level integration tests live in
  skale-skills (`tests/rodney/test.sh`), not here.

## Conventions

- `main_test.go` must be updated in the same change as `main.go` — a signature
  change without its test update broke the build once (extractScopeArgs); the
  fast guardrail tier does not compile tests, so run `--full` when signatures change.
- Version lives in `main.go` (`var version`), set via `-ldflags` for releases.
- Upstream: `origin` (simonw/rodney). This fork: `fork` remote, branch `skale`.
  Rebase on upstream occasionally; keep fork-specific changes minimal and
  clearly marked.
