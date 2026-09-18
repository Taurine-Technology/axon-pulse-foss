# Household QoE and saturation diagnostics plan

Status: revised implementation plan, 9 September 2026. Supersedes the
proposal of the same date. This revision was checked against the Pulse
`v0.0.7-alpha.2` and `v0.0.7-alpha.3` sources and the matching controller,
console, and agent development branches. Ships as Pulse `0.0.7-alpha.4`
together with matching controller and console changes.

## Product decisions

- Pulse should reflect what people in the household actually feel. The
  scheduled default test is therefore a **paced household mix**, run four
  times a day, not a saturation test. A line that is only saturated now and
  then must not be graded as if it were always saturated.
- Saturation data is still gathered, but separately and less often: a
  **saturation** profile with much higher limits runs once a day by default
  and can be run by hand at any time. Its results stay in their own
  diagnostic card and never enter the household history.
- The household mix is configurable per sensor, with a simple editor in Pulse
  and in the console. Default mix: 2 × 4K streams, 1 video call, 1 online
  game and background browsing.
- Every household test is capped at **300 MB (300,000,000 bytes) download plus
  upload**. It usually spends far less because activities are paced to their
  demand, not to link capacity. The estimate and the actual spend are shown.
- Activity profiles describe generic activities. Published service
  requirements are reference inputs, not claims that Netflix, Zoom or Teams
  were measured.
- Agents describe network paths and egress health. An aggregation-point probe
  never receives a household score.

## Corrections to the original proposal

These were verified in code and change the design.

1. **The bidirectional under-provisioning is structural, not just a fast-link
   ceiling artefact.** The runner sizes each direction for the one-way phase
   plus the bidirectional window (12 s at 1.5 × the warm-up rate) and then
   hands one fifth of that to a phase that is one third of the window. Even
   below the byte ceiling, the bidirectional phase gets about 3.6 s of bytes
   for a 4 s window before ramp-up. The fix is a proportional split with an
   independent reserve per direction, not a larger ceiling alone.
2. **Time was already over-committed.** Two one-way phases at 40 % each and a
   bidirectional phase at 20 % consume the whole maximum before baseline,
   warm-ups and cooldown run. The one-way phases now get 30 % each and the
   bidirectional phase 25 %, leaving 15 % for the rest. The saturation profile
   runs for up to 60 s so the bidirectional window (15 s) can hold three
   complete stability buckets after ramp-up. The legacy 20 s content profile
   remains supported but is no longer scheduled by default.
3. **Per-direction stability partly existed.** Each bidirectional direction
   already computed its own stable region for its throughput number. Only the
   early-stop criterion and the phase evidence used the summed buckets. The
   stop criterion and the evidence now require both directions to be stable
   over overlapping buckets; a stable sum no longer hides one direction
   collapsing.
4. **Budget arithmetic conflicted with existing defaults.** The old content
   profile was already 352 MiB, so 300 MB is a reduction. The sensor and
   controller daily/monthly ledgers (1024 MB / 20 GB on the sensor, 150 MB /
   2 GB on the controller) would have trimmed four household tests plus a
   saturation test. Defaults are now 4096 MB per day and 61,440 MB per month on
   both sides; the ledger remains the hard bound and still trims tests when
   exhausted.
5. **Six-hour cadence and byte accounting already existed.** `cadence_minutes`,
   `predicted_bytes` and `used_bytes` are in place; the work is presentation
   and the household-specific ledger, not scheduling.
6. **The endpoint principle contradicted the runner.** The default endpoint is
   Cloudflare's speed-test backend and Pulse has no UDP code. This release keeps
   HTTPS against the existing endpoint for every household activity. Gaming
   is therefore measured as HTTPS request timing under household load and is
   labelled an estimate, not UDP media quality. A first-party endpoint with a
   datagram echo path is a later phase, not a prerequisite.
