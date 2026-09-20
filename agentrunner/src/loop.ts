// loop.ts — the agentic loop skeleton (Phase 3 T3):
//   capture (Playwright a11y snapshot + screenshot → artifacts dir)
//   → decide (gateway call producing a JSON action)
//   → act (executor injected — interface only in this task; T5 lands the
//     locator-first navigator)
//   → verify (optional injected verifier; T6 lands the vision judge)
//   → append step via api.ts → heartbeat (409 ⇒ KilledError ⇒ clean exit).
//
// Evidence artifacts land under $E2AI_ARTIFACTS_DIR/<run id>/agents/<session
// id>/ — the SAME root layout as deterministic runs (suite/execute.go mounts
// ArtifactsDir/<run id> at /artifacts and walks it for artifact rows), so the
// report reuses them. Evidence refs are recorded RELATIVE to the run dir.
//
// Prompt-injection defense (SAFE-01): untrusted page data is wrapped in
// explicit hardened data-fence blocks via buildEvidenceFence (fences.ts).

import { mkdir, writeFile } from "node:fs/promises";
import { join, relative } from "node:path";
import type { Page } from "playwright";
import type { Usage } from "./gateway.js";
import { chat, type ResolvedGateway } from "./gateway.js";
import type { AgentStep, AgentApi } from "./api.js";
import { Budget, BudgetExhaustedError } from "./budget.js";
import { buildEvidenceFence } from "./fences.js";
import { ApiError, KilledError } from "./api.js";

export class LoopConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "LoopConfigError";
  }
}

export interface EvidenceRef {
  /** artifact kind as stored on artifacts rows (screenshot, a11y, …). */
  kind: string;
  /** path relative to the run's artifacts dir. */
  path: string;
}

export interface ArtifactPaths {
  runDir: string;
  agentDir: string;
  screenshotsDir: string;
  a11yDir: string;
}

/** Same-root layout as deterministic runs: artifactsDir/<run id>/agents/<session id>/. */
export function artifactPaths(artifactsDir: string, runId: string, sessionId: string): ArtifactPaths {
  const runDir = join(artifactsDir, runId);
  const agentDir = join(runDir, "agents", sessionId);
  return {
    runDir,
    agentDir,
    screenshotsDir: join(agentDir, "screenshots"),
    a11yDir: join(agentDir, "a11y"),
  };
}

export interface CaptureResult {
  /** Playwright accessibility snapshot tree (a11y grounding, never pixels). */
  a11y: unknown;
  evidence: EvidenceRef[];
}

/** Capture pre-action evidence: a11y snapshot + screenshot, both persisted. */
export async function capture(page: Page, paths: ArtifactPaths, idx: number): Promise<CaptureResult> {
  await mkdir(paths.screenshotsDir, { recursive: true });
  await mkdir(paths.a11yDir, { recursive: true });

  // mode:"ai" emits element refs ([ref=eN]) — the refs locator-first
  // navigation (T5) consumes. Page data: untrusted until fenced.
  const snapshot = await page.ariaSnapshotJSON({ mode: "ai" });
  const a11yPath = join(paths.a11yDir, `step-${idx}.json`);
  await writeFile(a11yPath, JSON.stringify(snapshot, null, 2), "utf8");

  const screenshotPath = join(paths.screenshotsDir, `step-${idx}.png`);
  await page.screenshot({ path: screenshotPath });

  return {
    a11y: snapshot,
    evidence: [
      { kind: "a11y", path: relative(paths.runDir, a11yPath) },
      { kind: "screenshot", path: relative(paths.runDir, screenshotPath) },
    ],
  };
}

/** The decision the agent produced from the capture. `kind` maps to the
 * session_steps kind vocabulary; extra keys are decision payload. */
export interface Decision {
  kind: "navigate" | "act" | "observe" | "judge" | "assert" | "error" | "done";
  [key: string]: unknown;
}

/** Parse the gateway's JSON action out of its content (tolerates a fenced
 * ```json code block). Throws LoopConfigError on unparseable output. */
export function parseDecision(content: string): Decision {
  const fenced = /```(?:json)?\s*([\s\S]*?)```/.exec(content);
  const raw = (fenced?.[1] ?? content).trim();
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new LoopConfigError(`gateway returned non-JSON decision: ${content.slice(0, 200)}`);
  }
  if (typeof parsed !== "object" || parsed === null || !("kind" in parsed)) {
    throw new LoopConfigError("gateway decision missing `kind`");
  }
  return parsed as Decision;
}

