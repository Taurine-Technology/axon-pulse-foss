# Axon Pulse

Axon Pulse is a lightweight network quality sensor for the edge of a
subscriber's network. It continuously measures gateway and internet latency,
jitter and loss, DNS, HTTPS reachability, captive portals, local link health,
throughput and responsiveness (bufferbloat), and scores the result.

It runs on its own with no account, no controller and no uploads: all
measurements stay on the device. Optionally, it can connect to an
[Axon](https://taurinetech.com) controller and upload per-minute aggregates
over outbound HTTPS.

The always-on service is pure Go with a bounded, crash-safe SQLite spool and a
20 MiB binary size budget. The desktop app adds a tray icon and a small native
viewer in the same executable.

## Get started

Requirements: Go as pinned in `go.mod`. The desktop build on Linux also needs
`libgtk-4-dev` and `libwebkitgtk-6.0-dev`; macOS needs the Xcode command line
tools.

Build and run the headless sensor locally:

```sh
git clone https://github.com/Taurine-Technology/axon-pulse-foss.git
cd axon-pulse-foss
make build                       # bin/axon-pulse
bin/axon-pulse run &             # start the measurement service
bin/axon-pulse local             # keep everything on this machine
bin/axon-pulse status
bin/axon-pulse test --profile household
```

Build the desktop app instead with `make build-desktop`. On first launch choose
**Use Pulse locally**.

Other useful commands: `logs`, `pause`, `resume`, `support-bundle`, `version`
and `help`. Cross-compile every headless target with `make build-all`, and run
the checks with `make test` and `make lint`. See [CONTRIBUTING.md](CONTRIBUTING.md)
for the full build and review process.

## Install a prebuilt package

Taurine builds, signs and maintains packages of this code.

**Ubuntu and Debian (APT).** The signed repository provides both the headless
service and the desktop app, and keeps them updated with `apt`:

```sh
curl --fail --show-error --location --output install-apt.sh \
  https://dist.taurinetech.com/pulse/apt/install-apt.sh
sh install-apt.sh --channel main --package headless   # or --package desktop
```

See [docs/apt.md](docs/apt.md) for supported systems, channels and optional
unattended upgrades.

**macOS and Windows.** Signed installers (a notarized macOS DMG and Windows
installers) are published per release stream. The easiest way to get them is
through an Axon controller: your provider sends a claim link, which offers the
download for your platform and connects the sensor with one click.
Ubuntu users can also download a portable AppImage the same way.

## Connect to an Axon controller

Connecting is optional. Desktop users open the claim link and click
**Connect Pulse**. Headless installs use the command shown in the controller
under **Network → Sensors → Invite**, or connect by hand:

```sh
axon-pulse connect --url https://controller.example --token spt_...
```

Switch back with `axon-pulse local` or from **Settings** in the desktop app.
Connected sensors only ever make outbound HTTPS requests and never inspect
subscriber traffic; see [docs/privacy.md](docs/privacy.md).

## Learn more

- [Architecture boundary](docs/architecture.md)
- [Measurement methodology](docs/internet-quality-methodology.md)
- [Operations](docs/operations.md) and [release contract](docs/release.md)
- [Shared Axon packages](third_party/axon-contracts/README.md) carried in-tree

## Contributing and security

Contributions are welcome under [CONTRIBUTING.md](CONTRIBUTING.md) and the
[code of conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md), never in a public issue.

## License

Axon Pulse is free software licensed under the
[GNU Affero General Public License v3.0](LICENSE). Bundled components under
other licenses are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
The Axon and Taurine names and logos are trademarks of Taurine Technology
(Pty) Ltd and are not covered by the software license.
