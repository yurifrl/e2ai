// api.test.ts — fake agent-api server: steps append shape (0004_agents.sql
// columns), heartbeat, complete/fail, 409 → KilledError clean-exit mapping.
import test from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { AgentApi, ApiError, ApiConfigError, KilledError, apiConfigFromEnv } from "./api.js";

interface Captured {
  method: string;
  url: string;
  auth: string | null;
  body: Record<string, unknown>;
}

function startApi(
  routes: Record<string, { status: number } | { status: number; json: unknown }>,
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
      const route = routes[req.url ?? ""] ?? { status: 404 };
      res.writeHead(route.status, { "content-type": "application/json" });
      res.end("json" in route ? JSON.stringify(route.json) : "{}");
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

function api(url: string): AgentApi {
  return new AgentApi(apiConfigFromEnv({ E2AI_API_BASE_URL: url, E2AI_RUNNER_SERVICE_TOKEN: "st_runner_token" }));
}

const stepRoutes = {
  "/api/agent-sessions/s1/heartbeat": { status: 200 },
  "/api/agent-sessions/s1/steps": { status: 201 },
  "/api/agent-sessions/s1/complete": { status: 200 },
  "/api/agent-sessions/s1/fail": { status: 200 },
};

test("appendStep posts the session_steps DDL shape with runner bearer auth", async () => {
  const s = await startApi(stepRoutes);
  try {
    const a = api(s.url);
    await a.appendStep("s1", {
      idx: 4,
      kind: "act",
      decision: { kind: "act", selector: "#login" },
      action: { clicked: "#login" },
      evidence: [{ kind: "screenshot", path: "agents/s1/screenshots/step-4.png" }],
      tokensIn: 1200,
      tokensOut: 45,
      costMillicents: 247,
    });
    const req = s.captured[0]!;
    assert.equal(req.method, "POST");
    assert.equal(req.url, "/api/agent-sessions/s1/steps");
    assert.equal(req.auth, "Bearer st_runner_token");
    // exact column semantics from api/internal/app/migrations/0004_agents.sql
    assert.deepEqual(req.body, {
      idx: 4,
      kind: "act",
      decision: { kind: "act", selector: "#login" },
      action: { clicked: "#login" },
      evidence: [{ kind: "screenshot", path: "agents/s1/screenshots/step-4.png" }],
      tokens_in: 1200,
      tokens_out: 45,
      cost_millicents: 247,
    });
  } finally {
    await s.close();
  }
});

test("heartbeat, complete, and fail hit their endpoints; fail carries error_note", async () => {
  const s = await startApi(stepRoutes);
  try {
    const a = api(s.url);
    await a.heartbeat("s1");
    await a.complete("s1");
    await a.fail("s1", "playwright hung on navigation");
    assert.deepEqual(
      s.captured.map((c) => c.url),
      [
        "/api/agent-sessions/s1/heartbeat",
        "/api/agent-sessions/s1/complete",
        "/api/agent-sessions/s1/fail",
      ],
    );
    assert.deepEqual(s.captured[2]!.body, { error_note: "playwright hung on navigation" });
  } finally {
    await s.close();
  }
});

test("409 on any call → KilledError (clean exit semantics, non-retryable)", async () => {
  const killed: Record<string, { status: number }> = {
    "/api/agent-sessions/s1/heartbeat": { status: 409 },
    "/api/agent-sessions/s1/steps": { status: 409 },
  };
  {
    const s = await startApi(killed);
    try {
      await assert.rejects(api(s.url).heartbeat("s1"), (err: unknown) => err instanceof KilledError);
    } finally {
      await s.close();
    }
  }
  {
    const s = await startApi(killed);
    try {
      await assert.rejects(
        api(s.url).appendStep("s1", {
          idx: 0,
          kind: "observe",
          decision: {},
          action: {},
          evidence: [],
          tokensIn: 0,
          tokensOut: 0,
          costMillicents: 0,
        }),
        (err: unknown) => err instanceof KilledError,
      );
    } finally {
      await s.close();
    }
  }
});

test("other non-2xx → ApiError with retryable only for 5xx", async () => {
  const s = await startApi({
    "/api/agent-sessions/s1/heartbeat": { status: 500 },
    "/api/agent-sessions/s1/complete": { status: 403 },
  });
  try {
    const a = api(s.url);
    await assert.rejects(a.heartbeat("s1"), (err: unknown) => err instanceof ApiError && err.retryable === true);
    await assert.rejects(a.complete("s1"), (err: unknown) => err instanceof ApiError && err.retryable === false);
  } finally {
    await s.close();
  }
});

test("apiConfigFromEnv validates required env", () => {
  assert.throws(() => apiConfigFromEnv({}), ApiConfigError);
  assert.throws(() => apiConfigFromEnv({ E2AI_API_BASE_URL: "http://x" }), ApiConfigError);
  const cfg = apiConfigFromEnv({ E2AI_API_BASE_URL: "http://x/", E2AI_RUNNER_SERVICE_TOKEN: "st_t" });
  assert.equal(cfg.baseUrl, "http://x");
  assert.equal(cfg.serviceToken, "st_t");
});
