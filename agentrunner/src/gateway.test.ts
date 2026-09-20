// gateway.test.ts — fake OpenAI-compatible gateway server (NO network).
import test from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import {
  chat,
  gatewayConfigFromEnv,
  modelForRole,
  GatewayConfigError,
  GatewayTimeoutError,
  PaymentRequiredError,
  RateLimitedError,
  type ResolvedGateway,
} from "./gateway.js";

interface Captured {
  method: string;
  url: string;
  auth: string | null;
  body: Record<string, unknown>;
}

function startGateway(
  handler: (req: http.IncomingMessage, body: string) => { status: number; json: unknown },
): Promise<{ url: string; captured: Captured[]; close: () => Promise<void> }> {
  const captured: Captured[] = [];
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks).toString("utf8");
      captured.push({
        method: req.method ?? "",
        url: req.url ?? "",
        auth: req.headers["authorization"] ?? null,
        body: body ? (JSON.parse(body) as Record<string, unknown>) : {},
      });
      const { status, json } = handler(req, body);
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(json));
    });
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      const port = typeof addr === "object" && addr ? addr.port : 0;
      resolve({
        url: `http://127.0.0.1:${port}`,
        captured,
        close: () => new Promise((r) => server.close(() => r())),
      });
    });
  });
}

function okCompletion(model = "test-model") {
  return {
    choices: [{ message: { content: '{"kind":"act"}' } }],
    usage: { prompt_tokens: 100, completion_tokens: 20 },
    model,
  };
}

function gw(url: string, env: Record<string, string | undefined> = {}, timeoutMs = 5000): ResolvedGateway {
  return {
    ...gatewayConfigFromEnv({ AIHUB_BASE_URL: url, AIHUB_API_TOKEN: "gt_test_token", ...env }),
    timeoutMs,
    fetchImpl: fetch,
  };
}

test("chat sends OpenAI-compatible request: path, bearer auth, role-mapped model, messages", async () => {
  const s = await startGateway(() => ({ status: 200, json: okCompletion("fast-flash") }));
  try {
    const result = await chat(gw(s.url, { E2AI_AGENT_MODEL_NAVIGATOR: "fast-flash" }), "navigator", [
      { role: "system", content: "you are a navigator" },
      { role: "user", content: "step data" },
    ]);
    const req = s.captured[0]!;
    assert.equal(req.method, "POST");
    assert.equal(req.url, "/v1/chat/completions");
    assert.equal(req.auth, "Bearer gt_test_token");
    assert.equal(req.body["model"], "fast-flash");
    assert.deepEqual(req.body["messages"], [
      { role: "system", content: "you are a navigator" },
      { role: "user", content: "step data" },
    ]);
    assert.equal(result.content, '{"kind":"act"}');
    assert.equal(result.model, "fast-flash");
    assert.deepEqual(result.usage, { tokensIn: 100, tokensOut: 20 });
  } finally {
    await s.close();
  }
});

test("chat strips trailing slash from base URL", async () => {
  const s = await startGateway(() => ({ status: 200, json: okCompletion() }));
  try {
    await chat(gw(`${s.url}/`, { E2AI_AGENT_MODEL_NAVIGATOR: "m" }), "navigator", [{ role: "user", content: "x" }]);
    assert.equal(s.captured[0]!.url, "/v1/chat/completions");
  } finally {
    await s.close();
  }
});

test("modelForRole resolves E2AI_AGENT_MODEL_<ROLE>; missing role throws GatewayConfigError", () => {
  assert.equal(modelForRole("vision_judge", { E2AI_AGENT_MODEL_VISION_JUDGE: "strong-vlm" }), "strong-vlm");
  assert.throws(() => modelForRole("navigator", {}), GatewayConfigError);
});

test("402 maps to non-retryable PaymentRequiredError", async () => {
  const s = await startGateway(() => ({ status: 402, json: { error: "budget spent" } }));
  try {
    await assert.rejects(
      chat(gw(s.url, { E2AI_AGENT_MODEL_NAVIGATOR: "m" }), "navigator", [{ role: "user", content: "x" }]),
      (err: unknown) => err instanceof PaymentRequiredError && err.retryable === false,
    );
  } finally {
    await s.close();
  }
});

test("429 maps to retryable RateLimitedError", async () => {
  const s = await startGateway(() => ({ status: 429, json: { error: "throttled" } }));
  try {
    await assert.rejects(
      chat(gw(s.url, { E2AI_AGENT_MODEL_NAVIGATOR: "m" }), "navigator", [{ role: "user", content: "x" }]),
      (err: unknown) => err instanceof RateLimitedError && err.retryable === true,
    );
  } finally {
    await s.close();
  }
});

test("timeout maps to retryable GatewayTimeoutError", async () => {
  const server = http.createServer(() => {
    /* never responds */
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const addr = server.address();
  const url = `http://127.0.0.1:${typeof addr === "object" && addr ? addr.port : 0}`;
  try {
    await assert.rejects(
      chat(gw(url, { E2AI_AGENT_MODEL_NAVIGATOR: "m" }, 50), "navigator", [{ role: "user", content: "x" }]),
      (err: unknown) => err instanceof GatewayTimeoutError && err.retryable === true,
    );
  } finally {
    server.closeAllConnections();
    await new Promise((r) => server.close(() => r(undefined)));
  }
});

test("response without usage is rejected — usage is the BUDG-01 cost source", async () => {
  const s = await startGateway(() => ({ status: 200, json: { choices: [{ message: { content: "{}" } }] } }));
  try {
    await assert.rejects(
      chat(gw(s.url, { E2AI_AGENT_MODEL_NAVIGATOR: "m" }), "navigator", [{ role: "user", content: "x" }]),
      /usage/,
    );
  } finally {
    await s.close();
  }
});
