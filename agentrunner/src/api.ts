// api.ts — agent-api client against the Go control plane (Phase 3 T2 HTTP
// surface). Endpoints: POST /api/agent-sessions/{id}/heartbeat|steps|complete|fail.
// Phase 5 wave 1 adds agent-facing reads: GET /api/agent-sessions/{id}/plan
// (approved plan steps) and GET /api/agent/corpus/behaviors?app_id=&key=
// (the corpus oracle the vision judge verifies against). Both runner-token
// gated; payload shapes are validated on arrival — malformed JSON from the
// control plane is an ApiError, never silently trusted.
//
// Phase 5 T7 (FLAK-02) adds POST /api/agent-sessions/{id}/repair-proposals:
// the navigator's selector-repair proposal intake (report-only, additive).
//
// Auth: `Authorization: Bearer <E2AI_RUNNER_SERVICE_TOKEN>` (st_-style
// runner service token path in httpx).
// 409 on ANY call → KilledError: the session was killed (kill switch,
// BUDG-02); the loop must exit immediately and cleanly (clean exit
// semantics — no retry, no fail call, exit code carries the reason).

import type { RepairSuggestion } from "./repair.js";

export class KilledError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "KilledError";
  }
}

export class ApiError extends Error {
  readonly status: number;
  readonly retryable: boolean;
  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryable = status >= 500;
  }
}

export class ApiConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ApiConfigError";
  }
}

export interface ApiConfig {
  /** control-plane base URL (E2AI_API_BASE_URL), no trailing slash. */
  baseUrl: string;
  /** runner service bearer token (E2AI_RUNNER_SERVICE_TOKEN). */
  serviceToken: string;
  timeoutMs: number;
  fetchImpl: typeof fetch;
}

export function apiConfigFromEnv(env: NodeJS.ProcessEnv = process.env, fetchImpl: typeof fetch = fetch): ApiConfig {
  const baseUrl = env["E2AI_API_BASE_URL"];
  const serviceToken = env["E2AI_RUNNER_SERVICE_TOKEN"];
  if (!baseUrl) throw new ApiConfigError("missing required env E2AI_API_BASE_URL");
  if (!serviceToken) throw new ApiConfigError("missing required env E2AI_RUNNER_SERVICE_TOKEN");
  const timeoutRaw = env["E2AI_API_TIMEOUT_MS"];
  const timeoutMs = timeoutRaw ? Number(timeoutRaw) : 15_000;
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    throw new ApiConfigError(`E2AI_API_TIMEOUT_MS must be a positive number, got ${timeoutRaw}`);
  }
  return { baseUrl: baseUrl.replace(/\/+$/, ""), serviceToken, timeoutMs, fetchImpl };
}

/** One step of the approved plan — JSON encoding of Go agents.planStepDTO
 * (runner_data.go): snake_case keys on the wire (`number`, `intent`, `judge`,
 * `uses`, `on_failure`). `uses` names corpus behavior keys; `judge` is the
 * falsifiable expectation the vision judge verifies; `on_failure` mirrors the
 * plan parser's stop|continue enum. */
export interface PlanStep {
  number: number;
  intent: string;
  judge: string;
  uses: string[];
  on_failure: string;
}

/** GET /api/agent-sessions/{id}/plan payload — app_id travels here (the
 * judge's corpus queries key off it; no env var for it ever exists). */
export interface PlanPayload {
  app_id: string;
  plan_id: string;
  steps: PlanStep[];
}

/** GET /api/agent/corpus/behaviors?app_id=&key= payload. `content` is the
 * behavior's jsonb oracle; `status` gates trust (non-verified oracles are
 * INCONCLUSIVE by definition — see judge.ts). */
export interface BehaviorPayload {
  app_id: string;
  key: string;
  status: string;
  content: unknown;
  provenance: unknown;
}

function planStepGuard(v: unknown): v is PlanStep {
  if (typeof v !== "object" || v === null) return false;
  const s = v as Record<string, unknown>;
  return (
    typeof s["number"] === "number" &&
    typeof s["intent"] === "string" &&
    typeof s["judge"] === "string" &&
    Array.isArray(s["uses"]) &&
    (s["uses"] as unknown[]).every((u) => typeof u === "string") &&
    typeof s["on_failure"] === "string"
  );
}

function parsePlanPayload(body: unknown): PlanPayload {
  if (typeof body !== "object" || body === null) {
    throw new ApiError("plan payload is not an object", 0);
  }
  const p = body as Record<string, unknown>;
  if (typeof p["app_id"] !== "string" || typeof p["plan_id"] !== "string" || !Array.isArray(p["steps"])) {
    throw new ApiError("plan payload missing app_id/plan_id/steps", 0);
  }
  const steps = p["steps"] as unknown[];
  const bad = steps.findIndex((s) => !planStepGuard(s));
  if (bad >= 0) {
    throw new ApiError(`plan payload step ${bad} failed the PlanStep shape`, 0);
  }
  return { app_id: p["app_id"], plan_id: p["plan_id"], steps: steps as PlanStep[] };
}

