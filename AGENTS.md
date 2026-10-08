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

## Orphan GC: TTL + session cap (2026-10-11, v0.11.3)

`sweepOrphans()` runs on every `start`. Two passes:

1. **PID pass** (original): kill rod-family Chromium whose PID is referenced by
   no live `state.json`. Catches hard-killed runs.
2. **Age pass** (v0.11.3): kill a browser **older than the TTL even while its
   PID is alive and its state is valid**. This is the hung-Chrome case the PID
   pass structurally cannot see — it protected it forever. Anchored on
   `State.StartedAt` (unix seconds, set at launch). `StartedAt <= 0` (state from
   an older rodney) is never swept by age — unknown age must not mean death.

Knobs: `RODNEY_ORPHAN_TTL_HOURS` (default 24, `0` disables),
`RODNEY_MAX_SESSIONS` (default 6, `0` disables). The cap counts existing named
sessions and refuses with the names to stop.

Decision logic lives in the pure `ttlExpired(startedAt, now, ttlHours)` — unit
tests cover disabled / unknown-age / fresh / boundary / clock-skew. The live
behaviour (fresh survives, old-live killed, global session intact) is verified
end-to-end in a sandboxed `HOME` with real sleeping PIDs.

## Gotchas

See [README.md#gotchas](README.md#gotchas) and
[notes.md#incident-diskfull](notes.md#incident-diskfull) for load-bearing traps.

- **`--single-process`** — removed by default in 0.11.1. Was causing SIGABRT
  crash loops that produced ~1.2 TB core dumps per crash and filled the disk
  in seconds. Only opt in via `RODNEY_SINGLE_PROCESS=1` for gVisor/container
  environments.
- **`--disable-features=HistoryEmbeddings`** — pinned Chromium's on-device AI
  history-embeddings model CHECK-fails on text-heavy pages. Always present on
  launch.
- **Video recording disk cap** — `start-video` caps total frame bytes (default
  2 GiB, `--max-mib` to override, 0 = unlimited). Frames accumulate across
  CLI invocations; unattended recordings hit the cap and self-stop.
