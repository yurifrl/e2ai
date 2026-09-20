// driver.test.ts — mobile driver seams against spawn stubs (no real device
// tooling): boot gates classify failures as typed InfraFlakeError
// (device_offline / tool_missing / maestro_flow_failed / screenshot_failed),
// act renders maestro flows through the shared mapper, capture writes
// run-relative evidence. Also web seam edges that need no browser.
import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { artifactPaths, type Decision } from "../loop.js";
import { AndroidDriver } from "./maestro-android.js";
import { IOSDriver } from "./simctl-ios.js";
import { WebDriver } from "./web.js";
import {
  InfraFlakeError,
  SurfaceConfigError,
  type LaunchContext,
  type SurfaceDriver,
} from "../surface.js";
import { maestroFlowYaml, maestroFlowSteps } from "./maestro-flow.js";
import type { ToolExec, ToolRun, LongProcess } from "./exec.js";

interface Call {
  tool: string;
  args: string[];
  binary: boolean;
}

/** Type guard: an act() action carrying the rendered maestro flow. */
function hasMaestroFlow(a: unknown): a is { maestroFlow: string } {
  return (
    typeof a === "object" && a !== null && typeof (a as { maestroFlow?: unknown }).maestroFlow === "string"
  );
}

const LAUNCH_CTX: LaunchContext = { env: {} };

function okRun(stdout = ""): ToolRun {
  return { code: 0, signal: null, stdout, stderr: "", timedOut: false };
}

function failRun(code = 1, stderr = ""): ToolRun {
  return { code, signal: null, stdout: "", stderr, timedOut: false };
}

/** Scripted ToolExec: records every invocation, resolves each via handler.
 * Returns a LongProcess stub for calls the handler answers with `long`. */
function fakeExec(
  handler: (tool: string, args: string[], binary: boolean) => ToolRun | LongProcess,
): ToolExec & { calls: Call[] } {
  const calls: Call[] = [];
  return {
    calls,
    run(tool, args, opts) {
      calls.push({ tool, args, binary: opts?.binary === true });
      return Promise.resolve(handler(tool, args, opts?.binary === true) as ToolRun);
    },
    async spawnLong(tool, args) {
      calls.push({ tool, args, binary: false });
      return handler(tool, args, false) as LongProcess;
    },
  };
}

const ADB_DEVICES_OK = ["List of devices attached", "emulator-5554\tdevice", ""].join("\n");

function decision(partial: Partial<Decision> & { kind: Decision["kind"] }): Decision {
  return partial as Decision;
}

// ---------------------------------------------------------------- maestro flow

test("maestro flow: selector → tapOn id, label → tapOn text, key → pressKey", () => {
  assert.deepEqual(maestroFlowSteps(decision({ kind: "act", selector: "#login" })), [
    "- tapOn:\n    id: #login",
  ]);
  assert.deepEqual(maestroFlowSteps(decision({ kind: "act", label: "Log in" })), [
    '- tapOn: "Log in"',
  ]);
  assert.deepEqual(maestroFlowSteps(decision({ kind: "act", key: "ENTER" })), [
    "- pressKey: ENTER",
  ]);
});