function parseBehaviorPayload(body: unknown): BehaviorPayload {
  if (typeof body !== "object" || body === null) {
    throw new ApiError("behavior payload is not an object", 0);
  }
  const b = body as Record<string, unknown>;
  if (typeof b["app_id"] !== "string" || typeof b["key"] !== "string" || typeof b["status"] !== "string") {
    throw new ApiError("behavior payload missing app_id/key/status", 0);
  }
  return { app_id: b["app_id"], key: b["key"], status: b["status"], content: b["content"], provenance: b["provenance"] };
}

/** Stored repair-proposal row — JSON encoding of Go agents.repairProposalWireDTO
 * (snake_case on the wire; decided fields null until a human decides). */
export interface RepairProposalPayload {
  id: string;
  learning_id: string;
  app_id: string;
  behavior_key: string;
  broken_selector: string;
  proposed_selector: string;
  rationale: string;
  confidence: number;
  evidence_ref: string;
  status: string;
  proposed_by: string;
  decided_by: string | null;
  created_at: string;
}

function parseRepairProposalPayload(body: unknown): RepairProposalPayload {
  if (typeof body !== "object" || body === null) {
    throw new ApiError("repair proposal payload is not an object", 0);
  }
  const p = body as Record<string, unknown>;
  const required = ["id", "learning_id", "app_id", "behavior_key", "broken_selector", "proposed_selector", "rationale", "status", "proposed_by", "created_at"] as const;
  const bad = required.find((k) => typeof p[k] !== "string");
  if (bad !== undefined || typeof p["confidence"] !== "number") {
    throw new ApiError("repair proposal payload missing required fields", 0);
  }
  return {
    id: p["id"] as string,
    learning_id: p["learning_id"] as string,
    app_id: p["app_id"] as string,
    behavior_key: p["behavior_key"] as string,
    broken_selector: p["broken_selector"] as string,
    proposed_selector: p["proposed_selector"] as string,
    rationale: p["rationale"] as string,
    confidence: p["confidence"] as number,
    evidence_ref: typeof p["evidence_ref"] === "string" ? p["evidence_ref"] : "",
    status: p["status"] as string,
    proposed_by: p["proposed_by"] as string,
    decided_by: typeof p["decided_by"] === "string" ? p["decided_by"] : null,
    created_at: p["created_at"] as string,
  };
}

/** One appended trajectory row — column semantics from 0004_agents.sql session_steps:
 * idx (gapless per-session sequence), kind (navigate|act|observe|judge|assert|error),
 * decision/action/evidence jsonb, tokens_in/out, cost_millicents. Append-only. */
export interface AgentStep {
  idx: number;
  kind: "navigate" | "act" | "observe" | "judge" | "assert" | "error";
  decision: unknown;
  action: unknown;
  evidence: unknown;
  tokensIn: number;
  tokensOut: number;
  costMillicents: number;
}

export class AgentApi {
  private readonly cfg: ApiConfig;

  constructor(cfg: ApiConfig) {
    this.cfg = cfg;
  }

  /** Keep the session alive; staleness (90s lapse) drives orphan sweep. */
  async heartbeat(sessionId: string): Promise<void> {
    await this.post(`/api/agent-sessions/${sessionId}/heartbeat`);
  }

  /** Fetch the session's approved plan (Phase 5 wave 1 contract, mounted by
   * api/internal/agents — runner-token gated like heartbeat/append). The
   * control plane resolves session → run → approved plan_version and
   * returns steps verbatim from agents.planStepDTO ({number, intent, judge,
   * uses, on_failure} — snake_case on the wire). */
  async getPlan(sessionId: string): Promise<PlanPayload> {
    const body = await this.get(`/api/agent-sessions/${sessionId}/plan`);
    return parsePlanPayload(body);
  }

  /** Fetch one corpus behavior oracle (Phase 5 wave 1: GET
   * /api/agent/corpus/behaviors?app_id=&key=, runner-token gated). Content is
   * the behavior's jsonb oracle; status gates trust (non-verified oracles are
   * INCONCLUSIVE by definition). */
  async getBehavior(appId: string, key: string): Promise<BehaviorPayload> {
    const path = `/api/agent/corpus/behaviors?app_id=${encodeURIComponent(appId)}&key=${encodeURIComponent(key)}`;
    const body = await this.get(path);
    return parseBehaviorPayload(body);
  }

