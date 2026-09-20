// ios-smoke.test.ts — REAL simulator smoke for IOSDriver (plan 04-01 T6).
//
// Unlike driver.test.ts (spawn-stub unit tests), this test exercises the
// actual seam on a real iPhone simulator: boots an existing iPhone 17 sim
// (E2AI_IOS_UDID override; runtime preference iOS 26.5), drives Safari via
// `simctl openurl` through the SurfaceDriver seam, verifies the deterministic
// status_bar override the driver issues at launch, captures a real PNG
// screenshot, and shuts the simulator down.
//
// Gate: runs whenever `xcrun simctl` is available and an iPhone sim exists;
// otherwise skips cleanly. E2AI_IOS_SMOKE=1 forces an attempt (missing
// tooling or device fails the test instead of skipping).
//
// NOTE: booting a cold simulator takes ~1–2 minutes the first time; the test
// timeout reflects that. This test runs under `npm test` because the
// tooling gate holds on dev machines with Xcode installed.

import test from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import { artifactPaths } from "../loop.js";
import type { CaptureContext } from "../surface.js";
import { IOSDriver, parseUdid } from "./simctl-ios.js";
import { systemExec, type ToolExec } from "./exec.js";

const run = promisify(execFile);

interface SimDevice {
  name: string;
  udid: string;
  runtime: string;
}

/** Parse `xcrun simctl list devices available` (runtime headers + device
 * lines). Device-name extraction tolerates names with spaces/parentheses. */
async function listAvailableSims(): Promise<SimDevice[]> {
  const { stdout } = await run("xcrun", ["simctl", "list", "devices", "available"], {
    timeout: 30_000,
  });
  const devices: SimDevice[] = [];
  let runtime = "";
  for (const raw of stdout.split("\n")) {
    const header = raw.trim().match(/^-- (.+) --$/);
    if (header) {
      runtime = header[1]!;
      continue;
    }
    // e.g. "    iPhone 17 (BD7A…-…) (Shutdown) "
    const m = raw.trim().match(/^(.+?) \(([0-9A-Fa-f-]{25,})\) \(\w+\)$/);
    if (m) devices.push({ name: m[1]!, udid: m[2]!, runtime });
  }
  return devices;
}

/** Device pick: E2AI_IOS_UDID pin, else best iPhone sim — exact "iPhone 17"
 * first, then iPhone 17 family, runtime "iOS 26.5" preferred. */
async function pickSim(): Promise<SimDevice | undefined> {
  const pinned = process.env.E2AI_IOS_UDID?.trim();
  if (pinned) return { name: "E2AI_IOS_UDID pin", udid: pinned, runtime: "" };
  const sims = await listAvailableSims().catch(() => [] as SimDevice[]);
  const iphones = sims.filter((s) => s.name.startsWith("iPhone"));
  const score = (s: SimDevice): number =>
    (s.name === "iPhone 17" ? 2 : s.name.startsWith("iPhone 17") ? 1 : 0) * 10 +
    (s.runtime.startsWith("iOS 26.5") ? 2 : s.runtime.startsWith("iOS 26") ? 1 : 0);
  iphones.sort((a, b) => score(b) - score(a));
  return iphones[0];
}

/** Boot the sim (tolerating "already booted") and wait for full OS start. */
async function bootSim(udid: string): Promise<void> {
  try {
    await run("xcrun", ["simctl", "boot", udid], { timeout: 120_000 });
  } catch (err) {
    // execFile failures carry the child's stderr on the error object.
    const msg =
      typeof err === "object" && err !== null && "stderr" in err
        ? String(err.stderr)
        : err instanceof Error
          ? err.message
          : String(err);
    if (!/(already booted|current state: booted)/i.test(msg)) throw err;
  }
  // -b: block until the OS is fully reachable (fast no-op when already up).
  await run("xcrun", ["simctl", "bootstatus", udid, "-b"], { timeout: 300_000 });
}

