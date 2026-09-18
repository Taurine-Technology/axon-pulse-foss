# What Axon Pulse sends

In **Local** mode, Pulse sends no measurement records to Axon. Synthetic probe
and speed-test traffic still reaches the configured public test endpoints, as
it must to measure the Internet path, but results remain in the bounded local
SQLite store. A database-level `local_only` marker prevents those rows from
entering a controller batch if the user later switches to Connected mode.

In **Connected** mode, the controller receives the aggregates below. Switching
back to Local mode stops new upload eligibility without deleting the claim.

Axon Pulse measures only traffic that it creates. It does not capture packets,
read DNS or web activity from other applications, inspect content, scan the
local network, or join the Axon VPN/tailnet.

The controller receives one-minute aggregates of:

- latency, jitter and packet loss to the default gateway, controller, and
  configured public anchors;
- cold and warm DNS timing, resolver outcome, and NXDOMAIN-hijack detection;
- synthetic HTTPS DNS/connect/TLS/TTFB timing and captive-portal outcome;
- interface type, link rate, signal strength, channel, PHY, metered/battery
  flags, per-field availability, and route-change events where the OS exposes
  them;
- scheduled or user-started throughput and latency-under-load results,
  bufferbloat grade, confidence, cross-traffic context, and byte use;
- app version, uptime, queue depth, clock skew, and lifecycle events.

Enrollment (claiming a sensor) sends a one-time registration payload: the
single-use claim token, a random sensor UID, the device's OS hostname (so the
operator can recognise the device — note a hostname is often a personal name),
operating system and CPU architecture, and the Pulse app version.

The controller also records the public source IP it necessarily sees on
enrollment and heartbeat so an operator can identify coarse network geography;
Pulse does not call a separate location service or collect precise location.

Wi-Fi network names (SSIDs) are disabled by default. Pulse reads and sends an
SSID only when the subscriber explicitly opts in on that device **and** the
administrator enables the bounded controller config field. With consent, an
SSID can remain in the HTTPS upload queue or local history for at
most seven days and within a 50 MiB logical telemetry-payload budget, including
while offline. SQLite pages, indexes, and retry bookkeeping add physical storage
overhead; status reports the actual database/WAL/SHM bytes separately.
Withdrawing consent immediately clears in-memory aggregates and the local
spool after waiting for active collectors and uploads to finish, so an in-flight
collector cannot re-add an SSID after withdrawal returns. A durable pending
marker blocks every upload and heartbeat before deletion starts. The state and
a separately fsynced purge-intent sentinel durably distinguish SSID withdrawal,
disconnect, and revocation, so a failed state rewrite or restart still performs
the required credential wipe as well as queue deletion; only successful cleanup
clears both. A revocation also sets an in-memory latch before either write,
keeping the process fail-closed while storage is unavailable and retrying the
durable intent before any later network request.
Queued SSIDs cannot be retried.

Raw BSSID/MAC and default-gateway addresses are never placed on the wire. A
controller can separately opt in to 128-bit HMAC pseudonyms for the current
network and first hop. They are keyed by a device-local random secret that is
never sent to the controller and rotates when the sensor is re-enrolled. The
pseudonyms remain only under the same seven-day raw record / 90-day aggregate
retention bounds. With the option off (the default), both identifiers are
absent rather than zero-valued. An SSID is never used as an identity fallback
unless the separate SSID consent gate is active.

Controller-managed anchors, DNS resolvers, and synthetic web targets are
restricted to globally routable destinations. Pulse re-checks resolved
addresses when it dials, rejects redirects, and does not let controller policy
turn these measurements into private-network scans. The explicitly entered
controller and the device's own default gateway remain separate local-user
trust boundaries.
Controller-provided ingest paths must remain on the exact controller origin the
user enrolled; cross-origin ingest destinations are rejected before storage or
dialing.

Claim credentials and the offline spool are stored in the user's application
data directory with user-only permissions. **Disconnect** or uninstall removes
both. Uploaded SSIDs follow the same provider-side retention as raw minute data:
14 days by default. Hourly aggregates and events remain for up to 90 days by
default; providers must disclose any policy override.

Support bundles are created only on an explicit local export. They contain
version and service state, sanitized config counts and budgets, spool/upload
counters, recent record and state-transition counts, and categorized errors.
They exclude raw measurements and are centrally redacted for credentials,
authorization headers, URLs with queries, SSID/BSSID/MAC, IP addresses,
the device's own hostname, and user paths. A bundle is capped at 256 KiB and is never uploaded
automatically.

Pulse also keeps at most three user-private local log files (the active file
and two rotations), each capped at 512 KiB. Log attributes use the same central
redaction path before reaching either the local file or platform logger.
Consented bundle upload is intentionally not implemented until the controller
defines a short-lived upload grant, authenticated size limit, expiry, and audit
contract; local export remains the safe supported handoff.
