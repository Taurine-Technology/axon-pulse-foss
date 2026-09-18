// Household QoE helpers shared by app.js and the node tests. Pure functions
// only: no DOM, no IPC. Demand figures mirror the activity catalog in
// quality/household.go and must change with it.

export const PROFILE_LABELS = {household: "Household mix", saturation: "Saturation", content: "Everyday (legacy)", capacity: "Capacity (legacy)"};
export function profileLabel(name) { return PROFILE_LABELS[name] || String(name || "—"); }
export function defaultProfile(names) { return names.includes("household") ? "household" : (names[0] || ""); }
export function isSaturationProfile(name) { return name === "saturation" || name === "capacity"; }

// Reference demand per activity instance in Mbps.
export const MIX_FIELDS = [
  {key: "uhd_streams", label: "4K streams", noun: "4K stream", down: 15, up: 0},
  {key: "fhd_streams", label: "Full HD streams", noun: "Full HD stream", down: 5, up: 0},
  {key: "hd_streams", label: "HD streams", noun: "HD stream", down: 3, up: 0},
  {key: "video_calls", label: "Video calls", noun: "video call", down: 3, up: 3.8},
  {key: "gaming", label: "Games", noun: "game", down: 0.1, up: 0.05},
];
export const BROWSING_DEMAND = {down: 0.75, up: 0};
export const MAX_COUNT = 4;
export const MAX_INSTANCES = 8;
// Decimal megabytes, matching the sensor's 300 MB per-test allowance.
export const HOUSEHOLD_CAP_BYTES = 300_000_000;
const SCENARIO_SECONDS = 14;
const OVERHEAD = 1.1;

export function normalizeMix(mix) {
  const out = {};
  for (const field of MIX_FIELDS) out[field.key] = Math.trunc(Number(mix?.[field.key]) || 0);
  out.browsing = Boolean(mix?.browsing);
  return out;
}

export function mixInstances(mix) {
  const m = normalizeMix(mix);
  return MIX_FIELDS.reduce((sum, field) => sum + m[field.key], 0) + (m.browsing ? 1 : 0);
}

export function mixDemand(mix) {
  const m = normalizeMix(mix);
  let down = 0, up = 0;
  for (const field of MIX_FIELDS) { down += m[field.key] * field.down; up += m[field.key] * field.up; }
  if (m.browsing) { down += BROWSING_DEMAND.down; up += BROWSING_DEMAND.up; }
  return {down, up, total: down + up};
}

// mixEstimateMB is the payload a scenario of the given mix moves in a typical
// run (decimal MB), before the sensor's cap. Real runs stop earlier when the
// evidence is conclusive, so this is an upper-ish estimate, not a promise.
export function mixEstimateMB(mix) { return mixDemand(mix).total * SCENARIO_SECONDS / 8 * OVERHEAD; }

export function validateMix(mix) {
  for (const field of MIX_FIELDS) {
    const value = mix?.[field.key] === undefined ? 0 : Number(mix[field.key]);
    if (!Number.isInteger(value) || value < 0 || value > MAX_COUNT) return {ok: false, message: `${field.label} must be a whole number from 0 to ${MAX_COUNT}.`};
  }
  const total = mixInstances(mix);
  if (total < 1) return {ok: false, message: "Choose at least one activity."};
  if (total > MAX_INSTANCES) return {ok: false, message: `At most ${MAX_INSTANCES} activities at once (you have ${total}).`};
  return {ok: true, message: ""};
}

export function describeMix(mix) {
  const m = normalizeMix(mix);
  const parts = [];
  for (const field of MIX_FIELDS) {
    const n = m[field.key];
    if (!n) continue;
    parts.push(field.key.endsWith("_streams") ? `${n} × ${field.noun}` : `${n} ${field.noun}${n > 1 ? "s" : ""}`);
  }
  if (m.browsing) parts.push("browsing");
  return parts.join(" · ") || "no activities";
}

export function mixSourceLabel(source) {
  return {default: "default mix", controller: "set by your controller", local: "local override"}[source] || "";
}

export function bytesToMB(bytes) { return Number(bytes || 0) / 1e6; }

// Phase evidence helpers. A phase's duration is its own window, never the
// offset of its end from the start of the whole test.
export function phaseDurationSeconds(phase) {
  const start = Number(phase?.start_ms), end = Number(phase?.end_ms);
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return NaN;
  return (end - start) / 1000;
}

// observedRateMbps is the plain bytes/time rate over a phase's buckets, used
// when no stable region exists. Returns null without usable buckets.
export function observedRateMbps(phase) {
  const buckets = Array.isArray(phase?.buckets) ? phase.buckets : [];
  let bytes = 0, ms = 0;
  for (const bucket of buckets) { bytes += Number(bucket.bytes) || 0; ms += Number(bucket.duration_ms) || 0; }
  if (ms <= 0) return null;
  return bytes * 8 / ms / 1000;
}

export function phaseRateText(phase) {
  const region = phase?.stable_region || {};
  if (region.stable) return `${Number(region.throughput_mbps || 0).toFixed(1)} Mbps`;
  const observed = observedRateMbps(phase);
  return observed === null ? "—" : `observed ${observed.toFixed(1)} Mbps`;
}

export const ACTIVITY_REASONS = {
  segments_late: "late segments", throughput_below_demand: "below demand", slow_startup: "slow start",
  delivery_delay: "delayed delivery", jitter: "jitter", lost_or_late_frames: "lost/late frames",
  slow_response_start: "slow responses", slow_connection_setup: "slow setup", request_errors: "errors",
  insufficient_samples: "too few samples", no_samples: "no samples",
};
export function humanizeActivityReason(reason) { return ACTIVITY_REASONS[reason] || String(reason || "").replaceAll("_", " "); }

export const STOP_REASONS = {
  enough_evidence: "enough evidence", insufficient_bytes: "data allowance ran out", insufficient_time: "ran out of time",
  transport_failure: "transport failure", cancelled: "cancelled",
};
export function humanizeStopReason(reason) { return STOP_REASONS[reason] || String(reason || "").replaceAll("_", " "); }

// householdStatusClass maps the household verdict onto the rating classes the
// stylesheet already colours; "insufficient" stays muted on purpose.
export function householdStatusClass(status) {
  return {pass: "good", struggle: "average", fail: "bad"}[status] || "muted";
}