test("ios smoke: boots iPhone 17 sim, drives Safari via seam, captures real PNG, shuts down", { timeout: 540_000 }, async (t) => {
  const forced = process.env.E2AI_IOS_SMOKE === "1";
  const sim = await pickSim().catch(() => undefined);

  if (!sim) {
    if (forced) throw new Error("E2AI_IOS_SMOKE=1 but no iPhone simulator was found");
    t.skip("no iPhone simulator available — ios smoke skipped");
    return;
  }

  const udid = sim.udid;
  const artifactsDir = await mkdtemp(join(tmpdir(), "e2ai-ios-smoke-"));
  const paths = artifactPaths(artifactsDir, "smoke-run", "ios-smoke");
  const ctx: CaptureContext = { paths, idx: 1 };

  // Recording wrapper around the real exec seam so the smoke can assert the
  // driver actually issued its launch-time simctl calls on this device.
  const calls: Array<{ tool: string; args: string[] }> = [];
  const real = systemExec();
  const exec: ToolExec = {
    run: async (tool, args, opts) => {
      calls.push({ tool, args });
      return real.run(tool, args, opts);
    },
    spawnLong: async (tool, args) => {
      calls.push({ tool, args });
      return real.spawnLong(tool, args);
    },
  };

  const driver = new IOSDriver({
    env: { ...process.env, E2AI_IOS_UDID: udid },
    exec,
    udid,
  });

  await bootSim(udid);
  t.diagnostic(`booted sim ${sim.name} (${udid}, ${sim.runtime || "unknown runtime"})`);

  try {
    await driver.launch({ env: { E2AI_IOS_UDID: udid } });

    // The launch-time deterministic status_bar override must have gone to the
    // real device through the seam.
    // `simctl status_bar <udid> override ...` — subcommand first, then device.
    const barCall = calls.find(
      (c) =>
        c.tool === "xcrun" &&
        c.args[0] === "simctl" &&
        c.args[1] === "status_bar" &&
        c.args[2] === udid &&
        c.args[3] === "override",
    );
    assert.ok(barCall, "driver.launch must issue `simctl status_bar <udid> override`");

    // And the device must accept the same override directly (exit 0).
    await run(
      "xcrun",
      ["simctl", "status_bar", udid, "override", "--time", "9:41", "--batteryState", "charged", "--batteryLevel", "100"],
      { timeout: 30_000 },
    );

    // Act: navigate Safari through the seam (simctl openurl under the hood).
    const nav = await driver.act({ kind: "navigate", url: "https://example.com" });
    assert.deepEqual(nav.action, { navigated: "https://example.com" });
    // Settle wait — integration exception to the fake-timers rule: Safari
    // page render is real device-tooling latency with no observable loading
    // predicate exposed by simctl, so a bounded wall-clock wait is the only
    // available signal before observe/capture.
    await new Promise((r) => setTimeout(r, 8_000));

    // Observe: a11y grounding (simctl listapps) must return real content.
    // The payload is typed unknown (untrusted; the loop fences it) — narrow
    // structurally here.
    const obs = await driver.observe();
    const a11y = obs.a11y;
    assert.equal(
      typeof a11y === "object" && a11y !== null && "format" in a11y && a11y.format,
      "simctl-listapps",
      "observation must carry the listapps format tag",
    );
    assert.ok(
      typeof a11y === "object" &&
        a11y !== null &&
        "content" in a11y &&
        typeof a11y.content === "string" &&
        a11y.content.trim().length > 0,
      "listapps must return content",
    );

    // Capture: real PNG bytes on disk behind a run-relative ref.
    const shots = await driver.capture("screenshot", ctx);
    assert.equal(shots.length, 1);
    assert.equal(shots[0]!.kind, "screenshot");
    assert.equal(shots[0]!.path, join("agents", "ios-smoke", "screenshots", "step-1.png"));
    const png = await readFile(join(paths.screenshotsDir, "step-1.png"));
    assert.ok(
      png.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])),
      "captured file must be a real PNG",
    );
    assert.ok(png.byteLength > 10_000, "screenshot must not be a stub");

    t.diagnostic(`smoke artifacts in ${artifactsDir}`);
  } finally {
    await driver.teardown().catch(() => {});
    // Booted-list regression check through the driver's own parser: the
    // booted list must name our sim (guards the real-output parsing shape).
    const { stdout: bootedList } = await run("xcrun", ["simctl", "list", "devices", "booted"], {
      timeout: 30_000,
    });
    const bootedLine = bootedList
      .split("\n")
      .find((l) => l.includes("(Booted)") && l.includes(udid));
    assert.ok(bootedLine, "sim must be on the booted list");
    assert.equal(parseUdid(bootedLine.trim()), udid, "booted line must parse to the sim UDID");
    // Required shutdown: the smoke owns its device lifecycle end to end.
    await run("xcrun", ["simctl", "shutdown", udid], { timeout: 60_000 });
    t.diagnostic("sim shut down");
  }
});
