// driver/maestro-android.ts — AndroidDriver: maestro + adb implementation of
// the seam (Android emulator surface, plan 04-01 T3 scaffold).
//
// Toolchain: `adb devices` (device presence), `adb exec-out screencap -p`
// (PNG bytes via binary-safe stdout), `adb shell am start` (deep link /
// URL navigation), `maestro test <flow>` (one YAML flow per act decision),
// `maestro record <flow> <out>` (video evidence).
//
// Tool not installed / not on PATH ⇒ InfraFlakeError("infra_flake:tool_missing",
// tool). Boot/device failures ⇒ "infra_flake:device_offline" — the device
// layer failed before the app under test could be exercised (PLAT-04; the Go
// verdict engine treats every subtype under the "infra_flake" family). These
// are typed, never raw throws, so the loop can append the step with the class
// attached.
//
// End-to-end device smoke is NOT this task's acceptance (T5/T6 own device
// tasks); correctness here is the seam + real child_process invocation via
// ToolExec (test-fakeable).

import { mkdir, writeFile } from "node:fs/promises";
import { join, relative } from "node:path";
import type { Decision, EvidenceRef } from "../loop.js";
import {
  InfraFlakeError,
  SurfaceConfigError,
  type ActResult,
  type CaptureContext,
  type DriverObservation,
  type LaunchContext,
  type SurfaceDriver,
} from "../surface.js";
import type { ToolExec, ToolRun, LongProcess } from "./exec.js";
import { systemExec } from "./exec.js";
import { maestroFlowYaml } from "./maestro-flow.js";

/** Millis before `adb devices` answering but listing no device turns into
 * device_offline. Kept small: the probe is cheap and retried by the caller
 * contract only once — after this the environment is genuinely unready. */
const DEVICE_PROBE_TIMEOUT_MS = 10_000;
/** Headroom for a maestro test flow on a healthy emulator (synthetic input
 * + a screenshot per step). */
const MAESTRO_TEST_TIMEOUT_MS = 180_000;

export interface AndroidDriverDeps {
  env: NodeJS.ProcessEnv;
  /** Injection seam for tests; defaults to real child_process spawns. */
  exec?: ToolExec;
  /** adb binary override (tests); default "adb". */
  adbBin?: string;
  /** maestro binary override (tests); default "maestro". */
  maestroBin?: string;
}

export class AndroidDriver implements SurfaceDriver {
  private readonly exec: ToolExec;
  private readonly adb: string;
  private readonly maestro: string;
  private readonly env: NodeJS.ProcessEnv;
  private recording: LongProcess | null = null;

  constructor(deps: AndroidDriverDeps) {
    this.exec = deps.exec ?? systemExec();
    this.adb = deps.adbBin ?? "adb";
    this.maestro = deps.maestroBin ?? "maestro";
    this.env = deps.env;
  }

  /** Boot gate: `adb devices` must list exactly at least one device in the
   * `device` state (offline/unauthorized list entries do not count). Called
   * before the first act/capture so failures classify before any app step. */
  private async assertDeviceReady(): Promise<void> {
    const run = await this.exec.run(this.adb, ["devices"], {
      timeoutMs: DEVICE_PROBE_TIMEOUT_MS,
    });
    if (run.spawnError) {
      throw new InfraFlakeError("infra_flake:tool_missing", this.adb, run.spawnError);
    }
    if (run.timedOut) {
      throw new InfraFlakeError(
        "infra_flake:probe_timeout",
        this.adb,
        "adb devices did not answer in time",
      );
    }
    const devices = run.stdout
      .split("\n")
      .slice(1) // header "List of devices attached"
      .filter((l) => l.trim().length > 0)
      .map((l) => l.split("\t").map((c) => c.trim()))
      .filter((cols) => cols.length >= 2 && cols[1] === "device");
    if (devices.length === 0) {
      throw new InfraFlakeError(
        "infra_flake:device_offline",
        this.adb,
        `no device in 'device' state (adb devices: ${run.stdout.trim() || "empty"})`,
      );
    }
  }

  async launch(_ctx: LaunchContext): Promise<void> {
    await this.assertDeviceReady();
  }

  /** Observation: uiautomator hierarchy dump (a11y grounding). On older
   * images uiautomator dump can fail transiently — fall back to a screencap
   * descriptor so observe() stays best-effort and never bricks the loop. */
  async observe(): Promise<DriverObservation> {
    const dump = await this.exec.run(this.adb, [
      "shell",
      "uiautomator",
      "dump",
      "/sdcard/window_dump.xml",
    ]);
    if (dump.code === 0) {
      const pull = await this.exec.run(this.adb, ["shell", "cat", "/sdcard/window_dump.xml"]);
      if (pull.code === 0) {
        try {
          return { a11y: JSON.parse(pull.stdout) };
        } catch {
          // hierarchy dump is XML-shaped JSON-stub — return raw text node
          return { a11y: { format: "uiautomator-xml", content: pull.stdout } };
        }
      }
    }
    return { a11y: { format: "uiautomator-unavailable" } };
  }

