// driver/web.ts — WebDriver: the Playwright implementation of the seam.
//
// Extracted verbatim from loop.ts (Phase 4 T3 refactor): identical Playwright
// calls, identical evidence shapes — a11y snapshot via ariaSnapshotJSON
// (mode:"ai" emits element refs for the T5 locator-first navigator), PNG
// screenshots under <runDir>/agents/<sessionId>/screenshots/, run-relative
// evidence refs. The loop persists the observation JSON itself (surface-
// agnostic plumbing), so this driver owns only the page interactions.
//
// Behavior parity contract: no web behavior changed in this refactor. The
// locator-first navigator (T5) will grow `act`; today it covers the decision
// vocabulary the gateway already emits with plain Playwright calls.

import { mkdir } from "node:fs/promises";
import { join, relative } from "node:path";
import type { Page } from "playwright";
import type { Decision, EvidenceRef } from "../loop.js";
import { assertNavigationAllowed } from "../sandbox.js";
import {
  SurfaceConfigError,
  type ActResult,
  type CaptureContext,
  type DriverObservation,
  type LaunchContext,
  type SurfaceDriver,
} from "../surface.js";

export interface WebDriverDeps {
  /** Optional until T5 moves browser launch into this driver: selectDriver
   * constructs a page-less web driver, and the session wiring (or tests)
   * attach the page. Access before attach throws SurfaceConfigError. */
  page?: Page;
  /** Egress allowlist env for agent-driven navigations (SAFE-01). */
  env?: NodeJS.ProcessEnv;
}

export class WebDriver implements SurfaceDriver {
  constructor(private readonly deps: WebDriverDeps) {}

  private page(): Page {
    if (!this.deps.page) {
      throw new SurfaceConfigError(
        "web driver has no Playwright page — attach a page before observe/act/capture",
      );
    }
    return this.deps.page;
  }

  /** No-op: the Playwright browser/page lifecycle is owned by the session
   * wiring that constructs this driver and hands it to the loop (T5 moves
   * browser launch here). */
  async launch(_ctx: LaunchContext): Promise<void> {}

  /** Pre-action observation: the Playwright a11y snapshot tree (a11y
   * grounding, never pixels). Untrusted — the loop fences it. */
  async observe(): Promise<DriverObservation> {
    // mode:"ai" emits element refs ([ref=eN]) — the refs locator-first
    // navigation (T5) consumes. Page data: untrusted until fenced.
    return { a11y: await this.page().ariaSnapshotJSON({ mode: "ai" }) };
  }

  async capture(kind: "screenshot", ctx: CaptureContext): Promise<EvidenceRef[]>;
  async capture(kind: "video", ctx: CaptureContext): Promise<EvidenceRef[]>;
  async capture(kind: "screenshot" | "video", ctx: CaptureContext): Promise<EvidenceRef[]> {
    if (kind === "video") {
      throw new SurfaceConfigError(
        "web video capture requires context-level Playwright recording — wire it where the browser context is created (T5/T6)",
      );
    }
    await mkdir(ctx.paths.screenshotsDir, { recursive: true });
    const screenshotPath = join(ctx.paths.screenshotsDir, `step-${ctx.idx}.png`);
    await this.page().screenshot({ path: screenshotPath });
    return [{ kind: "screenshot", path: relative(ctx.paths.runDir, screenshotPath) }];
  }

  /** Scaffold action mapping pending the T5 locator-first navigator: plain
   * Playwright calls for the decision kinds that mutate the page. Egress
   * allowlist is enforced in-process on every navigate (SAFE-01). */
  async act(decision: Decision): Promise<ActResult> {
    const page = this.page();
    switch (decision.kind) {
      case "navigate": {
        const url = String(decision.url ?? "");
        assertNavigationAllowed(url, this.deps.env ?? process.env);
        await page.goto(url);
        return { action: { navigated: url }, evidence: [] };
      }
      case "act": {
        if (typeof decision.text === "string" && typeof decision.selector === "string") {
          await page.locator(decision.selector).fill(decision.text);
          return { action: { filled: decision.selector }, evidence: [] };
        }
        if (typeof decision.selector === "string") {
          await page.locator(decision.selector).click();
          return { action: { clicked: decision.selector }, evidence: [] };
        }
        throw new SurfaceConfigError(`unsupported web action decision: ${JSON.stringify(decision)}`);
      }
      default:
        // observe/judge/assert carry no page mutation until T5/T6 land.
        return { action: { kind: decision.kind, noop: true }, evidence: [] };
    }
  }

  /** No-op: page teardown is owned by the session wiring (T5). */
  async teardown(_err?: unknown): Promise<void> {}
}
