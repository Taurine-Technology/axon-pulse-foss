# Internet quality methodology

Pulse reports connection quality and measurement confidence separately. A slow
or laggy connection can be measured with high confidence; a fast-looking result
can have low confidence when the test never stabilized.

Pulse also keeps two kinds of evidence apart. The **household** profile
reproduces the activities people actually run and asks whether they coexist;
it is what the everyday cards and the representative history are built from.
The **saturation** profile drives the line to its limit; its results answer a
different question and stay in their own diagnostic card. A line that is only
saturated now and then is never graded as if it always were.

## Profiles and budgets

| Profile | Purpose | Default data ceiling | Scheduled | Experience eligible |
|---|---|---:|:---:|:---:|
| Household | Configured everyday mix, paced to demand | 300 MB total (225 MB down + 75 MB up) | Every 6 hours by default | Yes |
| Saturation | What happens at the path's limit | up to 1 GiB down + 512 MiB up | Once a day by default | No |
| Content (legacy) | Pre-household everyday test, still run for older controllers | up to 256 MiB down + 96 MiB up | Only when configured | Yes |
| Capacity (legacy) | Pre-household manual saturation test | up to 512 MiB down + 256 MiB up | Only when configured | No |

The household ceiling is a cap, not a target: the default mix (two 4K
streams, a video call, a game and background browsing) is about 38 Mbps of
demand and spends roughly 70 MB in a run. Saturation ceilings are also not
targets. Warm-up measures the link rate and each direction is allocated only
what sustaining that rate through the phase time caps requires (with
headroom); a phase also ends as soon as its throughput stabilizes. A 20 Mbps
link therefore spends a few megabytes per saturation test while a gigabit
link may spend the whole ceiling. The daily (4096 MB) and monthly (61,440 MB)
ledgers remain the hard spend bound on every profile.

Every result records requested and effective bytes, duration, concurrency,
predicted consumption, actual use, and whether a cap stopped it. The sensor
enforces daily and monthly ledgers, minimum spacing, one active test, metered
and low-battery gates (scheduled load is deferred below 20% charge or when the
charge level is unreadable — running on a healthy battery is fine, and a
machine with no battery at all counts as mains powered), and a maximum of
eight requests even if controller policy asks for more. These gates apply to
scheduled tests only. A test the user starts by hand and a one-off an
operator requests from the console are both explicit intent: they always
run, with cross traffic, battery, and metering recorded as annotations on
the result instead. Manual tests also measure with the device-owned profile
ceilings even when remote policy schedules smaller background tests — how a
user's own test measures is a device decision. An operator's one-off keeps
the controller-configured ceilings, so remote policy still bounds what a
console click can spend. Either may knowingly spend past the remaining daily
or monthly ledger; the UI warns about the overage but never blocks, and the
overdraw is still charged durably, so scheduled tests stay deferred until
the ledger rolls over. Pulse durably reserves the complete effective allowance before a test
starts and refunds unused bytes after a clean return. A crash or power loss
therefore remains conservatively charged rather than bypassing the cap.
Metered and battery opt-ins are device-owned; remote policy cannot relax
them.

Each profile may use a bounded 1-hour to 24-hour cadence inside one or more
allowed local-time windows; the complement acts as quiet hours. The randomized
instant is stable for a sensor and slot, and `never` disables scheduling.
Authenticated controller one-offs carry an issue time, unique nonce, expiry of
at most ten minutes, and an HMAC bound to the current sensor enrollment.
Nonces are consumed durably before traffic starts and survive restart. A
one-off waits only for the sensor to be free of another test, update, or
upload; it skips cadence spacing and the failure backoff, and its signed
expiry bounds how long it may wait. The sensor reports the verdict once, after
the run: `controller_speed_test_accepted` naming the `measurement_id` it
produced, so the controller can join the directive to its result, or
`controller_speed_test_rejected` with the concrete reason (an invalid or
replayed directive, a disabled profile, a reserve failure, or
`endpoint_or_network_failure`), so the console never shows an acceptance
followed by silence. Saturation
and capacity tests fail closed when the operating system cannot report
metering or power state. Endpoint/network failures and missing required
phase evidence back off exponentially to one hour.

### Controller contract

Household profiles, the household mix and the household result block belong
to sensor contract version 3. A controller that still speaks contract 2 never
receives household profiles from the sensor's normalization, so it is never
sent a result it would reject; the sensor keeps running the legacy content
and capacity profiles it is given. Deploy the controller before the sensors.

## Household mix

The mix is saved per sensor: counts of 4K, Full HD and HD streams, video
calls and games (0–4 each), plus background browsing, for at most eight
concurrent instances. A local override set in Pulse wins over the controller's
configuration, which wins over the catalog default; every result records
which one applied, a mix revision string and the catalog and scorer versions,
so history never compares runs of different scenarios without saying so.

### Activity catalog v1