test("maestro flow: selector+text composes tapOn then inputText; appId prepends config section + launchApp", () => {
  const steps = maestroFlowSteps(decision({ kind: "act", selector: "#user", text: "yuri" }));
  assert.equal(steps.length, 2);
  assert.match(steps[0]!, /tapOn:\n {4}id: #user/);
  assert.match(steps[1]!, /inputText: "yuri"/);
  const yaml = maestroFlowYaml(decision({ kind: "act", selector: "#user" }), {
    appId: "com.e2ai.app",
  });
  assert.match(yaml, /^appId: com\.e2ai\.app\n---\n- launchApp:\n {4}appId: com\.e2ai\.app\n {4}clearState: false\n/);
});

test("maestro flow: unmappable and non-act decisions are rejected (app-level error)", () => {
  assert.throws(() => maestroFlowSteps(decision({ kind: "act" })));
  assert.throws(() => maestroFlowSteps(decision({ kind: "navigate", url: "https://x" })));
});

// ------------------------------------------------------------------- android

test("AndroidDriver.launch passes when adb lists a device in 'device' state", async () => {
  const exec = fakeExec((tool) => (tool === "adb" ? okRun(ADB_DEVICES_OK) : failRun()));
  const driver = new AndroidDriver({ env: {}, exec, adbBin: "adb" });
  await driver.launch(LAUNCH_CTX);
  assert.deepEqual(exec.calls[0], { tool: "adb", args: ["devices"], binary: false });
});

test("AndroidDriver.launch without devices → infra_flake:device_offline", async () => {
  const exec = fakeExec(() => okRun("List of devices attached\n"));
  const driver = new AndroidDriver({ env: {}, exec });
  await assert.rejects(
    driver.launch(LAUNCH_CTX),
    (err: unknown) =>
      err instanceof InfraFlakeError &&
      err.failureClass === "infra_flake:device_offline" &&
      err.tool === "adb",
  );
});

test("AndroidDriver.launch with adb missing → infra_flake:tool_missing", async () => {
  const exec = fakeExec(() => ({ ...okRun(), code: -1, spawnError: "spawn adb ENOENT" }));
  const driver = new AndroidDriver({ env: {}, exec });
  await assert.rejects(
    driver.launch(LAUNCH_CTX),
    (err: unknown) =>
      err instanceof InfraFlakeError && err.failureClass === "infra_flake:tool_missing",
  );
});

test("AndroidDriver.act renders a maestro flow and runs `maestro test`", async () => {
  const exec = fakeExec((tool) => (tool === "adb" ? okRun(ADB_DEVICES_OK) : okRun()));
  const driver = new AndroidDriver({ env: { E2AI_ANDROID_APP_ID: "com.e2ai.app" }, exec });
  const res = await driver.act(decision({ kind: "act", selector: "#balance" }));
  const maestroCall = exec.calls[1]!;
  assert.equal(maestroCall.tool, "maestro");
  assert.equal(maestroCall.args[0], "test");
  assert.match(maestroCall.args[1]!, /\.yaml$/);
  // flow written to the temp path is exactly what act reports
  const flow = await readFile(maestroCall.args[1]!, "utf8");
  assert.match(flow, /launchApp:\n {4}appId: com\.e2ai\.app/);
  assert.match(flow, /id: #balance/);
  assert.deepEqual(res, { action: { maestroFlow: flow }, evidence: [] });
});

test("AndroidDriver.act when maestro flow fails → infra_flake:maestro_flow_failed", async () => {
  let first = true;
  const exec = fakeExec((tool) => {
    if (tool === "adb") return okRun(ADB_DEVICES_OK);
    if (first) {
      first = false;
      return failRun(1, "flow failed: element not found");
    }
    return failRun();
  });
  const driver = new AndroidDriver({ env: {}, exec });
  await assert.rejects(
    driver.act(decision({ kind: "act", selector: "#x" })),
    (err: unknown) =>
      err instanceof InfraFlakeError &&
      err.failureClass === "infra_flake:maestro_flow_failed" &&
      err.tool === "maestro" &&
      err.message.includes("element not found"),
  );
});

test("AndroidDriver.act navigate goes through `adb shell am start`, not maestro", async () => {
  const exec = fakeExec((tool) => (tool === "adb" ? okRun(ADB_DEVICES_OK) : failRun()));
  const driver = new AndroidDriver({ env: {}, exec });
  const res = await driver.act(decision({ kind: "navigate", url: "e2ai://open/accounts" }));
  const amCall = exec.calls[1]!;
  assert.deepEqual(amCall.args, [
    "shell",
    "am",
    "start",
    "-a",
    "android.intent.action.VIEW",
    "-d",
    "e2ai://open/accounts",
  ]);
  assert.deepEqual(res, { action: { navigated: "e2ai://open/accounts" }, evidence: [] });
});

test("AndroidDriver.capture screenshot persists screencap bytes as run-relative ref", async () => {
  const artDir = await mkdtemp(join(tmpdir(), "e2ai-android-"));
  try {
    const exec = fakeExec((tool, _args, binary) =>
      tool === "adb"
        ? binary
          ? {
              code: 0,
              signal: null,
              stdout: "",
              stderr: "",
              timedOut: false,
              stdoutBase64: Buffer.from("png-bytes").toString("base64"),
            }
          : okRun(ADB_DEVICES_OK)
        : failRun(),
    );
    const driver: SurfaceDriver = new AndroidDriver({ env: {}, exec });
    const paths = artifactPaths(artDir, "run-1", "sess-1");
    const refs = await driver.capture("screenshot", { paths, idx: 3 });
    assert.deepEqual(refs, [
      { kind: "screenshot", path: join("agents", "sess-1", "screenshots", "step-3.png") },
    ]);
    assert.equal(await readFile(join(paths.screenshotsDir, "step-3.png"), "utf8"), "png-bytes");
  } finally {
    await rm(artDir, { recursive: true, force: true });
  }
});

test("AndroidDriver.capture screenshot with empty stdout → infra_flake:screenshot_failed", async () => {
  const exec = fakeExec((tool, _args, binary) =>
    tool === "adb" ? (binary ? okRun() : okRun(ADB_DEVICES_OK)) : failRun(),
  );
  const driver = new AndroidDriver({ env: {}, exec });
  await assert.rejects(
    driver.capture("screenshot", {
      paths: artifactPaths("/tmp/e2ai-never", "r", "s"),
      idx: 0,
    }),
    (err: unknown) =>
      err instanceof InfraFlakeError && err.failureClass === "infra_flake:screenshot_failed",
  );
});

// ------------------------------------------------------------------------ ios

const SIMCTL_BOOTED = [
  "Known Devices:",
  "AAA-111-UDID (iPhone 16 Pro) (Booted)",
  "BBB-222-UDID (iPhone 15) (Shutdown)",
  "",
].join("\n");

function iosExec(handler?: (tool: string, args: string[]) => ToolRun | LongProcess) {
  return fakeExec((tool, args, binary) => {
    if (tool === "xcrun" && args.includes("list") && args.includes("booted")) {
      return okRun(SIMCTL_BOOTED);
    }
    return handler ? handler(tool, args) : okRun();
  });
}

test("IOSDriver.launch picks the first booted UDID and overrides the status bar", async () => {
  const exec = iosExec();
  const driver = new IOSDriver({ env: {}, exec });
  await driver.launch(LAUNCH_CTX);
  const bar = exec.calls[1]!;
  assert.equal(bar.tool, "xcrun");
  assert.deepEqual(
    bar.args.slice(1),
    ["status_bar", "AAA-111-UDID", "override", "--time", "9:41", "--batteryState", "charged", "--batteryLevel", "100"],
  );
});

test("IOSDriver.launch with no booted simulator → infra_flake:device_offline", async () => {
  const exec = fakeExec(() => okRun("Known Devices:\nAAA (Shutdown)\n"));
  const driver = new IOSDriver({ env: {}, exec });
  await assert.rejects(
    driver.launch(LAUNCH_CTX),
    (err: unknown) =>
      err instanceof InfraFlakeError &&
      err.failureClass === "infra_flake:device_offline" &&
      err.tool === "xcrun",
  );
});

test("IOSDriver.launch with pinned UDID not booted → infra_flake:device_offline", async () => {
  const exec = iosExec();
  const driver = new IOSDriver({ env: {}, exec, udid: "ZZZ-999" });
  await assert.rejects(
    driver.launch(LAUNCH_CTX),
    (err: unknown) =>
      err instanceof InfraFlakeError &&
      err.failureClass === "infra_flake:device_offline" &&
      err.message.includes("ZZZ-999"),
  );
});

test("IOSDriver.act runs the maestro flow; navigate uses `simctl openurl`", async () => {
  const exec = iosExec();
  const driver = new IOSDriver({ env: { E2AI_IOS_APP_ID: "com.e2ai.ios" }, exec });
  const res = await driver.act(decision({ kind: "act", label: "Pay" }));
  assert.equal(exec.calls[1]!.tool, "maestro");
  assert.deepEqual(exec.calls[1]!.args[0], "test");
  assert.ok(hasMaestroFlow(res.action));
  assert.match(res.action.maestroFlow, /tapOn: "Pay"/);

  const nav = await driver.act(decision({ kind: "navigate", url: "https://staging.e2ai/x" }));
  const open = exec.calls.at(-1)!;
  assert.deepEqual(open.args.slice(1), ["openurl", "AAA-111-UDID", "https://staging.e2ai/x"]);
  assert.deepEqual(nav, { action: { navigated: "https://staging.e2ai/x" }, evidence: [] });
});

test("IOSDriver.capture screenshot invokes `simctl io <udid> screenshot <path>`", async () => {
  const artDir = await mkdtemp(join(tmpdir(), "e2ai-ios-"));
  try {
    const exec = iosExec();
    const driver = new IOSDriver({ env: {}, exec });
    const paths = artifactPaths(artDir, "run-1", "sess-2");
    const refs = await driver.capture("screenshot", { paths, idx: 1 });
    const shot = exec.calls.at(-1)!;
    assert.equal(shot.tool, "xcrun");
    // real simctl syntax: `xcrun simctl io <udid> screenshot <output path>`
    assert.deepEqual(shot.args.slice(1), [
      "io",
      "AAA-111-UDID",
      "screenshot",
      join(paths.screenshotsDir, "step-1.png"),
    ]);
    assert.deepEqual(refs, [
      { kind: "screenshot", path: join("agents", "sess-2", "screenshots", "step-1.png") },
    ]);
  } finally {
    await rm(artDir, { recursive: true, force: true });
  }
});

// ------------------------------------------------------------------------ web

test("WebDriver without a page fails typed (SurfaceConfigError), never raw", async () => {
  const driver = new WebDriver({ env: {} });
  await assert.rejects(driver.observe(), SurfaceConfigError);
  await assert.rejects(
    driver.act(decision({ kind: "act", selector: "#x" })),
    SurfaceConfigError,
  );
});

test("WebDriver.capture refuses video at the seam (T5 owns browser video)", async () => {
  const driver = new WebDriver({ env: {} });
  await assert.rejects(
    driver.capture("video", { paths: artifactPaths("/tmp/e2ai-never", "r", "s"), idx: 0 }),
    SurfaceConfigError,
  );
});
