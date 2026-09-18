const preferredInternetTargets = ["controller", "anchor:1.1.1.1", "anchor:8.8.8.8"];

function timestampMS(value) {
  if (typeof value === "number") return value < 1e12 ? value * 1000 : value;
  return Date.parse(value);
}

function rttMS(sample) {
  const value = sample?.rtt_ms?.p50 ?? sample?.rtt_ms;
  return value == null ? NaN : Number(value);
}

export function selectInternetTarget(source) {
  const rows = Array.isArray(source) ? source : [];
  for (const target of preferredInternetTargets) {
    if (rows.some((sample) => sample?.target === target)) return target;
  }
  return [...rows].reverse().find((sample) => sample?.target && sample.target !== "gateway")?.target || "";
}

export function targetSamples(source, target, windowMS=Infinity) {
  if (!target) return [];
  const rows = (Array.isArray(source) ? source : [])
    .filter((sample) => sample?.target === target)
    .map((sample) => ({sample, timestamp: timestampMS(sample.timestamp)}))
    .filter((row) => Number.isFinite(row.timestamp))
    .sort((a, b) => a.timestamp - b.timestamp);
  if (!rows.length || !Number.isFinite(windowMS)) return rows.map((row) => row.sample);
  const cutoff = rows[rows.length-1].timestamp - Math.max(0, windowMS);
  return rows.filter((row) => row.timestamp >= cutoff).map((row) => row.sample);
}

export function rollingStats(source, target, windowMS=60_000) {
  const rows = targetSamples(source, target, windowMS);
  if (!rows.length) return null;
  const successful = rows.filter((sample) => sample.success === true && Number.isFinite(rttMS(sample)));
  const chronologicalRTTs = successful.map(rttMS);
  const sortedRTTs = [...chronologicalRTTs].sort((a, b) => a - b);
  let jitter;
  if (chronologicalRTTs.length > 1) {
    jitter = 0;
    for (let index=1; index<chronologicalRTTs.length; index++) jitter += Math.abs(chronologicalRTTs[index] - chronologicalRTTs[index-1]);
    jitter /= chronologicalRTTs.length - 1;
  }
  const p50 = sortedRTTs.length ? sortedRTTs[Math.ceil(sortedRTTs.length * 0.5) - 1] : undefined;
  return {
    target,
    rtt_ms: {p50},
    jitter_ms: jitter,
    loss_pct: 100 * (rows.length - successful.length) / rows.length,
    samples: rows.length,
  };
}