| Activity | Reference demand | Pattern | Delivery requirement |
|---|---|---|---|
| Stream HD / FHD / 4K | 3 / 5 / 15 Mbps down | 2 s segments, the next fetched when the previous is due or done | Segments arrive before the next is due; startup time |
| Video call | 3.0 Mbps down, 3.8 Mbps up | 100 ms media frames both ways, at most three in flight per direction | Frame delivery delay p95, jitter, lost or dropped frames |
| Online game | ~0.1 Mbps | 50 ms tiny requests on a persistent connection | Round-trip p95, jitter, loss |
| Browsing | ~0.75 Mbps | Every 3 s: one object on a fresh connection, then five warmed objects | Response start p95; connection setup time |

Reference demands come from published service guidance (Netflix recommended
speeds; Zoom's 1080p example; Microsoft Teams network planning), checked on
9 September 2026. They describe generic activities; Pulse does not contact
those services and makes no claim about routes to them. The game activity is
measured as HTTPS request timing under the household load and is labelled an
estimate: it is not UDP media quality.

Every instance has its own pacer and its own samples. Activity traffic runs
over HTTP/1.1 so each in-flight unit rides its own TCP connection, the way
separate household devices do; HTTP/2 would multiplex the whole mix onto one
connection and let a 4K segment head-of-line block the call. The latency
probes keep the same dedicated persistent client as every other profile. The
activity pool is bounded at 32 connections regardless of the mix.

### Timeline, ledger and window

1. **Baseline:** idle latency, exactly as for the other profiles.
2. **Reference:** the highest-demand stream alone for three segments after
   startup, so the mix result can say whether the activity works at all
   before it says whether it coexists. Dropped when it does not fit the byte
   cap or the time available.
3. **Household:** a 2 s warm-up, then a 12 s observation window with every
   instance active. Same-kind streams start staggered across their segment
   interval, so two 4K streams never burst at the same instant the way
   synchronized test instances would but real households never do. Samples outside the window are not scored. Latency probes
   at 100 ms run on the dedicated persistent client, giving bufferbloat under
   household load rather than under saturation.
4. **Cooldown:** recovery latency; diagnostic only.

One byte ledger covers the reference, the mix and the probes. Every delivery
unit claims its payload before it starts; a unit that cannot be funded is
refused whole, never shrunk, so an activity never quietly runs below its
demand. Ten percent of the cap is held back for protocol overhead the payload
ledger cannot see. Before the run, planned bytes are computed from the mix
demand and the window: the reference is dropped first, then the window
shrinks one second at a time down to 8 s. The requested mix is never
reduced; if even the shortest window does not fit, the scenario still runs
and the result is `insufficient`.

Stop reasons are explicit: `enough_evidence`, `insufficient_bytes`,
`insufficient_time`, `transport_failure` or `cancelled`. A budget- or
time-limited run is a diagnostic with its reason, never a bad score.

### Scoring

Each activity is scored from its own delivery requirements, using the
published thresholds in `quality/household.go`. Excess throughput earns no
credit once demand is met: a gigabit line and a 20 Mbps line score a 4K
stream identically when both deliver every segment. A stream's achieved rate
is its delivered bytes over the time its segments occupied (each at least its
interval), so a partial window never under-reports an on-time stream.

- **Streams:** late-segment share, achieved rate against demand, startup
  time, fetch errors.
- **Calls and games:** typical delivery delay (p90), robust jitter (median
  absolute deviation) and the share of units that missed their deadline. The
  p95 is recorded for diagnosis but does not decide the score: a sparse tail
  of spikes that a jitter buffer absorbs must not condemn a call, while a
  frame later than the deadline is what a receiver actually drops. In-flight
  limits cover a full deadline so queueing shows up as lateness, never as
  synthetic loss.
- **Browsing:** warmed response start p90, cold connection setup p95, errors.

Ratings use the same bands as before: **Good** 80–100, **Average** 60–79,
**Bad** 0–59. When the idle baseline already shows spikes (p95 more than
100 ms above the median, a Wi-Fi symptom), the summary says so rather than
letting a struggling call imply that the mix caused them. An activity with fewer samples than its minimum is
`unavailable`, never scored.

The household status is decided by the weakest requested activity: `pass`,
`struggle`, `fail`, or `insufficient` when the run stopped early, the window
was too short or any activity could not be scored. Several good streams
cannot average away one unusable call. The everyday cards (Browsing,
Streaming, Video calls, Gaming) are filled from the household activities,
streaming from the weakest configured stream; an activity that is not in the
mix is shown as not requested. The overall score is the weakest activity's
score, not an average.

Only household results that stopped with enough evidence, produced a scorable
window and usable confidence enter the representative home history.
Saturation, capacity, cross-traffic-contaminated, budget-capped and
low-confidence results keep an explicit exclusion reason and never replace a
representative everyday result.

## Saturation phase model

1. **Baseline:** latency without generated bulk traffic. The initial request
   warms the dedicated probe connection and is retained as fresh-path evidence,
   including failures, rather than scored as persistent idle latency. Then 20
   requests are attempted on the persistent client at a 100 ms cadence, within
   the existing test time and response-byte caps. Reconnection delays after
   warm-up remain part of the observed latency.
2. **Download warm-up:** endpoint, block, and stream calibration; never scored.
3. **Download:** throughput plus latency under downstream load.
4. **Upload warm-up:** separate upstream calibration; never scored.
5. **Upload:** throughput plus latency under upstream load.
6. **Bidirectional:** simultaneous down/up load for diagnostic responsiveness.
   It does not alter the standard bufferbloat headline.
7. **Cooldown:** recovery latency after load; diagnostic only.

Time shares are 30% of the maximum for each one-way phase and 25% for the
bidirectional phase, leaving 15% for baseline, warm-ups and cooldown. Bytes
are reserved for the bidirectional phase independently per direction, in
proportion to its share of the allocated window and never below a quarter, so
an asymmetric uplink cannot run dry before the common observation interval
ends. With the 60 s saturation default the bidirectional window is 15 s,
long enough for three complete stability buckets after ramp-up.

Calibration, ramp-up, stream-count changes, scored steady state, and recovery
are distinct intervals. Each scored transfer emits one-second buckets with its
duration, bytes, sample count, and stream count. Incomplete buckets do not
qualify for stability.

## Stable region

Pulse needs at least three consecutive complete buckets with one unchanged
stream count. It compares bucket throughput using both coefficient of variation
and median absolute deviation divided by the median. Defaults are CV ≤ 0.10 and
MAD ratio ≤ 0.08.

The first qualifying window starts the stable region. Compatible later buckets
extend it. A phase may end after its minimum window once stable; otherwise it
continues toward the local byte or maximum-duration cap. Throughput is taken
from the stable-region median. If no region qualifies, Pulse reports the
observed value with `throughput_did_not_stabilize` and lowers confidence.

The bidirectional phase is judged per direction. Each direction must be stable
on its own and the two stable regions must overlap for at least three
buckets; a stable sum of the two directions is not evidence, because opposing
changes cancel out and one direction can collapse unnoticed. The phase
reports `download_not_stable`, `upload_not_stable`,
`neither_direction_stable`, `directions_not_overlapping`, or
`insufficient_complete_buckets` when one direction ran out of bytes or time.
The desktop app shows a phase without a stable region as unavailable, with
the observed transfer rate labelled separately; it never shows zero.

Rate-adaptive allocation means bytes normally never end a phase before its
time caps do. When they still do — a link faster than the profile ceiling can
sustain, or a drained daily/monthly ledger shrinking the effective envelope —
the result carries a `<phase>_budget_limited` confidence reason so consumers
can attribute the missing evidence to the budget rather than to the network.

## Responsiveness and bufferbloat

For each loaded direction of a saturation test:

`bloat = max(0, loaded p90 RTT − baseline p5 RTT)`

| Increase | Grade |
|---:|:---:|
| < 5 ms | A+ |
| < 30 ms | A |
| < 60 ms | B |
| < 200 ms | C |
| < 400 ms | D |
| ≥ 400 ms | F |

The saturation headline is the worse of download and upload. Bidirectional
load and cooldown remain visible diagnostics but do not change it. A
household result grades the same delta under its own mix load instead, and
says so: it describes responsiveness while the household is busy, not at the
line's limit. Neither grade is used on its own to say a single user will have
a poor experience.

## Everyday outcomes for legacy profiles

Content results still combine phase-appropriate p95 latency, loss, and
representative throughput using the policy in `quality/quality.go`; nerd mode
additionally exposes Audio calls and Backup, component scores, raw phases,
and reasons. Missing latency, loss, or throughput is never interpreted as
zero. An application is unavailable when its required phase or throughput
evidence is missing, when its loaded phase produced fewer than five latency
samples, or (for browsing) when fewer than 20 idle samples succeeded. Legacy
results remain in history with legacy labels and are never rescored as if
they contained household measurements.

## Persistent and fresh paths

Results reserve a measurement identifier before asynchronous phase work. Path
evidence has separate slots for:

- **persistent path:** latency on an already-established probe path while bulk
  connections are active;
- **fresh path:** DNS/connect/TLS/HTTP setup for a newly established request.

Missing path evidence is unavailable, never zero. The current native transport
lowers confidence for too few successful probes, unreliable fresh paths,
cross-traffic contamination, and unstable transfer phases. Household runs add
`household_window_too_short`, `household_window_shortened`,
`household_budget_limited`, `household_time_limited` and
`household_activities_unscored`. The policy core also accepts
endpoint/source disagreement, CPU pressure, and network or IP-family changes
when a transport supplies those signals; they are not claimed as observed by
the current single-endpoint native run. Confidence warnings do not
automatically lower connection quality.

## Background validity

Pulse runs the engine in the native Go service, outside a throttled browser
tab. Closing the desktop webview does not stop measurement; the tray and
headless service continue. The desktop viewer only renders bounded IPC data and
can be destroyed while the service remains active.
