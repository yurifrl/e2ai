// readiness.test.ts — surface-agnostic probe semantics (PLAT-04): settle
// polling, the two-consecutive-frames render fallback, the double-lag capture
// guard, and the INFRA_FLAKE class vocabulary drivers store verbatim.
// Clocks and delays are injected; no real timers, no device tooling.
import test from "node:test";
import assert from "node:assert/strict";

import {
  FAILURE_CLASS,
  ProbeRunner,
  animationSettled,
  isInfraFlake,
  networkIdle,
  renderIdle,
  summarize,
  verifyStabilityAroundCapture,
  type ProbeOptions,
} from "./readiness.js";

/** Injectable fake clock: `sleep` IS what advances time, so probes progress. */
interface FakeClock {
  now: () => number;
  sleep: (ms: number) => Promise<void>;
  readonly time: number;
}

function fakeClock(start = 0): FakeClock {
  let t = start;
  return {
    now: () => t,
    sleep: async (ms: number) => {
      t += ms;
    },
    get time() {
      return t;
    },
  };
}

interface ProbeOpts extends ProbeOptions {
  clock: FakeClock;
}

function probe(over: Partial<ProbeOptions> = {}): ProbeOpts {
  const clock = fakeClock();
  return {
    timeoutMS: 1000,
    intervalMS: 100,
    now: clock.now,
    sleep: clock.sleep,
    clock,
    ...over,
  };
}

test("animationSettled waits for two consecutive settled checks", async () => {
  // Settles on the 3rd poll: one churn poll (counter reset) then two in a row,
  // so the consecutive-requirement is observable. No real timers — the fake
  // clock only advances through the injected sleep.
  let polls = 0;
  const p = probe();
  const r = await animationSettled(() => ++polls >= 3, p);
  assert.equal(r.name, "animation");
  assert.equal(r.ok, true);
  assert.equal(r.failureClass, undefined);
  assert.match(r.evidence, /2 consecutive/);
  assert.ok(r.elapsedMS >= 0);
});

test("animationSettled fails with the probe_timeout subtype on timeout", async () => {
  const p = probe();
  const r = await animationSettled(() => false, p);
  assert.equal(r.ok, false);
  assert.equal(r.failureClass, FAILURE_CLASS.ProbeTimeout);
  assert.match(r.evidence, /not settled/);
  assert.ok(p.clock.time >= 1000);
});

test("networkIdle survives churn: alternating idle/busy never settles", async () => {
  let flips = 0;
  const p = probe();
  const r = await networkIdle(() => ++flips % 2 === 0, p);
  assert.equal(r.ok, false);
  assert.equal(r.failureClass, FAILURE_CLASS.ProbeTimeout);
});

test("networkIdle settles when the network stays idle", async () => {
  const p = probe();
  const r = await networkIdle(() => true, p);
  assert.equal(r.ok, true);
  assert.equal(r.name, "network");
  assert.match(r.evidence, /2 consecutive/);
});

test("renderIdle settles on two consecutive identical frame digests", async () => {
  let i = 0;
  const p = probe();
  // First two samples identical ("a","a") → settles on the second.
  const r = await renderIdle(() => (++i % 4 < 2 ? "a" : "b"), p);
  assert.equal(r.ok, true);
  assert.equal(r.name, "render");
  assert.match(r.evidence, /stable at/);
});

test("renderIdle is the no-debug-bridge fallback: it settles without one", async () => {
  const p = probe();
  const r = await renderIdle(() => "frame-hash", p);
  assert.equal(r.ok, true);
});

test("renderIdle fails with unstable_frames when digests keep changing", async () => {
  let i = 0;
  const p = probe();
  const r = await renderIdle(() => `frame-${++i}`, p);
  assert.equal(r.ok, false);
  assert.equal(r.failureClass, FAILURE_CLASS.UnstableFrames);
  assert.match(r.evidence, /never stabilized/);
});

test("renderIdle treats null samples as instability", async () => {
  const p = probe();
  const r = await renderIdle(() => null, p);
  assert.equal(r.ok, false);
  assert.equal(r.failureClass, FAILURE_CLASS.UnstableFrames);
});

test("verifyStabilityAroundCapture accepts identical digests on both sides", () => {
  const g = verifyStabilityAroundCapture("sha:x", "sha:x");
  assert.deepEqual(g, { ok: true, reason: "" });
});

test("verifyStabilityAroundCapture rejects a moving frame on either side", () => {
  const before = verifyStabilityAroundCapture("sha:x", "sha:y");
  assert.equal(before.ok, false);
  assert.match(before.reason, /double-lag/);
});

test("verifyStabilityAroundCapture rejects unavailable samples", () => {
  const both = verifyStabilityAroundCapture(null, null);
  assert.match(both.reason, /both sides/);
  const one = verifyStabilityAroundCapture(null, "sha:x");
  assert.match(one.reason, /before/);
});

test("ProbeRunner folds every probe into one report with total timing", async () => {
  const clock = fakeClock();
  const runner = new ProbeRunner(
    [
      async () => ({
        name: "animation",
        ok: true,
        elapsedMS: 10,
        evidence: "",
      }),
      async () => ({
        name: "network",
        ok: false,
        elapsedMS: 20,
        evidence: "busy",
        failureClass: FAILURE_CLASS.ProbeTimeout,
      }),
    ],
    { now: clock.now },
  );
  const report = await runner.run();
  assert.equal(report.ok, false);
  assert.equal(report.results.length, 2);
  assert.equal(report.elapsedMS, 0);
  assert.equal(
    summarize(report),
    "animation:ok@10ms; network:failed(infra_flake:probe_timeout)@20ms",
  );
});

test("ProbeRunner stays ok when every probe passes", async () => {
  const runner = new ProbeRunner([
    async () => ({ name: "render", ok: true, elapsedMS: 5, evidence: "" }),
  ]);
  const report = await runner.run();
  assert.equal(report.ok, true);
});

test("isInfraFlake accepts canonical and subtype-qualified classes only", () => {
  assert.equal(isInfraFlake("infra_flake"), true);
  for (const sub of [
    FAILURE_CLASS.BootTimeout,
    FAILURE_CLASS.DeviceOffline,
    FAILURE_CLASS.ScreenshotFailed,
    FAILURE_CLASS.ProbeTimeout,
    FAILURE_CLASS.UnstableFrames,
  ]) {
    assert.equal(isInfraFlake(sub), true);
  }
  assert.equal(isInfraFlake("app_bug"), false);
  assert.equal(isInfraFlake("infra"), false);
  assert.equal(isInfraFlake(""), false);
  assert.equal(isInfraFlake("infra_flaky"), false);
});
