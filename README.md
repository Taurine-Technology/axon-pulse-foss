# Axon Pulse

Axon Pulse is Axon's lightweight subscriber-side network quality sensor. It
continuously measures gateway and internet latency, jitter and loss, DNS,
HTTPS reachability, captive portals, local link health, throughput and
responsiveness (bufferbloat), then keeps them locally or uploads connected-mode
minute aggregates to an Axon controller. Sensors use outbound HTTPS only; they
never join an Axon tailnet,
inspect subscriber traffic, or receive MQTT credentials.

The always-on service is pure Go and uses a bounded, crash-safe SQLite spool.
The desktop distribution is a single dual-mode executable: its native tray and
four-screen Wails viewer talk to the service through a mode-0600 local socket,
while the same executable runs the detached measurement mode. Closing the
window destroys the webview without stopping measurement. A standalone
headless binary remains available for servers.

## Connect

On first launch choose **Use Pulse locally** or connect a provider invite. Local
mode needs no account or hosted Axon endpoint and never uploads measurements.
The Settings screen can switch a claimed installation between local and
connected operation at any time.

Desktop subscribers open their provider's claim link, install Pulse, and click
**Connect Pulse**. The registered `axon-pulse://claim` deep link transfers the
controller address and one-time `spt_` token to the app. The token is exchanged
once; only a per-sensor secret is persisted with user-only permissions.

For a Linux server (systemd, x86-64 or ARM64), create an invite in
**Network → Sensors → Invite**, expand **Install on a Linux server**, and copy
the generated command to the target machine. The command includes the controller
address and invite, detects the architecture, verifies the download, installs
and starts the system service, and enrolls it. No browser is needed on the server.
It requests sudo access when run by a non-root user.

To supply the arguments yourself (replace the URL and token):

```bash
( installer=$(curl --proto '=https' --proto-redir '=https' -fsSL https://dist.taurinetech.com/pulse/main/install.sh) && bash -c "$installer" -- --url https://controller.example --token spt_REPLACE_ME )
```

The command downloads the complete installer before executing it. Treat it as
private: it contains a one-time invite and may be saved in shell history. For a
different stream, use its installer URL and pass `--channel beta` or
`--channel alpha`. A mirror can be selected with `--download-base-url`.
Existing installations retain their binary, service configuration, and state;
an already-paired sensor is left paired and the new invite is not consumed.
An enrollment failure leaves Pulse installed so the command can be retried.
The installer forwards the invite to `connect --token-stdin`, keeping it out
of the child CLI and sudo argument lists. Manual CLI users can also supply a
token through standard input instead of `--token`.

For manual headless setup, start `axon-pulse run` in a separate terminal (or
install the supplied system service), then use the CLI:

```sh
axon-pulse local
axon-pulse connect --url https://controller.example --token spt_...
axon-pulse mode connected
axon-pulse status
```

Every installation follows an update stream: `main` (stable, the default),
`beta`, or `alpha`. Desktop users change it from **Settings → Update stream**;
headless installs run `axon-pulse channel beta`. Switching persists at once and
re-checks for updates against the chosen stream.

Additional commands are `channel [main|beta|alpha]`, `test --profile household|saturation|content|capacity`, `pause`, `resume`,
`disconnect`, `support-bundle`, `logs`, and `version`.

## Ubuntu and Debian

Use the signed APT repository for installation and updates. See
[Ubuntu/Debian setup](docs/apt.md) for commands, supported systems, optional
automatic updates, and release setup. AppImage remains a portable alternative.

## Developer commands

A clean checkout builds with the Go toolchain alone: every dependency
resolves from the public module proxy and no GitHub credentials, `GOPRIVATE`
setting, or organisation secret is needed. See [CONTRIBUTING.md](CONTRIBUTING.md)
for prerequisites and the review policy.

```sh
make test
make lint
make build-all
make build-desktop
make size-check
make licenses
```

`make test` runs the race-detector suites of the root module, the desktop
module, and the local copy of the shared Axon packages.
`make lint` runs `go vet`, `gofmt`, and golangci-lint (pinned, run via
`go run`; rules in `.golangci.yml`, shared with Axon's other Go codebases)
over the root and desktop modules, plus `go vet` over the shared packages.
`make build-all` cross-compiles Linux, macOS, and Windows for amd64 and arm64
with `CGO_ENABLED=0`. Every headless binary must remain below 20 MiB.
`make build-desktop` builds the native host and enforces the 25 MiB installed
single-file budget plus the 200 KiB JavaScript budget.

See [docs/privacy.md](docs/privacy.md), [docs/operations.md](docs/operations.md),
the [architecture boundary](docs/architecture.md), the
[quality methodology](docs/internet-quality-methodology.md), and
[docs/release.md](docs/release.md) for the subscriber disclosure, admin
playbook, method, and channel/update contract.

The bounded measurement runners and small runtime helpers Pulse shares with
the Axon switch agent come from the Apache-2.0 module
`github.com/Taurine-Technology/axon-contracts/gen/go`. Until that module is
published, Pulse carries the packages it uses in
[`third_party/axon-contracts`](third_party/axon-contracts/README.md) and points
both Go modules at that directory with a `replace` directive; see
[ADR 0002](docs/adr/0002-local-copy-of-shared-packages.md).

## Contributing and security

Contributions are welcome under the process in
[CONTRIBUTING.md](CONTRIBUTING.md) and the
[code of conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md), never in a public issue.

## License

Axon Pulse is free software licensed under the
[GNU Affero General Public License v3.0](LICENSE). Bundled components under
other licenses are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
The Axon and Taurine names and logos are trademarks of Taurine Technology
(Pty) Ltd and are not covered by the software license.
