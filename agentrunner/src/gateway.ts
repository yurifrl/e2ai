// gateway.ts — OpenAI-compatible chat client for the in-house aihub gateway
// (Phase 3 T3; contract verified against aihub README/ADR-0002 in RESEARCH.md).
//
//   POST {AIHUB_BASE_URL}/v1/chat/completions
//   Authorization: Bearer <AIHUB_API_TOKEN>
//   Response carries usage.prompt_tokens / usage.completion_tokens — the
//   per-run cost accumulator source (BUDG-01).
//
// Error mapping (documented non-retryable semantics):
//   402 → PaymentRequiredError (retryable = false — gateway budget spent)
//   429 → RateLimitedError     (retryable = true  — transient throttle)
//   timeout → GatewayTimeoutError (retryable = true)
//   other non-2xx → GatewayError (retryable = status >= 500)

export interface ChatTextMessage {
  role: "system" | "user" | "assistant";
  content: string;
}

export interface Usage {
  tokensIn: number;
  tokensOut: number;
}

export interface ChatResult {
  content: string;
  model: string;
  usage: Usage;
}

export class GatewayError extends Error {
  readonly status: number | null;
  readonly retryable: boolean;
  constructor(message: string, opts: { status?: number | null; retryable?: boolean } = {}) {
    super(message);
    this.name = "GatewayError";
    this.status = opts.status ?? null;
    this.retryable = opts.retryable ?? false;
  }
}

export class PaymentRequiredError extends GatewayError {
  constructor(message: string) {
    super(message, { status: 402, retryable: false });
    this.name = "PaymentRequiredError";
  }
}

export class RateLimitedError extends GatewayError {
  constructor(message: string) {
    super(message, { status: 429, retryable: true });
    this.name = "RateLimitedError";
  }
}

export class GatewayTimeoutError extends GatewayError {
  constructor(message: string) {
    super(message, { status: null, retryable: true });
    this.name = "GatewayTimeoutError";
  }
}

export class GatewayConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "GatewayConfigError";
  }
}

export interface ResolvedGateway {
  /** aihub base URL, no trailing slash. */
  baseUrl: string;
  /** gateway bearer token (gt_-style scoped e2ai service token). */
  apiToken: string;
  /** per-call deadline in ms (default 30_000 — RESEARCH: screenshot+LLM < 30s typical). */
  timeoutMs: number;
  fetchImpl: typeof fetch;
  /** env source for role→model resolution (defaults process.env). */
  env: NodeJS.ProcessEnv;
}

export function gatewayConfigFromEnv(
  env: NodeJS.ProcessEnv = process.env,
  fetchImpl: typeof fetch = fetch,
): ResolvedGateway {
  const baseUrl = required(env, "AIHUB_BASE_URL");
  const apiToken = required(env, "AIHUB_API_TOKEN");
  const timeoutRaw = env["AIHUB_TIMEOUT_MS"];
  const timeoutMs = timeoutRaw ? Number(timeoutRaw) : 30_000;
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    throw new GatewayConfigError(`AIHUB_TIMEOUT_MS must be a positive number, got ${timeoutRaw}`);
  }
  return { baseUrl: baseUrl.replace(/\/+$/, ""), apiToken, timeoutMs, fetchImpl, env };
}

function required(env: NodeJS.ProcessEnv, key: string): string {
  const v = env[key];
  if (!v) throw new GatewayConfigError(`missing required env ${key}`);
  return v;
}

/** Role → model map: `E2AI_AGENT_MODEL_<ROLE>` (e.g. navigator → E2AI_AGENT_MODEL_NAVIGATOR). */
export function modelForRole(role: string, env: NodeJS.ProcessEnv = process.env): string {
  const key = `E2AI_AGENT_MODEL_${role.toUpperCase()}`;
  const model = env[key];
  if (!model) throw new GatewayConfigError(`missing ${key} for role ${role}`);
  return model;
}

export interface ChatOptions {
  temperature?: number;
  maxTokens?: number;
}

/** One OpenAI-compatible chat completion call for the given roster role. */
export async function chat(
  gw: ResolvedGateway,
  role: string,
  messages: readonly ChatTextMessage[],
  opts: ChatOptions = {},
): Promise<ChatResult> {
  const model = modelForRole(role, gw.env);
  const body: Record<string, unknown> = { model, messages };
  if (opts.temperature !== undefined) body["temperature"] = opts.temperature;
  if (opts.maxTokens !== undefined) body["max_tokens"] = opts.maxTokens;

  let res: Response;
  try {
    res = await gw.fetchImpl(`${gw.baseUrl}/v1/chat/completions`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        authorization: `Bearer ${gw.apiToken}`,
      },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(gw.timeoutMs),
    });
  } catch (err) {
    if (err instanceof Error && (err.name === "TimeoutError" || err.name === "AbortError")) {
      throw new GatewayTimeoutError(`gateway call timed out after ${gw.timeoutMs}ms`);
    }
    throw new GatewayError(`gateway transport failure: ${err instanceof Error ? err.message : String(err)}`, {
      retryable: true,
    });
  }

  if (res.status === 402) throw new PaymentRequiredError("gateway budget: payment required (402)");
  if (res.status === 429) throw new RateLimitedError("gateway rate limited (429)");
  if (!res.ok) {
    throw new GatewayError(`gateway returned ${res.status}`, { status: res.status, retryable: res.status >= 500 });
  }

  const parsed = (await res.json()) as {
    choices?: Array<{ message?: { content?: string } }>;
    usage?: { prompt_tokens?: number; completion_tokens?: number };
    model?: string;
  };
  const content = parsed.choices?.[0]?.message?.content;
  if (typeof content !== "string") {
    throw new GatewayError("gateway response missing choices[0].message.content");
  }
  const promptTokens = parsed.usage?.prompt_tokens;
  const completionTokens = parsed.usage?.completion_tokens;
  if (typeof promptTokens !== "number" || typeof completionTokens !== "number") {
    // usage is the BUDG-01 cost source — a response without it is unusable.
    throw new GatewayError("gateway response missing usage.prompt_tokens/completion_tokens");
  }
  return {
    content,
    model: parsed.model ?? model,
    usage: { tokensIn: promptTokens, tokensOut: completionTokens },
  };
}
