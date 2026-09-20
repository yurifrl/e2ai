// api.ts — agent-api client against the Go control plane (Phase 3 T2 HTTP
// surface). Endpoints: POST /api/agent-sessions/{id}/heartbeat|steps|complete|fail.
//
// Auth: `Authorization: Bearer <E2AI_RUNNER_SERVICE_TOKEN>` (st_-style
// runner service token path in httpx).
// 409 on ANY call → KilledError: the session was killed (kill switch,
// BUDG-02); the loop must exit immediately and cleanly (clean exit
// semantics — no retry, no fail call, exit code carries the reason).

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
}
