// driver/simctl-ios.ts — IOSDriver: xcrun simctl implementation of the seam
// (iOS simulator surface, plan 04-01 T3 scaffold).
//
// Toolchain: `xcrun simctl list devices booted` (boot gate), `xcrun simctl
// status_bar <udid> override ...` (deterministic status bar for evidence),
// `xcrun simctl io <udid> screenshot` (PNG), `xcrun simctl io <udid>
// recordVideo` (long-running MP4), `xcrun simctl openurl <udid> <url>`
// (navigation). `maestro test <flow>` for act decisions — same flow mapping
// as Android via maestro-flow.ts.
//
// Missing xcrun/simctl or no booted simulator ⇒ typed InfraFlakeError
// ("infra_flake:tool_missing" / "infra_flake:device_offline") — the device
// layer failed before the app under test could be exercised (PLAT-04). Never
// raw throws.
//
// End-to-end simulator smoke is NOT this task's acceptance (T5/T6 own device
// tasks); correctness here is the seam + real child_process invocation via
// ToolExec (test-fakeable).

import { mkdir, writeFile } from "node:fs/promises";
import { join, relative } from "node:path";
import type { Decision, EvidenceRef } from "../loop.js";
import {
  InfraFlakeError,
  type ActResult,
  type CaptureContext,
  type DriverObservation,
  type LaunchContext,
  type SurfaceDriver,
} from "../surface.js";
import type { ToolExec, LongProcess } from "./exec.js";
import { systemExec } from "./exec.js";
import { maestroFlowYaml } from "./maestro-flow.js";

const PROBE_TIMEOUT_MS = 10_000;
const MAESTRO_TEST_TIMEOUT_MS = 180_000;
const RECORD_VIDEO_TIMEOUT_MS = 300_000;

/** Extract a UDID from a `simctl list devices` line. Handles both output
 * shapes: "iPhone 17 (UDID) (Booted)" (parenthesized UUID) and the older
 * "UDID (iPhone 17) (Booted)" (leading token before " ("). */
export function parseUdid(line: string): string | null {
  const tokens = line.match(/\(([^)]+)\)/g) ?? [];
  for (const tok of tokens) {
    const inner = tok.slice(1, -1);
    if (inner.length >= 25 && /^[0-9A-Fa-f-]+$/.test(inner)) return inner;
  }
  const lead = line.split(" (")[0]?.trim() ?? "";
  return lead.length > 0 && !lead.includes("(") ? lead : null;
}

export interface IOSDriverDeps {
  env: NodeJS.ProcessEnv;
  /** Injection seam for tests; defaults to real child_process spawns. */
  exec?: ToolExec;
  /** Binary overrides (tests); defaults "xcrun" / "maestro". */
  xcrunBin?: string;
  maestroBin?: string;
  /** Pinned simulator UDID (E2AI_IOS_UDID); when absent the first booted
   * device is used. */
  udid?: string;
}

export class IOSDriver implements SurfaceDriver {
  private readonly exec: ToolExec;
  private readonly xcrun: string;
  private readonly maestro: string;
  private readonly env: NodeJS.ProcessEnv;
  private readonly pinnedUdid?: string;
  private recording: LongProcess | null = null;

  constructor(deps: IOSDriverDeps) {
    this.exec = deps.exec ?? systemExec();
    this.xcrun = deps.xcrunBin ?? "xcrun";
    this.maestro = deps.maestroBin ?? "maestro";
    this.env = deps.env;
    this.pinnedUdid = deps.udid ?? deps.env.E2AI_IOS_UDID;
  }

  private simctl(args: string[]): { tool: string; args: string[] } {
    return { tool: this.xcrun, args: ["simctl", ...args] };
  }

  /** Boot gate: exactly ≥1 booted simulator must exist. Output shape:
   * line per device, " (Booted)" suffix marks the live one. */
  private async assertBooted(): Promise<string> {
    const probe = this.simctl(["list", "devices", "booted"]);
    const run = await this.exec.run(probe.tool, probe.args, { timeoutMs: PROBE_TIMEOUT_MS });
    if (run.spawnError) {
      throw new InfraFlakeError("infra_flake:tool_missing", this.xcrun, run.spawnError);
    }
    if (run.timedOut) {
      throw new InfraFlakeError(
        "infra_flake:probe_timeout",
        this.xcrun,
        "simctl list devices did not answer in time",
      );
    }
    const booted = run.stdout
      .split("\n")
      .map((l) => l.trim())
      .filter((l) => l.includes("(Booted)"));
    if (booted.length === 0) {
      throw new InfraFlakeError(
        "infra_flake:device_offline",
        this.xcrun,
        "no booted iOS simulator (simctl list devices booted: empty)",
      );
    }
    const pinned = this.pinnedUdid;
    if (pinned) {
      // The UDID string appears verbatim on its device line in every simctl
      // output shape, so a substring match is format-agnostic.
      const match = booted.find((l) => l.includes(pinned));
      if (!match) {
        throw new InfraFlakeError(
          "infra_flake:device_offline",
          this.xcrun,
          `pinned UDID ${this.pinnedUdid} is not booted`,
        );
      }
      return this.pinnedUdid;
    }
    // First booted UDID. Real simctl prints "iPhone 17 (UDID) (Booted)"; some
    // Xcode versions print the inverted "UDID (iPhone 17) (Booted)" — accept
    // both rather than pinning the parser to one era's formatting.
    const udid = parseUdid(booted[0]!);
    if (!udid) {
      throw new InfraFlakeError(
        "infra_flake:device_offline",
        this.xcrun,
        `could not parse a UDID from simctl output line: ${booted[0]}`,
      );
    }
    return udid;
  }