7. **A 10–15 s common observation window does not fit inside the 20 s
   content test.** The household mix is its own profile with its own timeline
   (baseline, optional single-activity reference, warm-up, observation,
   cooldown) and a 30 s maximum. Its byte cost is small: the default mix is
   about 38 Mbps of demand, roughly 70 MB for a 14 s scenario.
8. **Presentation bugs confirmed.** The desktop phase table showed
   `end_ms / 1000` as a duration and printed `0.0 Mbps` for phases without a
   stable region. Both are fixed; a missing stable region is shown as
   unavailable with the observed transfer rate labelled separately.
9. **Controller ingest is strict.** Unknown top-level keys in a speed test
   reject the whole record. The household block therefore requires the
   controller to be deployed before sensors are upgraded, and the sensor only
   offers the household profile when its effective configuration contains one
   (standalone mode always does). A new sensor on an old controller keeps
   running the legacy profiles it is given.

## Test model

| Mode | Purpose | Generated load | Primary output |
| --- | --- | --- | --- |
| Live health | What is happening now? | Existing continuous reachability/latency/DNS probes | Timestamped status, latency/jitter/loss, freshness |
| Household reference | Can one activity work well alone? | One highest-tier stream from the mix, alone, for three segments | Single-activity result the mix is compared against |
| Household mix | Can the configured activities coexist? | Concurrent activity instances, each with its own pacer | Household status, per-activity results and reasons |
| Saturation | What happens near the path's limit? | Explicit saturation in each direction, then both together | Throughput, loaded latency, per-direction stability, confidence |

Household results carry the scenario catalog version, scorer version, the
exact mix, the mix source (default, controller or local override), observed
window, byte ledger and stop reason. History may only compare results with
the same scenario and scorer versions; a change from one streamer to four is
a configuration change, not a network regression.

### Activity catalog v1

Reference demands and their sources (checked 9 September 2026):

| Activity | Demand | Pattern | Delivery requirement | Source |
| --- | --- | --- | --- | --- |
| Stream HD / FHD / 4K | 3 / 5 / 15 Mbps down | 2 s segments fetched back to back | Segment must arrive before the next is due; startup time | Netflix recommended speeds, https://help.netflix.com/en/node/306 |
| Video call | 3.0 Mbps down, 3.8 Mbps up | 100 ms media frames in both directions | Frame delivery delay p95, loss, jitter | Zoom 1080p example, https://support.zoom.com/hc/en/article?id=zm_kb&sysparm_article=KB0058323; Microsoft Teams network planning, https://learn.microsoft.com/en-us/microsoftteams/prepare-network |
| Online game | ~0.1 Mbps | 50 ms tiny requests on a persistent connection | Round-trip p95, jitter, loss (HTTPS estimate) | Synthetic; calibration pending |
| Browsing | ~1 Mbps average | Every 3 s: one cold-connection object plus five warmed objects | Response start p95, cold setup time | Synthetic |

Every activity instance has its own pacer and its own measurements. A stream
is not expected to have flat one-second throughput; saturation stability
thresholds are never applied to paced activities.

### Household budget and window rules

1. One shared byte ledger covers the reference, the mix, latency probes and
   retries. Every request claims bytes before it starts; when the ledger is
   exhausted all workers stop and the stop reason is `insufficient_bytes`.
   Counted bytes are HTTP payload bytes. A 10 % reserve inside the 300 MB cap
   covers protocol overhead; the cap is not claimed as an exact on-wire limit.
2. Before the test, planned bytes are computed from the mix demand and the
   window. If the reference plus mix does not fit, the reference is dropped
   first, then the observation window shrinks to no less than 8 s. The
   requested mix is never silently reduced; if the window cannot fit, the test
   runs with the shortened window and reports `insufficient` status.
3. The mix runs a 2 s warm-up followed by a 12 s observation window with all
   instances active. Samples outside the window are excluded from scoring.
4. Latency probes at 100 ms run on the dedicated persistent client during the
   mix, giving bufferbloat under household load rather than under saturation.

