import assert from "node:assert/strict";
import test from "node:test";

import {rollingStats, selectInternetTarget, targetSamples} from "../frontend/metrics.mjs";

const sample = (seconds, target, rtt, success=true) => ({
  timestamp: new Date(Date.UTC(2026, 7, 17, 12, 0, seconds)).toISOString(),
  target,
  rtt_ms: rtt,
  success,
});

test("controller is the preferred Internet target", () => {
  const rows = [sample(0, "anchor:1.1.1.1", 4), sample(0, "controller", 20), sample(0, "anchor:8.8.8.8", 18)];
  assert.equal(selectInternetTarget(rows), "controller");
});

test("rolling stats calculate p50, jitter, and loss for one target", () => {
  const rows = [
    sample(0, "controller", 10),
    sample(1, "anchor:1.1.1.1", 3),
    sample(10, "controller", 14),
    sample(20, "controller", 0, false),
    sample(30, "controller", 20),
  ];
  assert.deepEqual(rollingStats(rows, "controller"), {
    target: "controller",
    rtt_ms: {p50: 14},
    jitter_ms: 5,
    loss_pct: 25,
    samples: 4,
  });
});

test("target samples exclude other endpoints and expired values", () => {
  const rows = [
    sample(0, "controller", 100),
    sample(30, "anchor:1.1.1.1", 3),
    sample(45, "controller", 20),
    sample(59, "controller", 22),
  ];
  assert.deepEqual(targetSamples(rows, "controller", 30_000).map((row) => row.rtt_ms), [20, 22]);
});

test("a successful sample without RTT is not measured as zero latency", () => {
  const rows = [
    sample(0, "controller", 10),
    sample(10, "controller", null),
    sample(20, "controller", 14),
  ];
  const result = rollingStats(rows, "controller");
  assert.equal(result.rtt_ms.p50, 10);
  assert.equal(result.jitter_ms, 4);
  assert.equal(result.loss_pct, 100 / 3);
});