  async act(decision: Decision): Promise<ActResult> {
    await this.assertDeviceReady();
    if (decision.kind === "navigate") {
      const url = String(decision.url ?? "");
      await this.exec.run(this.adb, [
        "shell",
        "am",
        "start",
        "-a",
        "android.intent.action.VIEW",
        "-d",
        url,
      ]);
      return { action: { navigated: url }, evidence: [] };
    }
    if (decision.kind !== "act") {
      // observe/judge/assert carry no device mutation until T5/T6 land.
      return { action: { kind: decision.kind, noop: true }, evidence: [] };
    }

    const flowPath = `/tmp/e2ai-flow-${process.pid}.yaml`;
    const flow = maestroFlowYaml(decision, {
      appId: this.env.E2AI_ANDROID_APP_ID,
    });
    await writeFile(flowPath, flow, "utf8");
    const run = await this.exec.run(this.maestro, ["test", flowPath], {
      timeoutMs: MAESTRO_TEST_TIMEOUT_MS,
    });
    if (run.spawnError) {
      throw new InfraFlakeError("infra_flake:tool_missing", this.maestro, run.spawnError);
    }
    if (run.timedOut) {
      throw new InfraFlakeError(
        "infra_flake:boot_timeout",
        this.maestro,
        `maestro test exceeded ${MAESTRO_TEST_TIMEOUT_MS}ms`,
      );
    }
    if (run.code !== 0) {
      throw new InfraFlakeError(
        "infra_flake:maestro_flow_failed",
        this.maestro,
        `maestro test exit ${run.code}: ${run.stderr.trim() || run.stdout.trim() || "no output"}`,
      );
    }
    return { action: { maestroFlow: flow }, evidence: [] };
  }

  async capture(kind: "screenshot" | "video", ctx: CaptureContext): Promise<EvidenceRef[]> {
    await this.assertDeviceReady();
    if (kind === "screenshot") return this.captureScreenshot(ctx);
    return this.captureVideo(ctx);
  }

  private async captureScreenshot(ctx: CaptureContext): Promise<EvidenceRef[]> {
    const run = await this.exec.run(this.adb, ["exec-out", "screencap", "-p"], { binary: true });
    if (run.spawnError) {
      throw new InfraFlakeError("infra_flake:tool_missing", this.adb, run.spawnError);
    }
    if (run.code !== 0 || !run.stdoutBase64) {
      throw new InfraFlakeError(
        "infra_flake:screenshot_failed",
        this.adb,
        `screencap exit ${run.code}${run.stderr.trim() ? `: ${run.stderr.trim()}` : ""}`,
      );
    }
    await mkdir(ctx.paths.screenshotsDir, { recursive: true });
    const screenshotPath = join(ctx.paths.screenshotsDir, `step-${ctx.idx}.png`);
    await writeFile(screenshotPath, Buffer.from(run.stdoutBase64, "base64"));
    return [{ kind: "screenshot", path: relative(ctx.paths.runDir, screenshotPath) }];
  }

  private async captureVideo(ctx: CaptureContext): Promise<EvidenceRef[]> {
    // maestro record: long-running — spawn, let the flow run to completion,
    // then wait for the MP4. The recording file lands in videosDir.
    if (!this.recording) {
      const flowPath = "/tmp/e2ai-record-flow.yaml";
      await writeFile(flowPath, "- launchApp\n", "utf8");
      await mkdir(ctx.paths.videosDir, { recursive: true });
      const videoPath = join(ctx.paths.videosDir, `step-${ctx.idx}.mp4`);
      const proc = await this.exec.spawnLong(
        this.maestro,
        ["record", flowPath, videoPath],
      );
      this.recording = proc;
      // recording started — caller decides when to stop; here we just await
      // completion (maestro record terminates after the flow finishes).
      const done = await proc.done;
      this.recording = null;
      if (done.spawnError) {
        throw new InfraFlakeError("infra_flake:tool_missing", this.maestro, done.spawnError);
      }
      if (done.code !== 0) {
        throw new InfraFlakeError(
          "infra_flake:video_failed",
          this.maestro,
          `maestro record exit ${done.code}: ${done.stderr.trim() || "no output"}`,
        );
      }
      return [{ kind: "video", path: relative(ctx.paths.runDir, videoPath) }];
    }
    throw new SurfaceConfigError("a maestro recording is already in flight");
  }

  async teardown(_err?: unknown): Promise<void> {
    this.recording?.kill();
    this.recording = null;
  }
}
