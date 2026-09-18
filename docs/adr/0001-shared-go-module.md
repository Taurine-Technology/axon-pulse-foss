# ADR 0001: Shared Go code lives in axon-contracts/gen/go

- Status: Accepted; amended by [ADR 0002](0002-local-copy-of-shared-packages.md),
  which gives the public repository a copy of the packages Pulse uses until the
  module is public
- Date: 2026-08-15

## Context

The switch agent already contains the bounded DNS, HTTP, path trace, speed test,
and responsiveness engine Pulse needs. Keeping a second copy would allow grading
and safety limits to drift. A new repository would add release and private-module
infrastructure without creating a clearer ownership boundary: these packages
are wire-adjacent and already depend on Axon protobuf contracts.

The contracts repository root is AGPL-3.0. Pulse must not import GPL code.
Taurine owns the extracted agent code and can license the Go submodule
separately.

## Decision

Reusable Go packages live in the existing
`github.com/Taurine-Technology/axon-contracts/gen/go` module:

- `diagnostics`: transport-independent measurement runners plus the existing
  switch-agent protobuf adapter;
- `buildinfo`, `compress`, `geo`, and `logging`: small shared runtime
  packages.

The `gen/go` module has an explicit Apache-2.0 `LICENSE` and `NOTICE`.
The repository root and Python package remain AGPL-3.0. Both Pulse and the
switch agent run a permissive-license allowlist over their compiled dependency
graphs.

The diagnostics engine invokes no external `ping` binary. Linux and macOS use
unprivileged ICMP datagram sockets, Windows uses `IcmpSendEcho`, and a bounded
TCP handshake is the last-resort fallback. Linux CPU confidence reads procfs
behind a build-tagged implementation; other operating systems report the sample
as unavailable rather than failing a measurement.

## Consequences

Contracts must be released before dependent agent/Pulse commits. The first
release containing these packages is `gen/go/v0.4.0`. Shared diagnostics tests
run on Linux, macOS, and Windows CI. Breaking public Go API changes require a
coordinated module release and consumer updates.