### Scoring and presentation

- Each activity is scored from its own delivery requirements. Excess
  throughput earns no extra credit once demand is met.
- The household status is the weakest requested activity: `pass`, `struggle`,
  `fail` or `insufficient`. Several good streams cannot average away one
  unusable call. The everyday cards (browsing, streaming, calls, gaming) are
  filled from the household activities; activities that are not in the mix
  are shown as not requested.
- Saturation grades stay in their own card. They never decide the household
  status and are never used to say a single normal user will have a poor
  experience.
- Confidence is independent of quality. Unknown evidence is neither zero nor
  good. Stop reasons are explicit: `enough_evidence`, `insufficient_bytes`,
  `insufficient_time`, `transport_failure`, `cancelled`.

## Profiles and defaults after this change

| Profile | Scheduled | Cadence | Max duration | Byte ceiling | Experience eligible |
| --- | :---: | ---: | ---: | ---: | :---: |
| household | yes | 6 h | 30 s | 300 MB total (225 MB down, 75 MB up) | yes |
| saturation | yes | 24 h | 60 s | 1 GiB down + 512 MiB up | no |
| content (legacy) | only if configured | as configured | 20 s | 256 MiB + 96 MiB | yes |
| capacity (legacy) | only if configured | as configured | 60 s | 512 MiB + 256 MiB | no |

Ledgers: 4096 MB per day, 61,440 MB per month, on sensor and controller.
Metered, battery, cooldown, spacing and one-active-test gates are unchanged.
Retries share the original reservation.

## Delivery in this release (single cross-repo change)

1. **Pulse evidence fixes.** Proportional bidirectional split with
   per-direction reserves, 30/30/25 time shares, per-direction stability in
   the bidirectional evidence and stop criterion, duration and unavailable
   throughput display fixes, regression tests for fast asymmetric links.
2. **Pulse household engine.** Activity catalog and scorer in `quality`,
   paced runner in `internal/qualitytest/household.go`, shared ledger, reference plus mix,
   result schema, profile registration in config, scheduler, IPC, CLI and
   desktop UI, mix editor with local override.
3. **Controller.** Accept the four profile names and up to four profiles,
   household mix in the sensor configuration, household block and new phase
   names in ingest validation and pruning, promoted columns unchanged, raised
   budget defaults and bounds, profile choices for controller-directed tests.
4. **Console.** Types, profile labels, profile editor defaults, household mix
   editor, household section in the speed-test detail, list badges,
   locales.
5. **Documentation.** Methodology rewritten for the new profiles.

Deployment order: controller, console, then Pulse `0.0.7-alpha.4` on the
alpha channel. Old sensors keep working: their `content` and `capacity`
results remain valid and are displayed with legacy labels.

## Deferred (not in this release)

- First-party regional measurement endpoint with a UDP echo path, and the
  gaming/call loss measurements that depend on it.
- Capacity ladder ("at least N streams tested").
- Agent network diagnostics changes. The agent's phase split has the same 80/20
  shape and needs its own deadline review; it is not required for the
  end-to-end household test and is left untouched here.
- Full calibration matrix. The alpha gate is 20/5, 100/20 and 1000/1000 Mbps
  emulation plus real macOS and Linux clients; the complete matrix from the
  original proposal remains the beta target.

## Acceptance for this release

- The household ledger holds at 300 MB with concurrent instances and probes;
  the result reports planned, used and per-direction bytes.
- A 10 Mbps path fails a 2 × 4K mix with the streams named as the cause and
  can still pass a 1 × HD mix.
- A link passes the single-stream reference but fails the mix when the call's
  upload delay grows, and the result says so.
- A fast asymmetric link keeps both directions active for the whole
  bidirectional window in the saturation profile; a missing stable region is
  reported as unavailable, never as zero throughput.
- Equal total throughput cannot hide one direction collapsing.
- Legacy and new sensors ingest safely; history never mixes scenario or
  scorer versions without disclosure.
