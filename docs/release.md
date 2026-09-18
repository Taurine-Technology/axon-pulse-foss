# Distribution and update contract

Pulse publishes to Axon's own distribution endpoint at
`https://dist.taurinetech.com/`, under `pulse/{alpha|beta|main}/`. Each
channel directory holds one release: the artifacts, `checksums.txt`,
`latest`, `index.json`, and `index.json.sig`.
Installer filenames are stable across releases (only the Linux debs carry a
version), so a claim link or update URL never changes when a new build lands or
a channel is promoted. Each index entry identifies `version`, `os`, `arch`,
`kind`, a `url` relative to the index, lower-case SHA-256, and `size_bytes`.
`index.json.sig` is a detached Ed25519 signature over the exact index bytes.
Clients verify it against the public key embedded at build time before trusting
any artifact digest. The same signed build and metadata are copied between
channels during promotion; they are never rebuilt.

Ubuntu/Debian users should use the signed APT repository described in
[docs/apt.md](apt.md). Release publication and promotion are maintainer
operations performed by Taurine's release infrastructure.

Linux is published twice: a pure-Go headless tar/deb with no GUI dependencies,
and a desktop deb/AppImage with WebKitGTK. Windows publishes per-user and
managed installers. macOS publishes a universal signed/notarized DMG plus a
managed launchd asset.

Each release also generates `install.sh` by embedding the shared APT bootstrap
from `packaging/apt/install-apt.sh.in`, with the configured OpenPGP primary
fingerprint pinned in its source. This compatibility script defaults to the
headless package and accepts the same controller/invite flags as older claim
commands. It is indexed as `bootstrap` and included in `checksums.txt`, so
publication and promotion carry its exact bytes with the rest of the release.
New claim pages and UI commands use `pulse/apt/install-apt.sh --package headless`
with `--token-stdin`; both routes use APT for installation and later updates.
There is no raw-binary installation fallback in these scripts.

The HTTPS-delivered script is the initial trust root. It verifies the pinned
archive-key fingerprint, configures a scoped APT source, and lets APT verify
signed repository metadata and package hashes. Headless pairing uses the
system service's state and stdin-only CLI token flow. Existing claims and
measurement history are preserved. See [APT setup and rollout](apt.md) for
channel selection, dependencies, and migration.

The service owns updates. It downloads only HTTPS artifacts matching its OS,
architecture and configured channel, verifies exact positive size and SHA-256
before staging, closes measurement/upload admission, drains admitted work, and
durably flushes before activation. Desktop updates retain the previous slot
until the exact replacement service process stays alive and its local IPC
response reports the authenticated target version; the probe itself also runs
from a binary with that version. The GUI starts only after this bounded health
contract succeeds. On macOS the
helper then hands the running window over through its single-instance
handler; a window too old to understand the hand-off (0.0.7-alpha.2 and
earlier) is terminated after a bounded wait and the bundle reopened, so an
updated service never runs beneath a stale interface. The window also compares
its own build with the service version on every status update and offers a
**Restart Pulse** button when they differ, and the update button shows live
download progress while an update stages. Failure terminates the tracked service PID, restores the
previous slot, and relaunches its service and GUI. Windows requires valid
Authenticode from the installed publisher before a detached helper runs Inno
Setup and supervises the new service. macOS requires Gatekeeper acceptance and
the installed Team ID before an A/B bundle swap. Linux AppImages use the same
post-launch IPC rollback contract. Headless executable updates perform a
pre-swap version probe and retain the previous binary; both desktop and headless Debian
installs remain under the system package manager. Update checks use only Axon's
self-hosted distribution endpoint. Pulse v1 does not transmit crash telemetry
at all: faults remain in bounded local structured logs, which excludes
measurement payloads, claim tokens, and sensor credentials.

## Channels and installed clients

APT installations select their suite in the APT source configuration and do
not use the in-app stream selector. Other installations follow one update
stream, resolved in this order:

1. `AXON_PULSE_UPDATE_INDEX_URL` in the service environment pins a full index
   URL for managed installs and locks the stream; the app shows it as set by
   the administrator and the `channel` command cannot change it.
2. The stream the user chose, persisted in the service state and changed from
   **Settings → Update stream** in the desktop app or `axon-pulse channel
   <main|beta|alpha>` headless. Changing it swaps the update source at once,
   discards anything found or staged from the previous stream, and runs a
   fresh check so the user immediately sees what the new stream offers. A
   change is refused only while an update is downloading or installing.
3. `AXON_PULSE_UPDATE_CHANNEL` in the service environment.
4. `main`.

Controllers select the APT suite through `AXON_PULSE_RELEASE_CHANNEL`
(`main`, `beta`, or `alpha`). `AXON_PULSE_DOWNLOAD_BASE_URL` is only an optional
portable-download override; its default is derived from that explicit channel.
Existing controllers that previously selected Alpha/Beta solely through the
portable URL must also set the new release-channel variable.
Because promotion copies identical bytes, a sensor that keeps the default
stream upgrades when a build is promoted to `main`; a sensor switched to
`beta` upgrades as soon as the build lands there.
