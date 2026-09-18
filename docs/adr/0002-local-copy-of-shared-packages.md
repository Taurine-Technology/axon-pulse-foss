# ADR 0002: The public repository carries a copy of the shared Go packages

- Status: Accepted (amends ADR 0001)
- Date: 2026-09-18

## Context

ADR 0001 placed the measurement runners and small runtime helpers in the
`github.com/Taurine-Technology/axon-contracts/gen/go` module so the switch
agent and Pulse share one bounded engine. That repository is private and the
module is not on the public Go proxy, so a clean anonymous checkout of Pulse
could not run `go mod download`, and every CI job needed an organisation
token. Pulse is being published as open source under the GNU Affero General
Public License v3.0, and a public project must build without credentials and
must not require contributors to hold a token for a private repository.

Pulse uses four packages from that module: `buildinfo`, `logging`,
`compress`, and the transport-independent runners in `diagnostics`. It does
not use the protobuf contracts (`pb`), the MQTT topic helpers (`topics`),
`geo`, or the protobuf orchestrator and MQTT handler layered over the
runners. Those parts describe the switch agent's broker protocol and belong
to the agent, not to Pulse.

## Decision

Taurine's private repository keeps depending on the private module. When the
public tree is exported, the four packages are copied into
`third_party/axon-contracts` together with the module's Apache-2.0 `LICENSE`
and `NOTICE`, and both Pulse modules keep the original import paths and gain a
`replace` directive that points the module path at that directory. The copy omits the packages and adapter files listed
above and the two adapter test cases that depended on them; every other file
is identical to upstream tag `gen/go/v0.4.1`. The README in that directory
records the upstream tag and commit and the refresh procedure.

The copied module is a separate Go module with its own `go.mod`: `make test`
runs its tests, `make lint` runs `go vet` over it, and CI verifies its
manifests are tidy. It is deliberately not subject to Pulse's golangci-lint
rules so that upstream files can be refreshed byte-for-byte.

The `replace` directive is the only coupling. When the upstream module is
published to the public proxy, deleting the two `replace` lines and the
directory restores the shared dependency without touching any import.

## Consequences

- In the public repository `go mod download`, `make test`, and
  `make build-all` succeed with no GitHub credentials, no `GOPRIVATE`, and an
  empty module cache, and its CI receives no secret, so pull requests from
  forks run the full checks. The private repository's CI is unchanged.
- Removing the protobuf adapters also removed `google.golang.org/grpc`,
  `google.golang.org/protobuf`, and `genproto` from both Pulse modules.
- The copy can drift from the switch agent until it is refreshed. Refreshes
  are a file copy plus `go mod tidy`; the README in the directory lists the
  exact steps. Fixes to the runners should be made upstream first and then
  copied, not edited in place.
- Pulse itself is AGPL-3.0. The Apache-2.0 copy is compatible with that
  license, and the compiled dependency graph stays on the permissive
  allowlist enforced by `make licenses`.