  /** Append one trajectory step. Body matches the session_steps DDL columns. */
  async appendStep(sessionId: string, step: AgentStep): Promise<void> {
    await this.post(`/api/agent-sessions/${sessionId}/steps`, {
      idx: step.idx,
      kind: step.kind,
      decision: step.decision ?? {},
      action: step.action ?? {},
      evidence: step.evidence ?? [],
      tokens_in: step.tokensIn,
      tokens_out: step.tokensOut,
      cost_millicents: step.costMillicents,
    });
  }

  /** FLAK-02: submit a selector-repair proposal for human review (report-only;
   * the control plane owns the refuse-to-heal re-check and returns 422 below
   * the floor). Body mirrors Go agents.repairProposalRequest, response the
   * stored proposal row (agents.repairProposalWireDTO). Callers gate through
   * repair.healDecision first — this method performs no local validation. */
  async proposeRepair(sessionId: string, suggestion: RepairSuggestion): Promise<RepairProposalPayload> {
    const body = await this.postJSON(`/api/agent-sessions/${sessionId}/repair-proposals`, {
      behavior_key: suggestion.behaviorKey,
      broken_selector: suggestion.brokenSelector,
      proposed_selector: suggestion.proposedSelector,
      rationale: suggestion.rationale,
      confidence: suggestion.confidence,
      evidence_ref: suggestion.evidenceRef ?? "",
    });
    return parseRepairProposalPayload(body);
  }

  async complete(sessionId: string): Promise<void> {
    await this.post(`/api/agent-sessions/${sessionId}/complete`);
  }

  async fail(sessionId: string, errorNote: string): Promise<void> {
    await this.post(`/api/agent-sessions/${sessionId}/fail`, { error_note: errorNote });
  }

  private async post(path: string, body?: unknown): Promise<void> {

    let res: Response;
    try {
      res = await this.cfg.fetchImpl(`${this.cfg.baseUrl}${path}`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          authorization: `Bearer ${this.cfg.serviceToken}`,
        },
        body: JSON.stringify(body ?? {}),
        signal: AbortSignal.timeout(this.cfg.timeoutMs),
      });
    } catch (err) {
      throw new ApiError(`agent api transport failure on ${path}: ${err instanceof Error ? err.message : String(err)}`, 0);
    }
    if (res.status === 409) {
      // Session killed mid-flight: clean exit — never retried, never failed-over.
      throw new KilledError(`session killed: ${path} rejected with 409`);
    }
    if (!res.ok) {
      throw new ApiError(`agent api ${path} returned ${res.status}`, res.status);
    }
  }

  /** Same transport/auth/409 contract as post, but decodes a JSON body. */
  private async postJSON(path: string, body: unknown): Promise<unknown> {
    let res: Response;
    try {
      res = await this.cfg.fetchImpl(`${this.cfg.baseUrl}${path}`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          authorization: `Bearer ${this.cfg.serviceToken}`,
        },
        body: JSON.stringify(body ?? {}),
        signal: AbortSignal.timeout(this.cfg.timeoutMs),
      });
    } catch (err) {
      throw new ApiError(`agent api transport failure on ${path}: ${err instanceof Error ? err.message : String(err)}`, 0);
    }
    if (res.status === 409) {
      throw new KilledError(`session killed: ${path} rejected with 409`);
    }
    if (!res.ok) {
      throw new ApiError(`agent api ${path} returned ${res.status}`, res.status);
    }
    try {
      return await res.json();
    } catch (err) {
      throw new ApiError(`agent api ${path} returned non-JSON body: ${err instanceof Error ? err.message : String(err)}`, 0);
    }
  }

  /** Same transport/auth/409 contract as post, but decodes a JSON body. */
  private async get(path: string): Promise<unknown> {
    let res: Response;
    try {
      res = await this.cfg.fetchImpl(`${this.cfg.baseUrl}${path}`, {
        method: "GET",
        headers: { authorization: `Bearer ${this.cfg.serviceToken}` },
        signal: AbortSignal.timeout(this.cfg.timeoutMs),
      });
    } catch (err) {
      throw new ApiError(`agent api transport failure on ${path}: ${err instanceof Error ? err.message : String(err)}`, 0);
    }
    if (res.status === 409) {
      throw new KilledError(`session killed: ${path} rejected with 409`);
    }
    if (!res.ok) {
      throw new ApiError(`agent api ${path} returned ${res.status}`, res.status);
    }
    try {
      return await res.json();
    } catch (err) {
      throw new ApiError(`agent api ${path} returned non-JSON body: ${err instanceof Error ? err.message : String(err)}`, 0);
    }
  }
}
