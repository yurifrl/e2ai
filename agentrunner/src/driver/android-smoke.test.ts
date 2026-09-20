// android-smoke.test.ts — AndroidDriver real-device smoke (plan 04-01 T5).
//
// Two layers:
//
// 1. Device-free unit tests of the `app://<package>` navigate mapping
//    (resolve-activity → `am start -n`) against spawn stubs — these run
//    everywhere, no tooling needed.
//
// 2. The real-device smoke against the `e2ai-pixel` AVD (API 35,
//    google_apis/arm64-v8a): boots the emulator headless, drives the system
//    Settings app end-to-end through AndroidDriver (launch → navigate →
//    observe → act via maestro → capture screenshot → teardown), captures a
//    real PNG, then shuts the emulator down (`adb emu kill`). Gated on
//    tooling presence (adb + emulator + avdmanager from ANDROID_HOME or the
//    homebrew default, plus a JVM for maestro); skips cleanly without.
//    Set E2AI_ANDROID_SMOKE=1 to turn a missing-tooling skip into a hard
//    failure (CI should do exactly that).
//
// Report-only invariant: the smoke drives the system Settings UI read-only —
// no app data is mutated, and the emulator's userdata partition is disposable.
//
// Timer note: the boot/focus/shutdown waits are genuine integration waits on
// external processes (QEMU + adb) — no fake-timer or event-signal alternative
// exists; each poll checks a real condition (`getprop`, `dumpsys`) rather
// than guessing a duration.

