# Contributing to Axon Pulse

Thanks for helping improve Pulse. This document covers how to build the
project from a clean checkout, what checks a change must pass, and how we
review and license contributions. Read [SECURITY.md](SECURITY.md) before
reporting anything that could be a vulnerability.

## Build from a clean checkout

The public repository builds with the Go toolchain alone: no GitHub
credentials, private modules, or organisation secrets are needed, every
dependency resolves from the public module proxy, and the shared Axon packages
Pulse uses are carried in `third_party/axon-contracts`. (In Taurine's private
repository those packages come from the private `axon-contracts` module and
need `GOPRIVATE=github.com/Taurine-Technology/*` plus GitHub access.)

Requirements:

- Go as pinned in `go.mod` (currently 1.26.7).
- Node.js for the desktop frontend syntax checks and unit tests.
- Python 3 for the packaging tests.
- For the desktop module on Linux: `libgtk-4-dev` and `libwebkitgtk-6.0-dev`.
  macOS needs the Xcode command line tools. Windows builds the desktop with
  `CGO_ENABLED=0`.

```sh
git clone https://github.com/Taurine-Technology/axon-pulse-foss.git
cd axon-pulse-foss
make test        # race-detector tests in all three Go modules
make lint        # go vet, gofmt, golangci-lint (pinned, run through go run)
make build-all   # six headless targets under the 20 MiB size gate
make build-desktop
make licenses    # dependency license allowlist
```

The headless binary alone builds with `make build`. The desktop module tests
need the webview headers above; if you cannot install them, say so in the pull
request and CI will run them for you.

## Repository layout

- `cmd/axon-pulse`: CLI and headless service entry point.
- `internal/`: service, measurement, probing, aggregation, spool, uploader,
  protocol, state, IPC, update, and support packages.
- `quality/`: dependency-free scoring; see `docs/internet-quality-methodology.md`.
- `desktop/`: the Wails desktop host and its frontend.
- `packaging/`: installers, service units, and platform manifests.
- `third_party/axon-contracts` (public repository only): a copy of the
  Apache-2.0 shared Axon packages. Do not edit these files in place; see its
  README for how the copy is refreshed.
- `docs/`: architecture boundary, privacy disclosure, operations, methodology,
  release contract, and architecture decision records.

## What a change must satisfy

- `make test`, `make lint`, and `make licenses` pass locally, and `go mod tidy`
  leaves every `go.mod` and `go.sum` unchanged.
- New behaviour comes with tests. Concurrency and lifecycle changes need
  race-detector coverage; failure paths need explicit tests.
- Size budgets hold: 20 MiB per headless binary, 25 MiB for the desktop
  binary, and 200 KiB of frontend JavaScript.
- The trust and privacy rules in `docs/architecture.md` and the disclosure in
  `docs/privacy.md` stay true. Controller input is untrusted and clamped
  locally; raw identifiers stay out of support bundles and logs.
- Resource use stays bounded: no unbounded goroutines, queues, caches, or log
  growth, and no new polling loops without justification.
- New dependencies are rare and justified in the pull request. They must pass
  the license allowlist in the `Makefile`.
- Platform-specific files (`*_linux.go`, `*_darwin.go`, `*_windows.go`, IPC,
  update, packaging, desktop) should be exercised on that platform. CI runs
  the Linux, macOS, and Windows matrix on every pull request; mention in the
  description which platforms you tested yourself.

## Commits and pull requests

- Keep one logical change per pull request.
- Use the `type(scope): summary` convention already in the history, for
  example `fix(update): recheck the lock holder before replacing a bundle`.
- Fill in the pull request template: what changed, why, the checks you ran
  with their real results, risks and rollback, and the effect on CPU, memory,
  disk, and network use on subscriber machines.
- Sign off every commit (`git commit -s`) to certify the
  [Developer Certificate of Origin](https://developercertificate.org/). This
  states that you have the right to submit the work under the project
  license.

## Review policy

A maintainer listed in `.github/CODEOWNERS` reviews every pull request.
Changes to the following areas need explicit maintainer approval and a
description of the threat model or privacy impact:

- privacy: `internal/support`, redaction, consent handling, and `docs/privacy.md`;
- the updater and release verification: `internal/update`, `tools/release-index`;
- local IPC: `internal/ipc` and the desktop client;
- controller protocol and enrollment: `internal/protocol`, `internal/uploader`;
- installers, packaging, and GitHub workflows.

## Licensing of contributions

Axon Pulse is licensed under the GNU Affero General Public License v3.0
(see [LICENSE](LICENSE)). By contributing you agree that your contribution is
licensed under the same terms, as certified by your DCO sign-off. Code under
`third_party/` keeps its own license.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Report
conduct concerns to `info@taurinetech.com`.
