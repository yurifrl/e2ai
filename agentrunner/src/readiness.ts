// readiness.ts — surface-agnostic readiness probes (PLAT-04, T7).
//
// WebView/mobile loops must wait for the device environment to settle before
// capture: animation-settle, network-idle, render-idle (two consecutive stable
// frames — the fallback when no debug bridge exists). This module knows
// NOTHING about surfaces: adb/maestro/simctl/CDP are forbidden imports. Every
// signal is injected — the Android/iOS drivers (T5/T6) supply checkers and
// frame-digest producers from their own tooling.
//
// INFRA_FLAKE vocabulary (locked contract, mirrored in Go
// api/internal/verdict/classify.go): failure class "infra_flake", optionally
// subtype-qualified "infra_flake:<subtype>". Failed probe results carry their
// subtype so drivers can store the class on the step/test_result verbatim;
// anything not typed infra_flake keeps APP_BUG semantics downstream.

/** Canonical INFRA_FLAKE failure class and its documented subtypes. */
export const FAILURE_CLASS = {
  /** Canonical, unqualified: the device environment was unstable. */
  InfraFlake: "infra_flake",
  /** Emulator/simulator never left the boot state within the boot budget. */
  BootTimeout: "infra_flake:boot_timeout",
  /** Device disappeared or is unreachable to its driving channel. */
  DeviceOffline: "infra_flake:device_offline",
  /** Screenshot/capture step failed or produced an unusable image. */
  ScreenshotFailed: "infra_flake:screenshot_failed",
  /** A readiness probe here did not settle within its timeout budget. */
  ProbeTimeout: "infra_flake:probe_timeout",
  /** Frame digest never stabilized across the sampling window. */
  UnstableFrames: "infra_flake:unstable_frames",
} as const;

/** True when the driver-typed class is an infra_flake (optionally qualified). */
export function isInfraFlake(failureClass: string): boolean {
  return (
    failureClass === FAILURE_CLASS.InfraFlake ||
    failureClass.startsWith(`${FAILURE_CLASS.InfraFlake}:`)
  );
}

/** One probe outcome, with the timing evidence a step record can cite. */
export interface ReadinessResult {
  /** Probe name: "animation", "network", "render". */
  name: string;
  ok: boolean;
  /** Wall-clock span of the probe, from the injected `now` clock. */
  elapsedMS: number;
  /** Plain-language evidence: sample counts, what was observed. */
  evidence: string;
  /** Set only on failure: the subtype-qualified infra_flake class. */
  failureClass?: string;
}

/** A full readiness pass over every probe. */
export interface ReadinessReport {
  /** All probes settled. */
  ok: boolean;
  elapsedMS: number;
  results: ReadinessResult[];
}

/** Injectable knobs shared by every probe. */
export interface ProbeOptions {
  /** Overall budget for the probe; the failure subtype is probe_timeout. */
  timeoutMS: number;
  /** Poll cadence (default 250ms). */
  intervalMS?: number;
  /** Injected clock (default Date.now). */
  now?: () => number;
  /** Injected delay (default a real timer). Tests advance the fake clock here. */
  sleep?: (ms: number) => Promise<void>;
}

/** Producer that answers true when the watched condition holds this instant. */
export type SettledChecker = () => boolean | Promise<boolean>;

const defaults = (opts: ProbeOptions) => {
  const now = opts.now ?? Date.now;
  const sleep =
    opts.sleep ??
    ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  return { now, sleep, intervalMS: opts.intervalMS ?? 250 };
};

/**
 * Shared polling loop: wait until `checker` holds for `required` consecutive
 * polls, or fail with the probe_timeout subtype. `sleep` advancing the clock
 * is what makes time pass under the injected-fake test setup.
 */
async function pollUntilStable(
  name: string,
  checker: SettledChecker,
  opts: ProbeOptions,
  required: number,
): Promise<ReadinessResult> {
  const { now, sleep, intervalMS } = defaults(opts);
  const start = now();
  let consecutive = 0;
  let polls = 0;
  for (;;) {
    polls++;
    consecutive = (await checker()) ? consecutive + 1 : 0;
    const elapsed = now() - start;
    if (consecutive >= required) {
      return {
        name,
        ok: true,
        elapsedMS: elapsed,
        evidence: `${polls} sample(s), settled after ${required} consecutive check(s) at ${elapsed}ms`,
      };
    }
    if (elapsed >= opts.timeoutMS) {
      return {
        name,
        ok: false,
        elapsedMS: elapsed,
        evidence: `not settled after ${polls} sample(s) in ${elapsed}ms`,
        failureClass: FAILURE_CLASS.ProbeTimeout,
      };
    }
    await sleep(Math.min(intervalMS, opts.timeoutMS - elapsed));
  }
}