import test, { type TestContext } from "node:test";
import assert from "node:assert/strict";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { accessSync, constants as fsConstants, createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { AndroidDriver } from "./maestro-android.js";
import { InfraFlakeError, SurfaceConfigError } from "../surface.js";
import { artifactPaths, type Decision } from "../loop.js";
import type { ToolExec, ToolRun } from "./exec.js";

// ------------------------------------------------------------------- helpers

const ADB_DEVICES_OK = "List of devices attached\nemulator-5554\tdevice\n";

function okRun(stdout: string): ToolRun {
  return { code: 0, signal: null, stdout, stderr: "", timedOut: false };
}

function failRun(stderr = "boom"): ToolRun {
  return { code: 1, signal: null, stdout: "", stderr, timedOut: false };
}

function decision(partial: Partial<Decision> & { kind: Decision["kind"] }): Decision {
  return partial as Decision;
}

/** Spawn-stub ToolExec recording every call; handler answers per tool. */
function fakeExec(handler: (tool: string, args: string[]) => ToolRun): ToolExec & {
  calls: Array<{ tool: string; args: string[] }>;
} {
  const calls: Array<{ tool: string; args: string[] }> = [];
  return {
    calls,
    async run(tool, args) {
      calls.push({ tool, args });
      return handler(tool, args);
    },
    async spawnLong() {
      throw new Error("spawnLong not expected in these tests");
    },
  };
}

function binExecutable(path: string): boolean {
  try {
    accessSync(path, fsConstants.X_OK);
    return true;
  } catch {
    return false;
  }
}

/** Poll until `fn` returns true; resolves false at the deadline without
 * throwing — callers that need a failure message assert themselves. Poll
 * interval is real time — see the timer note at the top of this file. */
async function pollFor(fn: () => boolean | Promise<boolean>, deadlineMs: number, intervalMs = 500): Promise<boolean> {
  const deadline = Date.now() + deadlineMs;
  for (;;) {
    if (await fn()) return true;
    if (Date.now() >= deadline) return false;
    await new Promise<void>((resolve) => setTimeout(resolve, intervalMs));
  }
}

/** Poll until `fn` returns true; asserts with `fail` at the deadline. */
async function pollUntil(
  fn: () => boolean | Promise<boolean>,
  fail: () => string,
  deadlineMs: number,
  intervalMs = 500,
): Promise<void> {
  if (!(await pollFor(fn, deadlineMs, intervalMs))) assert.fail(fail());
}

// ---------------------------------------------- app:// navigate (device-free)

test("AndroidDriver navigate app:// resolves launcher activity and starts by component", async () => {
  const exec = fakeExec((tool, args) => {
    if (tool !== "adb") return failRun();
    if (args[0] === "devices") return okRun(ADB_DEVICES_OK);
    if (args[0] === "shell" && args[1] === "cmd") {
      // `cmd package resolve-activity --brief <pkg>`: component is the last
      // non-empty stdout line.
      return okRun(
        "priority=0 preferredOrder=0 match=0x108000 specificIndex=-1 isDefault=true\ncom.android.settings/.Settings\n",
      );
    }
    return okRun("Starting: Intent { cmp=com.android.settings/.Settings }");
  });
  const driver = new AndroidDriver({ env: {}, exec });
  const res = await driver.act(decision({ kind: "navigate", url: "app://com.android.settings" }));
  const amCall = exec.calls.at(-1)!;
  assert.deepEqual(amCall.args, [
    "shell",
    "am",
    "start",
    "-n",
    "com.android.settings/.Settings",
  ]);
  assert.deepEqual(res, {
    action: { navigated: "app://com.android.settings", component: "com.android.settings/.Settings" },
    evidence: [],
  });
});

test("AndroidDriver navigate app:// to a package with no launcher fails typed", async () => {
  const exec = fakeExec((tool, args) => {
    if (tool !== "adb") return failRun();
    if (args[0] === "devices") return okRun(ADB_DEVICES_OK);
    // resolve-activity exits 0 even for unknown packages — empty stdout is
    // the "no launcher" signal.
    return okRun("");
  });
  const driver = new AndroidDriver({ env: {}, exec });
  await assert.rejects(
    driver.act(decision({ kind: "navigate", url: "app://com.example.missing" })),
    (err: unknown) =>
      err instanceof SurfaceConfigError && /no launcher activity for com\.example\.missing/.test(err.message),
  );
  // The resolve probe ran, but no am start was attempted.
  assert.equal(exec.calls.filter((c) => c.args.includes("start")).length, 0);
});

test("AndroidDriver navigate app:// surfaces resolve-activity spawn failure as infra_flake", async () => {
  const exec = fakeExec((tool, args) => {
    if (tool !== "adb") return failRun();
    if (args[0] === "devices") return okRun(ADB_DEVICES_OK);
    return { code: -1, signal: null, stdout: "", stderr: "", timedOut: false, spawnError: "ENOENT" };
  });
  const driver = new AndroidDriver({ env: {}, exec });
  await assert.rejects(
    driver.act(decision({ kind: "navigate", url: "app://com.android.settings" })),
    (err: unknown) =>
      err instanceof InfraFlakeError &&
      err.failureClass === "infra_flake:tool_missing" &&
      err.tool === "adb",
  );
});

// ---------------------------------------------------------------- real smoke

const AVD_NAME = "e2ai-pixel";
const AVD_KEY = "system-images;android-35;google_apis;arm64-v8a";
const AVD_DEVICE = "pixel_7";
const BOOT_DEADLINE_MS = 420_000;
const FOCUS_DEADLINE_MS = 15_000;
/** Set by the real-smoke gate from resolveMaestro(); "" only in device-free
 * unit tests (which never construct a live driver). */
let MAESTRO_BIN = "";

interface Tooling {
  sdkRoot: string;
  adb: string;
  emulator: string;
  avdmanager: string;
}

function resolveTooling(): Tooling | null {
  const sdkRoots = [
    process.env.ANDROID_HOME,
    process.env.ANDROID_SDK_ROOT,
    "/opt/homebrew/share/android-commandlinetools",
  ].filter((p): p is string => Boolean(p));
  for (const root of sdkRoots) {
    const adb = join(root, "platform-tools", "adb");
    const emulator = join(root, "emulator", "emulator");
    const avdmanager = join(root, "cmdline-tools", "latest", "bin", "avdmanager");
    if (binExecutable(adb) && binExecutable(emulator)) {
      return { sdkRoot: root, adb, emulator, avdmanager: binExecutable(avdmanager) ? avdmanager : "" };
    }
  }
  return null;
}

/** maestro on PATH, else the T2 install location. Null when absent (smoke
 * gates on it). */
function resolveMaestro(): string | null {
  const onPath = runCapture("/usr/bin/env", ["which", "maestro"], 10_000);
  if (onPath.code === 0) return onPath.stdout.trim();
  const homeBin = join(process.env.HOME ?? "", ".maestro", "bin", "maestro");
  return binExecutable(homeBin) ? homeBin : null;
}

function runCapture(bin: string, args: string[], timeoutMs: number): { code: number; stdout: string; stderr: string } {
  const r = spawnSync(bin, args, { timeout: timeoutMs, encoding: "utf8" });
  return {
    code: r.status ?? (r.error ? -1 : 1),
    stdout: r.stdout ?? "",
    stderr: r.stderr ?? "",
  };
}

test(
  "android driver real-device smoke: boot e2ai-pixel, drive Settings, capture PNG, shut down",
  { timeout: BOOT_DEADLINE_MS + 300_000 },
  async (t) => {
    // ---- gate: tooling presence -----------------------------------------
    const tooling = resolveTooling();
    const maestro = tooling ? resolveMaestro() : null;
    if (!tooling || !maestro) {
      const msg = "android tooling (adb/emulator/maestro) not found — skipping real-device smoke";
      if (process.env.E2AI_ANDROID_SMOKE === "1") throw new Error(msg);
      t.skip(msg);
      return;
    }
    // maestro's device layer needs adb on PATH and a JVM; the driver's exec
    // children inherit process.env, so fix it up before any tooling runs.
    process.env.ANDROID_HOME = tooling.sdkRoot;
    process.env.ANDROID_SDK_ROOT = tooling.sdkRoot;
    process.env.PATH = `${join(tooling.sdkRoot, "platform-tools")}:${process.env.PATH ?? ""}`;
    if (!process.env.JAVA_HOME) {
      const brewJdk = "/opt/homebrew/opt/openjdk/libexec/openjdk.jdk/Contents/Home";
      if (binExecutable(join(brewJdk, "bin", "java"))) process.env.JAVA_HOME = brewJdk;
    }

    // ---- AVD: create e2ai-pixel if missing ------------------------------
    const listed = runCapture(tooling.emulator, ["-list-avds"], 10_000);
    if (!listed.stdout.split("\n").includes(AVD_NAME)) {
      assert.ok(tooling.avdmanager, `AVD ${AVD_NAME} missing and avdmanager not found`);
      const created = runCapture(
        tooling.avdmanager,
        ["create", "avd", "--force", "-n", AVD_NAME, "-k", AVD_KEY, "-d", AVD_DEVICE],
        120_000,
      );
      assert.equal(created.code, 0, `avdmanager create failed: ${created.stderr}\n${created.stdout}`);
    }

    // ---- boot (reuse an already-running device, or cold-boot) -----------
    // A long-running emulator can wedge: the host-side adb server still
    // answers `adb devices` while every `adb shell` hangs (observed on the
    // reuse path). Health-probe before driving — an unresponsive device is
    // torn down and cold-booted fresh instead of driving into a guaranteed
    // maestro timeout.
    const smokeDir = await mkdtemp(join(tmpdir(), "e2ai-android-smoke-"));
    const emuLogPath = join(smokeDir, "emulator.log");
    let emu: ChildProcess | null = null;
    let bootedUs = false;
    const devicesNow = runCapture(tooling.adb, ["devices"], 10_000);
    let reuse = false;
    if (devicesNow.stdout.includes("\tdevice\n")) {
      const health = runCapture(tooling.adb, ["shell", "echo", "e2ai-healthcheck"], 10_000);
      if (health.code === 0 && health.stdout.includes("e2ai-healthcheck")) {
        reuse = true;
      } else {
        // `adb emu kill` reaches the emulator console directly (not through
        // adbd shell), so it lands even on a wedged device; pkill scoped to
        // this AVD is the fallback. The replacement boot below is owned, so
        // the finally block shuts it down.
        runCapture(tooling.adb, ["emu", "kill"], 10_000);
        // SIGKILL: a wedged process may not handle SIGTERM at all. The
        // [-] bracket keeps pkill's own getopt from eating the leading dash.
        spawnSync("pkill", ["-9", "-f", `[-]avd ${AVD_NAME}`], { timeout: 5_000 });
        await pollFor(
          () => !runCapture(tooling.adb, ["devices"], 10_000).stdout.includes("\tdevice\n"),
          30_000,
        );
      }
    }
    if (!reuse) {
      bootedUs = true;
      emu = spawn(
        tooling.emulator,
        [
          "-avd",
          AVD_NAME,
          "-no-window",
          "-no-audio",
          "-no-boot-anim",
          "-gpu",
          "swiftshader_indirect",
          "-no-metrics",
          "-no-snapshot",
        ],
        { stdio: ["ignore", "pipe", "pipe"] },
      );
      // Full boot log for triage (INFRA_FLAKE evidence if boot never lands).
      const emuLog = createWriteStream(emuLogPath);
      emu.stdout?.pipe(emuLog);
      emu.stderr?.pipe(emuLog);

      await pollUntil(
        () => {
          if (emu!.exitCode !== null) {
            assert.fail(`emulator exited during boot (exit ${emu!.exitCode}) — log at ${emuLogPath}`);
          }
          const wait = runCapture(tooling.adb, ["wait-for-device"], 15_000);
          if (wait.code !== 0) return false;
          const props = runCapture(tooling.adb, ["shell", "getprop", "sys.boot_completed"], 10_000);
          return props.stdout.trim() === "1";
        },
        () => `emulator did not reach sys.boot_completed=1 in ${BOOT_DEADLINE_MS}ms — log at ${emuLogPath}`,
        BOOT_DEADLINE_MS,
        2_000,
      );
    }

    // Everything below owns the device lifecycle: always shut down at the end.
    MAESTRO_BIN = maestro;
    let smokeError: unknown;
    try {
      await driveSettingsSmoke(t);
    } catch (err) {
      smokeError = err;
    } finally {
      if (bootedUs) {
        // `adb emu kill` is the graceful emulator shutdown path.
        const kill = runCapture(tooling.adb, ["emu", "kill"], 15_000);
        if (kill.code !== 0 && emu && emu.exitCode === null) emu.kill("SIGTERM");
        await pollUntil(
          () => emu === null || emu.exitCode !== null,
          () => "emulator did not exit after `adb emu kill`",
          30_000,
        );
      }
    }
    if (smokeError) throw smokeError;
  },
);

/** The actual end-to-end drive: AndroidDriver against the live emulator. */
async function driveSettingsSmoke(t: TestContext): Promise<void> {
  const dir = await mkdtemp(join(tmpdir(), "e2ai-android-smoke-run-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const paths = artifactPaths(dir, "smoke-run", "smoke-session");
  const driver = new AndroidDriver({
    env: { E2AI_ANDROID_APP_ID: "com.android.settings" },
    maestroBin: MAESTRO_BIN,
  });

  // launch: device presence gate passes against the live adb.
  await driver.launch({ env: process.env });

  // navigate: native app launch by resolved launcher component. The device
  // may have been left on an in-app sub-screen (am start resumes the task
  // with that screen still on top), and BACK can race am start — so retry
  // the reset+navigate sequence until Settings root actually has focus.
  // Navigation only; no app data is touched.
  // Only the mCurrentFocus line counts: the full dumpsys output mentions
  // every package somewhere, so a whole-output includes() matches spuriously.
  const focusLine = (): string =>
    runCapture(toolingAdb(), ["shell", "dumpsys", "window"], 10_000).stdout.match(/mCurrentFocus=[^\n]*/)?.[0] ?? "";
  let lastAction: unknown;
  let focusReached = false;
  for (let attempt = 0; attempt < 5 && !focusReached; attempt++) {
    await runCapture(toolingAdb(), ["shell", "input", "keyevent", "KEYCODE_BACK"], 10_000);
    const nav = await driver.act(decision({ kind: "navigate", url: "app://com.android.settings" }));
    lastAction = nav.action;
    focusReached = await pollFor(() => focusLine().includes("com.android.settings"), 4_000);
  }
  assert.ok(focusReached, `window focus never reached com.android.settings; last: ${focusLine()}`);
  const component =
    typeof lastAction === "object" && lastAction !== null && "component" in lastAction
      ? lastAction.component
      : undefined;
  assert.ok(
    typeof component === "string" && component.startsWith("com.android.settings/"),
    `unexpected component resolution: ${JSON.stringify(lastAction)}`,
  );

  // observe: real a11y hierarchy from the device. uiautomator dump fails
  // transiently right after a window change and leaves the previous XML on
  // /sdcard — the driver cats that stale file with exit 0 — so retry until
  // the dump actually shows Settings.
  const a11yContent = async (): Promise<string> => {
    const obs = await driver.observe();
    const a11y: unknown = obs.a11y;
    return typeof a11y === "object" && a11y !== null && "content" in a11y && typeof a11y.content === "string"
      ? a11y.content
      : "";
  };
  let lastA11y = "";
  await pollUntil(
    async () => {
      lastA11y = await a11yContent();
      return lastA11y.includes("com.android.settings");
    },
    () => `a11y dump never showed Settings UI; last dump starts: ${lastA11y.slice(0, 120)}`,
    FOCUS_DEADLINE_MS,
  );
  assert.ok(lastA11y.length > 0, "a11y dump empty");

  // act: a real maestro flow — tap the Settings search bar.
  const actRes = await driver.act(
    decision({ kind: "act", selector: "com.android.settings:id/search_action_bar" }),
  );
  assert.deepEqual(actRes.evidence, []);

  // capture: a real PNG on disk, behind the driver's readiness gate.
  const shot = await driver.capture("screenshot", { paths, idx: 0 });
  assert.equal(shot[0]!.kind, "screenshot");
  const pngPath = join(paths.runDir, shot[0]!.path);
  const bytes = await readFile(pngPath);
  assert.ok(bytes.length > 1_000, `screenshot suspiciously small (${bytes.length} bytes)`);
  assert.deepEqual([...bytes.subarray(0, 4)], [0x89, 0x50, 0x4e, 0x47], "capture is not a PNG");

  await driver.teardown();
}

function toolingAdb(): string {
  return join(process.env.ANDROID_HOME!, "platform-tools", "adb");
}
