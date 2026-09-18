# Ubuntu and Debian distribution

APT is the default supported Ubuntu/Debian installation and update method.
AppImage remains the portable alternative. The bootstrap script and the
package build live under `packaging/`; publishing to the repository is a
maintainer operation.

## Installation after publication

Ubuntu desktop builds target Ubuntu 24.04 LTS or compatible newer systems,
on amd64 and arm64. They require GTK 4, WebKitGTK 6 and AppArmor, resolved by
APT from the OS repositories. Debian compatibility depends on those packages
and their ABI versions; validate a target distribution before advertising it.
The headless package has no GUI dependencies.

Download the bootstrap over HTTPS and run it as your normal user. It invokes
sudo for repository configuration and package installation:

```sh
curl --fail --show-error --location --output install-apt.sh \
  https://dist.taurinetech.com/pulse/apt/install-apt.sh
sh install-apt.sh --channel main --package desktop
```

Use `--package headless` for a system service without a desktop. The existing
headless enrollment installer remains available under `pulse/<channel>/install.sh`
and embeds this same APT installer, defaulting to `--package headless`. Both
routes install and upgrade through APT, preserving `/var/lib/axon-pulse`.
Headless pairing additionally accepts `--url https://controller.example` and
`--token-stdin` (recommended) or `--token` for legacy commands. The installer
reads the invite before administrator elevation and passes it to the installed
CLI only over stdin, verifies service readiness and APT ownership, and refuses
to consume a new invite if the sensor is already paired. Non-Debian Linux hosts
receive an explicit missing-APT error; portable binaries remain downloadable.
Desktop users open Pulse from Applications, return to the private claim page,
and select **Connect Pulse**. Run the GUI as the logged-in user, never root.

Existing `.deb` users run the same bootstrap once to subscribe to updates.
Settings and local history are retained. No logout is needed after an upgrade:
the APT-managed service stats its installed executable once a minute and, when
dpkg has replaced it, drains, flushes and re-execs the new build in place
(same PID, so systemd and the desktop's detached child are unaffected). A
desktop window that is closed relaunches itself when it notices the service
is a newer build; an open window shows **Restart Pulse** in Settings instead
of restarting under the user. Headless APT upgrades additionally
`try-restart` an active system service from `postinst` and preserve a stopped
service.

Subsequent manual updates:

```sh
sudo apt update
sudo apt install --only-upgrade axon-pulse-desktop
```

APT repository setup alone does not enable unattended installation. Opt in on
the stable stream with `--enable-auto-updates`. This installs Ubuntu's existing
`unattended-upgrades` package, enables its daily APT timers through the standard
periodic configuration, and allows only Pulse's `main` origin in addition to
the user's existing allowed origins. It adds no Pulse daemon or polling loop.
The desktop picks up upgraded code as described above. Validate with
`sudo unattended-upgrade --dry-run --debug` (downloads/logs may occur).

## Repository contract and channels

Base URL: `https://dist.taurinetech.com/pulse/apt`

| Item | Contract |
| --- | --- |
| Suites | `main` (stable default), `beta`, `alpha` |
| Component | `main` |
| Architectures | `amd64`, `arm64` |
| Packages | `axon-pulse-desktop`, `axon-pulse` |
| Public key | `axon-pulse-archive-keyring.gpg` |
| Bootstrap | `install-apt.sh` |
| Signed metadata | `dists/<suite>/InRelease` |
| Origin / Label | `Taurine Technology` / `Axon Pulse` |

For a repository mirror use `--apt-base-url https://mirror.example/pulse/apt`.
The mirror must retain the same signed metadata and archive key. Legacy
`--download-base-url https://mirror.example/pulse/<channel>` is accepted and
maps to its sibling `pulse/apt` repository and retains the URL's suite unless
`--channel` explicitly overrides it.

The bootstrap configures `/etc/apt/sources.list.d/axon-pulse.sources`, with a
dedicated `Signed-By` key at `/etc/apt/keyrings/axon-pulse-archive-keyring.gpg`,
and `/etc/apt/preferences.d/axon-pulse` to exclude unrelated packages from this
repository. It verifies the key's primary fingerprint pinned in the script
before making changes. HTTPS delivery of that script is the initial trust
root. APT then verifies signed repository metadata and package hashes.

Choose prereleases explicitly using `--channel beta` or `--channel alpha`.
The suite is controlled by APT sources, not the in-app stream selector.
Re-running setup changes that source; APT will not silently downgrade an
installed newer version when switching back to `main`. Wait for stable to
catch up, or deliberately select an older version after checking state-schema
compatibility. The optional unattended-upgrade rule matches `main` only.

Debian package versions use epoch `1` and translate the SemVer prerelease dash
to `~`, e.g. `1:0.0.8~alpha.1`. The binary still reports `0.0.8-alpha.1`.
The epoch allows APT to replace legacy `0.1.0-preview.*` packages, and the tilde
orders prereleases below the corresponding final version. Never remove this
epoch from future package releases. Package filenames retain the SemVer for
compatibility with the signed Pulse artifact index.

Release CI builds separate APT binaries with
`-X github.com/Taurine-Technology/axon-pulse/internal/update.installMethod=apt`.
Both desktop and headless APT builds refuse internal executable replacement
and skip internal update polling. Raw binaries and AppImages retain the built-in
updater. Do not package a portable build for APT.