  async launch(_ctx: LaunchContext): Promise<void> {
    const udid = await this.assertBooted();
    // Deterministic status bar for evidence capture (battery full, fixed
    // clock). Best-effort — a failure here is not fatal to the run.
    const bar = this.simctl([
      "status_bar",
      udid,
      "override",
      "--time",
      "9:41",
      "--batteryState",
      "charged",
      "--batteryLevel",
      "100",
    ]);
    await this.exec.run(bar.tool, bar.args);
  }

  /** Observation: installed-app listing from the booted simulator (a11y
   * grounding placeholder until a UI dump path lands with T5/T6). */
  async observe(): Promise<DriverObservation> {
    const udid = await this.assertBooted();
    const probe = this.simctl(["listapps", udid]);
    const run = await this.exec.run(probe.tool, probe.args, { timeoutMs: PROBE_TIMEOUT_MS });
    if (run.code !== 0) {
      throw new InfraFlakeError(
        "infra_flake:probe_timeout",
        this.xcrun,
        `simctl listapps exit ${run.code}: ${run.stderr.trim() || "no output"}`,
      );
    }
    return { a11y: { format: "simctl-listapps", content: run.stdout } };
  }

  async act(decision: Decision): Promise<ActResult> {
    const udid = await this.assertBooted();
    if (decision.kind === "navigate") {
      const url = String(decision.url ?? "");
      const open = this.simctl(["openurl", udid, url]);
      const run = await this.exec.run(open.tool, open.args);
      if (run.code !== 0) {
        throw new InfraFlakeError(
          "infra_flake:navigation_failed",
          this.xcrun,
          `simctl openurl exit ${run.code}: ${run.stderr.trim() || "no output"}`,
        );
      }
      return { action: { navigated: url }, evidence: [] };
    }
    if (decision.kind !== "act") {
      return { action: { kind: decision.kind, noop: true }, evidence: [] };
    }

    const flowPath = `/tmp/e2ai-flow-${process.pid}.yaml`;
    const flow = maestroFlowYaml(decision, { appId: this.env.E2AI_IOS_APP_ID });
    await writeFile(flowPath, flow, "utf8");
    // Pin the device explicitly: with an Android emulator also running,
    // maestro auto-selects `emulator-5554` and runs the iOS flow on the
    // wrong platform (probed live on the iPhone 17 sim, plan 04-01 T6).
    const run = await this.exec.run(this.maestro, ["test", "--udid", udid, flowPath], {
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
    const udid = await this.assertBooted();
    if (kind === "screenshot") return this.captureScreenshot(udid, ctx);
    return this.captureVideo(udid, ctx);
  }

  private async captureScreenshot(udid: string, ctx: CaptureContext): Promise<EvidenceRef[]> {
    await mkdir(ctx.paths.screenshotsDir, { recursive: true });
    const screenshotPath = join(ctx.paths.screenshotsDir, `step-${ctx.idx}.png`);
    const io = this.simctl(["io", udid, "screenshot", screenshotPath]);
    const run = await this.exec.run(io.tool, io.args, { binary: true });
    if (run.spawnError) {
      throw new InfraFlakeError("infra_flake:tool_missing", this.xcrun, run.spawnError);
    }
    if (run.code !== 0) {
      throw new InfraFlakeError(
        "infra_flake:screenshot_failed",
        this.xcrun,
        `simctl io screenshot exit ${run.code}: ${run.stderr.trim() || "no output"}`,
      );
    }
    // simctl io writes the PNG to the local path argument directly.
    return [{ kind: "screenshot", path: relative(ctx.paths.runDir, screenshotPath) }];
  }

  private async captureVideo(udid: string, ctx: CaptureContext): Promise<EvidenceRef[]> {
    if (this.recording) {
      throw new InfraFlakeError(
        "infra_flake:video_failed",
        this.xcrun,
        "a simctl recordVideo is already in flight",
      );
    }
    await mkdir(ctx.paths.videosDir, { recursive: true });
    const videoPath = join(ctx.paths.videosDir, `step-${ctx.idx}.mp4`);
    const io = this.simctl(["io", udid, "recordVideo", "--force", videoPath]);
    const proc = await this.exec.spawnLong(io.tool, io.args);
    this.recording = proc;
    // recordVideo runs until signalled; bound the window then stop.
    const done = await Promise.race([
      proc.done,
      new Promise<null>((resolve) => setTimeout(() => resolve(null), RECORD_VIDEO_TIMEOUT_MS)),
    ]);
    this.recording = null;
    if (!done) {
      proc.kill();
    }
    const finalRun = done ?? (await proc.done);
    if (finalRun.spawnError) {
      throw new InfraFlakeError("infra_flake:tool_missing", this.xcrun, finalRun.spawnError);
    }
    // graceful SIGTERM stop is the expected end of a bounded recording
    const cleanStop =
      finalRun.code === 0 ||
      finalRun.signal === "SIGTERM" ||
      finalRun.code === -15;
    if (!cleanStop) {
      throw new InfraFlakeError(
        "infra_flake:video_failed",
        this.xcrun,
        `simctl recordVideo exit ${finalRun.code}: ${finalRun.stderr.trim() || "no output"}`,
      );
    }
    return [{ kind: "video", path: relative(ctx.paths.runDir, videoPath) }];
  }

  async teardown(_err?: unknown): Promise<void> {
    this.recording?.kill();
    this.recording = null;
  }
}
