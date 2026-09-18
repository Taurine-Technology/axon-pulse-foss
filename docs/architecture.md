# Pulse architecture and public boundary

Pulse is one product with two operating modes, not two subtly different test
engines:

- **Local** runs measurement, scoring, history, background scheduling, the tray
  app, and the headless CLI without an Axon account. Records are marked
  `local_only` at the SQLite boundary and are excluded from batch selection.
- **Connected** uses the same measurement and scoring core, then adds Axon
  enrollment, signed ingest, controller policy, and updates. Switching modes
  closes the mode interval so a partial local minute can never enter an upload.

The initial setup screen makes this choice explicit. A connected installation
can switch to local mode without destroying its claim, and can later switch
back. `disconnect` removes the claim and controller queue, then leaves Pulse in
local mode.

## Extraction seam

| Public-safe core | Proprietary integration |
|---|---|
| `quality/`: stability, budgets, application outcomes, confidence | enrollment and HMAC request signing |
| measurement transports and bounded public endpoints | controller config and ingest contracts |
| local SQLite history and `local_only` enforcement | controller retention, fleet views, and alerts |
| scheduler, headless CLI, desktop UI, accessibility | Axon release channels and managed policy |
| typed gateway/public/custom targets and link observations | controller/site target and identity mapping |

The `quality` package has no Axon, database, UI, or network dependency. It is
the intended first extraction unit for a FOSS repository. Controller URLs,
claim tokens, sensor secrets, upload signing, and Axon-specific update logic
must never move across that boundary.

## Trust and privacy rules

1. Controller policy is untrusted input. Intervals, target counts, durations,
   bytes, concurrency, and profile spacing are clamped locally.
2. Local-only rows have a database-level eligibility bit. Upload selection
   contains an explicit `local_only = 0` predicate; UI state alone is not a
   security boundary.
3. Pulse never accepts arbitrary executable probes, joins a tailnet, receives
   broker credentials, captures packets, or reads browser/DNS history.
4. Raw SSID, BSSID, MAC, gateway address, hostname, claim token, secret,
   authorization header, and user path are excluded from support summaries and
   centrally redacted as a second line of defence.
5. Support export is local, explicit, inspectable JSON capped at 256 KiB. No
   automatic support upload exists.
6. Unsupported link fields are omitted, not reported as zero-valued evidence.
7. One-off controller tests are HMAC-bound to the current enrollment and use a
   short issue/expiry window plus durable replay protection. They run with the
   operator's intent (no scheduler gates) but inside controller-configured
   ceilings, and resolve with exactly one accepted/rejected verdict after the
   run.
8. Controller-provided ingest paths cannot change the user-trusted controller
   origin.

## Resource envelope

- One service ticker and bounded worker flags; no unbounded goroutine creation.
- At most eight probe targets; saturation tests use at most eight transfer
  requests and a household mix at most 32 paced HTTP/1.1 connections.
- Household tests pace activities to their demand under a 300 MB cap and
  usually spend far less; saturation tests run once a day with higher ceilings
  and are excluded from experience history by policy.
- Daily and monthly byte ledgers are persisted before another scheduled test.
- Unique telemetry JSON retained in SQLite has a 50 MiB logical payload budget
  and a seven-day lifetime, with crash-safe WAL and one connection. Status
  reports payload usage and physical database/WAL/SHM bytes separately; SQLite
  page, index, and retry-serialization overhead is outside the payload budget.
- Headless and desktop installed-size gates remain 20 MiB and 25 MiB.

## Public endpoint policy

The first transport uses Cloudflare's documented speed-test endpoints. The
official open-source engine documents `https://speed.cloudflare.com/__down`
and `__up`, supports latency requests with `bytes=0`, and is MIT licensed:
<https://github.com/cloudflare/speedtest>. Pulse keeps endpoint construction
behind a transport interface, enforces client-side caps, and treats endpoint
failure or disagreement as confidence evidence rather than bad subscriber
quality.

Public infrastructure is not assumed to provide an SLA. A future M-Lab/NDT
transport can be added as a separately identified source, but source results
must not be silently merged and disagreeing sources lower confidence.
