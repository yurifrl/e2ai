// surface.ts — the SurfaceDriver seam (Phase 4 T3).
//
// The loop (loop.ts) is surface-agnostic: it talks to a SurfaceDriver for
// observe/act/capture and never imports Playwright or any device tooling
// directly. Concrete drivers live in driver/ (web.ts = Playwright,
// maestro-android.ts = adb+maestro, simctl-ios.ts = xcrun simctl+maestro).
//
// Surface vocabulary is LOCKED with the Go side (P4SurfaceProp, api/
// internal/suite/parse.go): "web" | "android" | "ios" — lowercase strings,
// zero-default ("a plan without surface: is a web plan").
//
// Driver method contract (locked, mirrors plan 04-01 T3):
//   launch(ctx)        — boot/readiness gate before the first step
//   act(step.action)   — perform one gateway decision on the device
//   observe()          — a11y grounding observation (untrusted; loop fences)
//   capture(kind, ctx) — evidence kind "screenshot" | "video"
//   teardown(err?)     — best-effort resource cleanup at session end
//
// Infra failure vocabulary: boot/launch failures surface as typed
// InfraFlakeError (class infra_flake per BUDG/INFRA — the mobile/device layer
// failed before the app under test could be exercised, so the verdict
// carries no signal about app behavior), never raw throws.

import type { ArtifactPaths, Decision, EvidenceRef } from "./loop.js";
import { WebDriver } from "./driver/web.js";
import { AndroidDriver } from "./driver/maestro-android.js";
import { IOSDriver } from "./driver/simctl-ios.js";

/** Locked vocabulary — matches Go P4SurfaceProp / suite parse.go. */
export type Surface = "web" | "android" | "ios";

const SURFACES: readonly Surface[] = ["web", "android", "ios"];

/** Parse/validate a surface string (E2AI_SURFACE value, runs.surface column).
 * Empty/undefined → "web" (zero-default, matches Go parse.go). Unknown →
 * throws — the vocabulary is locked, so an unknown value is a config bug. */
export function parseSurface(raw: string | undefined | null): Surface {
  const s = raw?.trim().toLowerCase();
  if (!s) return "web";
  if ((SURFACES as readonly string[]).includes(s)) return s as Surface;
  throw new SurfaceConfigError(
    `unknown surface ${JSON.stringify(raw)} — must be one of ${SURFACES.join(" | ")}`,
  );
}

/** Paths a driver may write capture artifacts into (run-relative refs). */
export interface CaptureContext {
  paths: ArtifactPaths;
  /** current step index — filenames stay step-<idx>.* for the report walk. */
  idx: number;
}

/** Pre-action observation from the device (a11y grounding; never pixels). */
export interface DriverObservation {
  /** tree (or structured dump) describing the current UI state. */
  a11y: unknown;
}

/** Context a driver receives at launch. */
export interface LaunchContext {
  env: NodeJS.ProcessEnv;
}

/** What act() returns: what it did + evidence refs relative to the run dir. */
export interface ActResult {
  action: unknown;
  evidence: EvidenceRef[];
}

/** The seam. loop.ts consumes exactly this — no Playwright, no adb, no
 * simctl types leak past it. */
export interface SurfaceDriver {
  launch(ctx: LaunchContext): Promise<void>;
  /** step.action = the parsed gateway decision (kind + payload). */
  act(decision: Decision): Promise<ActResult>;
  observe(): Promise<DriverObservation>;
  capture(kind: "screenshot" | "video", ctx: CaptureContext): Promise<EvidenceRef[]>;
  teardown(err?: unknown): Promise<void>;
}

/** Unsupported/invalid configuration at the seam — thrown by parseSurface,
 * selectDriver, and drivers that cannot honor a requested capability. */
export class SurfaceConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "SurfaceConfigError";
  }
}

/** Subtypes under the locked "infra_flake" family (Go: IsInfraFlake — the
 * verdict engine treats every subtype the same). `tool` names the failing
 * layer binary for runbook triage. */
export class InfraFlakeError extends Error {
  readonly failureClass: string;
  readonly tool: string;
  constructor(failureClass: string, tool: string, detail: string | unknown) {
    super(
      `infra_flake (${failureClass}) on ${tool}: ${typeof detail === "string" ? detail : JSON.stringify(detail)}`,
    );
    this.name = "InfraFlakeError";
    this.failureClass = failureClass;
    this.tool = tool;
  }
}

/** Factory: select the concrete driver from the surface string. Called once
 * at loop startup — the returned driver is the loop's only device handle. */
export function selectDriver(surface: Surface, env: NodeJS.ProcessEnv): SurfaceDriver {
  switch (surface) {
    case "web":
      return new WebDriver({ env });
    case "android":
      return new AndroidDriver({ env });
    case "ios":
      return new IOSDriver({ env });
  }
}