export interface StepExecutor {
  /** Perform the decided action on the page. Locator-first policy lives in
   * the T5 navigator; evidence refs are relative to the run dir. */
  act(page: Page, decision: Decision, ctx: { runDir: string; idx: number }): Promise<{ action: unknown; evidence: EvidenceRef[] }>;
}

export interface VerifyInput {
  decision: Decision;
  action: unknown;
  evidence: EvidenceRef[];
}

export interface StepVerifier {
  /** Judge the performed step against the corpus oracle (T6 vision judge:
   * deterministic assertions first, then VLM with evidence citations). */
  verify(input: VerifyInput): Promise<unknown>;
}

export interface LoopDeps {
  page: Page;
  gateway: ResolvedGateway;
  api: AgentApi;
  budget: Budget;
  /** roster role — also drives role→model resolution in the gateway. */
  role: string;
  sessionId: string;
  runId: string;
  artifactsDir: string;
  executor: StepExecutor;
  verify?: StepVerifier;
  systemPrompt: string;
  /** approved plan steps for this session; step i is presented at idx i. */
  planSteps?: readonly unknown[];
  /** max safety bound on steps even when the ceiling is unlimited. */
  hardStepBound?: number;
}

export interface StepContext {
  idx: number;
  decision: Decision;
  usage: Usage;
  costMillicents: number;
}

export async function runLoop(deps: LoopDeps): Promise<void> {
  const { page, gateway, api, budget, sessionId, runId, artifactsDir } = deps;
  const paths = artifactPaths(artifactsDir, runId, sessionId);
  const hardBound = deps.hardStepBound ?? 10_000;

  for (let idx = 0; idx < hardBound; idx++) {
    budget.beginStep();
    // Heartbeat before long actions (screenshot + LLM), per RESEARCH loop skeleton.
    await api.heartbeat(sessionId);

    const cap = await capture(page, paths, idx);

    const planStep = deps.planSteps?.[idx];
    const userMessage = [
      `Plan step ${idx}: ${JSON.stringify(planStep ?? null)}`,
      buildEvidenceFence("a11y_snapshot", cap.a11y),
    ].join("\n\n");

    const result = await chat(gateway, deps.role, [
      { role: "system", content: deps.systemPrompt },
      { role: "user", content: userMessage },
    ]);
    const decision = parseDecision(result.content);

    if (decision.kind === "done") {
      const cost = budget.recordStep(result.model, result.usage);
      await appendStep(api, sessionId, {
        idx,
        kind: "observe",
        decision: { ...decision, verify: null },
        action: { done: true },
        evidence: cap.evidence,
        usage: result.usage,
        costMillicents: cost,
      });
      return;
    }

    const performed = await deps.executor.act(page, decision, { runDir: paths.runDir, idx });

    let verdict: unknown = null;
    if (deps.verify) {
      verdict = await deps.verify.verify({ decision, action: performed.action, evidence: performed.evidence });
    }

    const cost = budget.recordStep(result.model, result.usage);
    await appendStep(api, sessionId, {
      idx,
      kind: decision.kind,
      decision: { ...decision, verify: verdict },
      action: performed.action,
      evidence: [...cap.evidence, ...performed.evidence],
      usage: result.usage,
      costMillicents: cost,
    });
  }
  throw new BudgetExhaustedError(`hard step bound ${hardBound} reached without a done decision`);
}

async function appendStep(
  api: AgentApi,
  sessionId: string,
  step: {
    idx: number;
    kind: AgentStep["kind"];
    decision: unknown;
    action: unknown;
    evidence: EvidenceRef[];
    usage: Usage;
    costMillicents: number;
  },
): Promise<void> {
  await api.appendStep(sessionId, {
    idx: step.idx,
    kind: step.kind,
    decision: step.decision,
    action: step.action,
    evidence: step.evidence,
    tokensIn: step.usage.tokensIn,
    tokensOut: step.usage.tokensOut,
    costMillicents: step.costMillicents,
  });
}

export { ApiError, KilledError };
export { BudgetExhaustedError };