/** Wait until the WebView reports no running animations. */
export function animationSettled(
  checker: SettledChecker,
  opts: ProbeOptions,
): Promise<ReadinessResult> {
  return pollUntilStable("animation", checker, opts, 2);
}

/**
 * Wait until no requests are in flight. `checker` answers true on an idle
 * network stack; two consecutive idle polls guard against request churn.
 */
export function networkIdle(
  checker: SettledChecker,
  opts: ProbeOptions,
): Promise<ReadinessResult> {
  return pollUntilStable("network", checker, opts, 2);
}

/**
 * Render-idle without a debug bridge: two consecutive identical frame digests.
 * `sample` returns a digest (hash/pixel-diff key) of the current frame, or
 * null when the surface cannot produce one this instant (null counts as
 * instability — an absent frame is never "stable").
 */
export async function renderIdle(
  sample: () => string | null,
  opts: ProbeOptions,
): Promise<ReadinessResult> {
  const { now, sleep, intervalMS } = defaults(opts);
  const start = now();
  let previous: string | null = null;
  let samples = 0;
  for (;;) {
    samples++;
    const current = sample();
    const elapsed = now() - start;
    if (current !== null && current === previous) {
      return {
        name: "render",
        ok: true,
        elapsedMS: elapsed,
        evidence: `${samples} frame sample(s), stable at ${elapsed}ms`,
      };
    }
    if (elapsed >= opts.timeoutMS) {
      return {
        name: "render",
        ok: false,
        elapsedMS: elapsed,
        evidence: `frames never stabilized across ${samples} sample(s) in ${elapsed}ms`,
        failureClass: FAILURE_CLASS.UnstableFrames,
      };
    }
    previous = current;
    await sleep(Math.min(intervalMS, opts.timeoutMS - elapsed));
  }
}

/** Outcome of the double-lag guard: capture is valid only when ok. */
export interface StabilityGuard {
  ok: boolean;
  /** Empty when ok; otherwise why the guard rejected the capture. */
  reason: string;
}

/**
 * Double-lag guard (PLAT-04): a capture is trustworthy only if the frame
 * digests sampled immediately before AND after it are identical. A frame that
 * moved on either side means the page was still rendering mid-capture.
 */
export function verifyStabilityAroundCapture(
  sampleBefore: string | null,
  sampleAfter: string | null,
): StabilityGuard {
  if (sampleBefore === null || sampleAfter === null) {
    return {
      ok: false,
      reason: "frame sample unavailable on " +
        (sampleBefore === null && sampleAfter === null
          ? "both sides"
          : sampleBefore === null
          ? "one side (before)"
          : "one side (after)"),
    };
  }
  if (sampleBefore !== sampleAfter) {
    return {
      ok: false,
      reason: "frame changed around the capture (double-lag suspected)",
    };
  }
  return { ok: true, reason: "" };
}

/**
 * Runs a set of readiness probes (the injected producers) and folds them into
 * one report. Probes run sequentially so the evidence stays deterministic.
 */
export class ProbeRunner {
  constructor(
    private readonly probes: ReadonlyArray<() => Promise<ReadinessResult>>,
    private readonly opts: { now?: () => number } = {},
  ) {}

  async run(): Promise<ReadinessReport> {
    const now = this.opts.now ?? Date.now;
    const start = now();
    const results: ReadinessResult[] = [];
    for (const probe of this.probes) {
      results.push(await probe());
    }
    return {
      ok: results.every((r) => r.ok),
      elapsedMS: now() - start,
      results,
    };
  }
}

/** Compact one-line summary of a report, for step-evidence strings. */
export function summarize(report: ReadinessReport): string {
  return report.results
    .map((r) => `${r.name}:${r.ok ? "ok" : `failed(${r.failureClass ?? "?"})`}@${r.elapsedMS}ms`)
    .join("; ");
}
