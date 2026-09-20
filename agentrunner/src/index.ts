// index.ts — agentrunner public surface (Phase 3 T3 scaffold).
//
// The agent loop runs one ephemeral process per agent session (ARCHITECTURE
// Pattern 1): state lives in Postgres (agent_sessions/session_steps), the
// process only talks to the gateway (brains) and the control-plane API
// (trajectory + heartbeats), and dies with its session.
//
// Env contract (T2/T4 produce, T3 consumes — keep in sync with README):
//   AIHUB_BASE_URL, AIHUB_API_TOKEN      — gateway client (gateway.ts)
//   AIHUB_TIMEOUT_MS                     — optional, default 30000
//   E2AI_AGENT_MODEL_<ROLE>              — role→model map (gateway.ts)
//   E2AI_API_BASE_URL, E2AI_RUNNER_SERVICE_TOKEN — agent-api client (api.ts)
//   E2AI_API_TIMEOUT_MS                  — optional, default 15000
//   E2AI_SESSION_ID, E2AI_RUN_ID         — session identity (this file)
//   E2AI_ARTIFACTS_DIR                   — evidence artifacts root
//   E2AI_STEP_CEILING, E2AI_COST_CEILING_MILLICENTS — budget limits (0 = unlimited)
//   E2AI_PRICE_<MODEL>_{IN,OUT}_MILLICENTS — per-token price table (budget.ts)
//   E2AI_STAGING_BASE_URL                — page-nav allowlist, comma-separated (sandbox.ts)
//   E2AI_ENV=dev|development|local       — allow http loopback navigation (sandbox.ts)
//
// Exit codes (T4 river worker maps them to complete/fail):
//   0 completed · 1 failed · 2 killed · 3 budget_exhausted

export {
  GatewayError,
  PaymentRequiredError,
  RateLimitedError,
  GatewayTimeoutError,
  GatewayConfigError,
  gatewayConfigFromEnv,
  modelForRole,
  chat,
} from "./gateway.js";
export type { ChatResult, ChatTextMessage, ResolvedGateway, Usage, ChatOptions } from "./gateway.js";

export {
  BudgetExhaustedError,
  MissingPriceError,
  Budget,
  budgetLimitsFromEnv,
  priceTableFromEnv,
  priceKey,
  stepCostMillicents,
} from "./budget.js";
export type { BudgetLimits, PriceTable, ModelPrice } from "./budget.js";

export { AgentApi, ApiError, KilledError, ApiConfigError, apiConfigFromEnv } from "./api.js";
export type { AgentStep, ApiConfig } from "./api.js";

export {
  runLoop,
  capture,
  artifactPaths,
  parseDecision,
  LoopConfigError,
} from "./loop.js";
export type { Decision, EvidenceRef, ArtifactPaths, CaptureResult, LoopDeps, StepContext, StepExecutor, StepVerifier, VerifyInput } from "./loop.js";

export { Sandbox, SandboxConfigError, EgressViolation, sandboxOptionsFromEnv, assertNavigationAllowed, originOf, assertNoAppCredentials, isCredentialShapedKey } from "./sandbox.js";
export type { SandboxOptions } from "./sandbox.js";
export { buildEvidenceFence, sanitizeFencePayload, EVIDENCE_FENCE_REMINDER } from "./fences.js";
export type { EvidenceKind } from "./fences.js";

import { ApiConfigError, KilledError } from "./api.js";
import { BudgetExhaustedError } from "./budget.js";
import { assertNoAppCredentials } from "./sandbox.js";

/** Maps terminal errors to process exit codes consumed by the T4 river
 * worker (KilledError → killed=2, BudgetExhaustedError → budget_exhausted=3,
 * anything else → failed=1). */
export function exitCodeFor(err: unknown): number {
  if (err instanceof KilledError) return 2;
  if (err instanceof BudgetExhaustedError) return 3;
  return 1;
}

/** Session identity + artifact root required for any loop run. */
export interface SessionEnv {
  sessionId: string;
  runId: string;
  artifactsDir: string;
}

export function sessionEnvFromEnv(env: NodeJS.ProcessEnv = process.env): SessionEnv {
  const sessionId = env["E2AI_SESSION_ID"];
  const runId = env["E2AI_RUN_ID"];
  const artifactsDir = env["E2AI_ARTIFACTS_DIR"];
  if (!sessionId) throw new ApiConfigError("missing required env E2AI_SESSION_ID");
  if (!runId) throw new ApiConfigError("missing required env E2AI_RUN_ID");
  if (!artifactsDir) throw new ApiConfigError("missing required env E2AI_ARTIFACTS_DIR");
  return { sessionId, runId, artifactsDir };
}

// Entry-point guard: running `node dist/index.js` directly is a config error
// — no executor is wired yet (the locator-first navigator lands in T5 via
// runLoop's injected StepExecutor). T4's river worker spawns the role-specific
// entry that T5/T6 provide.
if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  // Boot gate (SAFE-01): refuse to start if the environment carries any
  // app-secret-shaped variable — the container must only ever see the
  // E2AI_/AIHUB_-scoped runner tokens. Runs BEFORE any loop entry wiring.
  try {
    assertNoAppCredentials(process.env);
  } catch (err) {
    console.error(`agentrunner: boot refused: ${err instanceof Error ? err.message : err}`);
    process.exit(1);
  }
  console.error(
    "agentrunner: no role entry wired yet — spawn the role-specific entry (navigator lands in T5); this module is the library surface.",
  );
  process.exit(1);
}
