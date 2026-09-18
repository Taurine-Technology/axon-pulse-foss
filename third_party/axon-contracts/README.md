# Shared Axon runtime packages (local copy)

This directory is a partial copy of the Go module
`github.com/Taurine-Technology/axon-contracts/gen/go`, which is licensed under
the Apache License 2.0 (see `LICENSE` and `NOTICE` here). The upstream
repository is not yet publicly resolvable, so Pulse carries the packages it
needs and points both of its modules at this directory with a `replace`
directive. The import paths are unchanged, so removing the `replace` lines
restores the upstream dependency once it is published.

Copied from upstream tag `gen/go/v0.4.1`
(commit `1b4a3a23ef96b841642e1317bc898e7415fe407d`, subdirectory `gen/go`).

Packages included:

- `buildinfo`: product, version, commit, and date set through `-ldflags`.
- `logging`: `log/slog` setup with redaction and bounded rotation.
- `compress`: bounded zstd encode/decode helpers.
- `diagnostics`: transport-independent ping, DNS, HTTP/captive-portal, path
  trace, speed-test, responsiveness, and CPU-confidence runners.

Deliberately omitted from upstream:

- `pb`, `topics`, and `geo`: protobuf contracts and helpers for the Axon
  switch agent, which Pulse does not use.
- `diagnostics/handler*.go` and `diagnostics/orchestrator*.go`: the MQTT
  handler and protobuf adapters over the runners, plus their tests and the
  two adapter test cases in `httpcheck_test.go` and `pathtrace_test.go`.
- `diagnostics/doc.go` was reworded to describe this subset.

Everything else is byte-for-byte identical to upstream. To refresh: copy the
files listed above from the new upstream tag, reapply the omissions, update
the tag and commit recorded here, then run `go mod tidy` in this directory and
in both Pulse modules.
