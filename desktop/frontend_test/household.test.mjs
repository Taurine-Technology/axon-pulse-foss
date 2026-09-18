import assert from "node:assert/strict";
import test from "node:test";

import {
  bytesToMB, defaultProfile, describeMix, householdStatusClass, humanizeActivityReason, humanizeStopReason,
  isSaturationProfile, mixDemand, mixEstimateMB, mixInstances, mixSourceLabel, normalizeMix, observedRateMbps,
  phaseDurationSeconds, phaseRateText, profileLabel, validateMix,
} from "../frontend/household.mjs";

const defaultMix = {uhd_streams: 2, fhd_streams: 0, hd_streams: 0, video_calls: 1, gaming: 1, browsing: true};

test("profile labels distinguish the paced mix from saturation and legacy runs", () => {
  assert.equal(profileLabel("household"), "Household mix");
  assert.equal(profileLabel("saturation"), "Saturation");
  assert.equal(profileLabel("content"), "Everyday (legacy)");
  assert.equal(profileLabel("capacity"), "Capacity (legacy)");
  assert.equal(profileLabel("custom"), "custom");
  assert.equal(profileLabel(""), "—");
  assert.ok(isSaturationProfile("saturation"));
  assert.ok(isSaturationProfile("capacity"));
  assert.ok(!isSaturationProfile("household"));
  assert.ok(!isSaturationProfile("content"));
});

test("household is the default pick when offered, else the first profile", () => {
  assert.equal(defaultProfile(["saturation", "household"]), "household");
  assert.equal(defaultProfile(["content", "capacity"]), "content");
  assert.equal(defaultProfile([]), "");
});

test("default mix demand matches the catalog and the ~70 MB plan estimate", () => {
  const demand = mixDemand(defaultMix);
  assert.equal(demand.down, 2 * 15 + 3 + 0.1 + 0.75);
  assert.equal(demand.up, 3.8 + 0.05);
  assert.equal(demand.total, demand.down + demand.up);
  // 37.7 Mbps × 14 s / 8 × 1.1 ≈ 72.6 MB
  assert.ok(Math.abs(mixEstimateMB(defaultMix) - 72.55) < 0.05, String(mixEstimateMB(defaultMix)));
  assert.equal(mixEstimateMB({}), 0);
});

test("instance counting includes browsing as one instance", () => {
  assert.equal(mixInstances(defaultMix), 5);
  assert.equal(mixInstances({browsing: true}), 1);
  assert.equal(mixInstances({uhd_streams: "2", browsing: false}), 2);
});

test("normalizeMix coerces strings and missing keys", () => {
  assert.deepEqual(normalizeMix({uhd_streams: "3", browsing: 1}), {uhd_streams: 3, fhd_streams: 0, hd_streams: 0, video_calls: 0, gaming: 0, browsing: true});
  assert.deepEqual(normalizeMix(undefined), {uhd_streams: 0, fhd_streams: 0, hd_streams: 0, video_calls: 0, gaming: 0, browsing: false});
});

test("validateMix enforces per-activity bounds and 1–8 total instances", () => {
  assert.deepEqual(validateMix(defaultMix), {ok: true, message: ""});
  assert.equal(validateMix({browsing: false}).ok, false);
  assert.match(validateMix({browsing: false}).message, /at least one/);
  const crowded = {uhd_streams: 4, fhd_streams: 4, hd_streams: 0, video_calls: 0, gaming: 0, browsing: true};
  assert.equal(validateMix(crowded).ok, false);
  assert.match(validateMix(crowded).message, /At most 8/);
  assert.equal(validateMix({uhd_streams: 5, browsing: true}).ok, false);
  assert.equal(validateMix({uhd_streams: 1.5, browsing: true}).ok, false);
  assert.equal(validateMix({uhd_streams: -1, browsing: true}).ok, false);
  assert.equal(validateMix({uhd_streams: NaN, browsing: true}).ok, false);
  assert.equal(validateMix({uhd_streams: 4, fhd_streams: 4, browsing: false}).ok, true);
});

test("describeMix reads like the product copy", () => {
  assert.equal(describeMix(defaultMix), "2 × 4K stream · 1 video call · 1 game · browsing");
  assert.equal(describeMix({video_calls: 2, gaming: 3}), "2 video calls · 3 games");
  assert.equal(describeMix({fhd_streams: 1, hd_streams: 1}), "1 × Full HD stream · 1 × HD stream");
  assert.equal(describeMix({}), "no activities");
});

test("mix source labels and decimal MB", () => {
  assert.equal(mixSourceLabel("default"), "default mix");
  assert.equal(mixSourceLabel("controller"), "set by your controller");
  assert.equal(mixSourceLabel("local"), "local override");
  assert.equal(mixSourceLabel(undefined), "");
  assert.equal(bytesToMB(300_000_000), 300);
  assert.equal(bytesToMB(undefined), 0);
});

test("phase duration is the phase's own window, not its end offset", () => {
  assert.equal(phaseDurationSeconds({start_ms: 12000, end_ms: 20500}), 8.5);
  assert.ok(Number.isNaN(phaseDurationSeconds({end_ms: 20500})));
  assert.ok(Number.isNaN(phaseDurationSeconds({start_ms: 5000, end_ms: 4000})));
  assert.ok(Number.isNaN(phaseDurationSeconds(undefined)));
});

test("observed rate sums bucket bytes over bucket time", () => {
  const phase = {buckets: [{bytes: 1_250_000, duration_ms: 1000}, {bytes: 1_250_000, duration_ms: 1000}]};
  assert.equal(observedRateMbps(phase), 10);
  assert.equal(observedRateMbps({buckets: []}), null);
  assert.equal(observedRateMbps({buckets: [{bytes: 10, duration_ms: 0}]}), null);
  assert.equal(observedRateMbps({}), null);
});

test("phase rate text never prints 0.0 Mbps for an unstable phase", () => {
  assert.equal(phaseRateText({stable_region: {stable: true, throughput_mbps: 87.26}}), "87.3 Mbps");
  assert.equal(phaseRateText({stable_region: {stable: false, throughput_mbps: 0, reason: "paced_scenario"}, buckets: [{bytes: 2_500_000, duration_ms: 2000}]}), "observed 10.0 Mbps");
  assert.equal(phaseRateText({stable_region: {stable: false, reason: "latency_only_phase"}}), "—");
  assert.equal(phaseRateText({}), "—");
});

test("activity and stop reasons are humanised with a readable fallback", () => {
  assert.equal(humanizeActivityReason("segments_late"), "late segments");
  assert.equal(humanizeActivityReason("throughput_below_demand"), "below demand");
  assert.equal(humanizeActivityReason("lost_or_late_frames"), "lost/late frames");
  assert.equal(humanizeActivityReason("no_samples"), "no samples");
  assert.equal(humanizeActivityReason("something_new"), "something new");
  assert.equal(humanizeStopReason("enough_evidence"), "enough evidence");
  assert.equal(humanizeStopReason("insufficient_bytes"), "data allowance ran out");
  assert.equal(humanizeStopReason("odd_reason"), "odd reason");
  assert.equal(humanizeStopReason(undefined), "");
});

test("household status maps to the existing rating classes; insufficient stays muted", () => {
  assert.equal(householdStatusClass("pass"), "good");
  assert.equal(householdStatusClass("struggle"), "average");
  assert.equal(householdStatusClass("fail"), "bad");
  assert.equal(householdStatusClass("insufficient"), "muted");
  assert.equal(householdStatusClass(undefined), "muted");
});
