// Unit tests for sandbox.ts (node:test, no network).
import test from "node:test";
import assert from "node:assert/strict";
import {
  Sandbox,
  SandboxConfigError,
  EgressViolation,
  sandboxOptionsFromEnv,
  assertNavigationAllowed,
  assertNoAppCredentials,
  CredentialScanError,
  isCredentialShapedKey,
} from "./sandbox.js";

const DEV_ENV: NodeJS.ProcessEnv = {
  E2AI_ENV: "dev",
  E2AI_STAGING_BASE_URL: "https://staging.bet.example, https://staging.bet-alt.example",
  AIHUB_BASE_URL: "https://aihub.internal",
  E2AI_API_BASE_URL: "https://api.e2ai.internal",
};

test("allowlisted https staging origins pass and the absolute URL is returned", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv(DEV_ENV));
  assert.equal(sandbox.assertNavigationAllowed("https://staging.bet.example/payments/new"), "https://staging.bet.example/payments/new");
});

test("relative hrefs resolve against the current page before the allowlist check", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv(DEV_ENV));
  assert.equal(
    sandbox.assertNavigationAllowed("/payments/confirm", "https://staging.bet.example/payments/new"),
    "https://staging.bet.example/payments/confirm",
  );
});

test("relative href escaping to a non-allowlisted host throws EgressViolation", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv(DEV_ENV));
  assert.throws(
    () => sandbox.assertNavigationAllowed("https://evil.example/steal", "https://staging.bet.example/"),
    (err: unknown) => err instanceof EgressViolation && err.host === "evil.example" && err.reason === "host-not-allowlisted",
  );
});

test("relative href with no current page cannot be resolved", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv(DEV_ENV));
  assert.throws(
    () => sandbox.assertNavigationAllowed("/next"),
    (err: unknown) => err instanceof EgressViolation && err.reason === "unresolvable-href",
  );
});

test("implicit default ports normalize (443 matches the bare origin)", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv({ E2AI_STAGING_BASE_URL: "https://staging.bet.example" }));
  // URL normalizes the implicit default port away; origin matching is port-canonical
  assert.equal(sandbox.assertNavigationAllowed("https://staging.bet.example:443/x"), "https://staging.bet.example/x");
});

test("non-allowlisted https host is logged then rejected", (t) => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv(DEV_ENV));
  const logged = t.mock.method(console, "error", () => {});
  assert.throws(() => sandbox.assertNavigationAllowed("https://evil.example/x"), EgressViolation);
  assert.equal(logged.mock.callCount(), 1);
  assert.ok(logged.mock.calls[0]!.arguments.join(" ").includes("evil.example"));
});

test("plain http is refused outside dev loopback", () => {
  const sandbox = new Sandbox({ allowedOrigins: ["http://localhost:3000"], allowHttpLoopback: false });
  assert.throws(
    () => sandbox.assertNavigationAllowed("http://localhost:3000/login"),
    (err: unknown) => err instanceof EgressViolation && err.reason === "protocol-not-allowed",
  );
});

test("dev env allows http loopback fixture navigation when allowlisted", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv({ E2AI_ENV: "local", E2AI_STAGING_BASE_URL: "http://127.0.0.1:4173" }));
  assert.equal(sandbox.assertNavigationAllowed("/login", "http://127.0.0.1:4173/"), "http://127.0.0.1:4173/login");
});

test("dev env does NOT allow http on non-loopback hosts even if listed as target", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv({ E2AI_ENV: "dev", E2AI_STAGING_BASE_URL: "https://staging.bet.example" }));
  assert.throws(() => sandbox.assertNavigationAllowed("http://staging.bet.example/x"), EgressViolation);
});

test("non-http(s) protocols (javascript:, data:) are refused", () => {
  const sandbox = new Sandbox(sandboxOptionsFromEnv(DEV_ENV));
  assert.throws(() => sandbox.assertNavigationAllowed("javascript:alert(1)"), EgressViolation);
  assert.throws(() => sandbox.assertNavigationAllowed("data:text/html,<h1>hi</h1>"), EgressViolation);
});

test("standalone assertNavigationAllowed reads the allowlist from env", () => {
  assert.equal(assertNavigationAllowed("https://staging.bet.example/x", DEV_ENV), "https://staging.bet.example/x");
  assert.throws(() => assertNavigationAllowed("https://anything.example/x", {}), EgressViolation);
});

test("malformed allowlist entry throws SandboxConfigError", () => {
  assert.throws(
    () => sandboxOptionsFromEnv({ E2AI_STAGING_BASE_URL: "not a url :: ???", E2AI_ENV: "dev" }),
    SandboxConfigError,
  );
});

// --- boot-time credential scan ---------------------------------------------

test("credential scan: clean env passes", () => {
  assert.doesNotThrow(() =>
    assertNoAppCredentials({
      PATH: "/usr/bin",
      HOME: "/home/agent",
      E2AI_SESSION_ID: "s1",
      E2AI_RUNNER_SERVICE_TOKEN: "st_x",
      AIHUB_API_TOKEN: "gt_x",
      AIHUB_BASE_URL: "https://aihub.internal",
    }),
  );
});

test("credential scan: secret-shaped vars fail with offenders listed", () => {
  assert.throws(
    () => assertNoAppCredentials({ PATH: "/usr/bin", GITHUB_TOKEN: "gh_pat_x", POSTGRES_PASSWORD: "hunter2", SESSION_COOKIE: "sid=1" }),
    (err: unknown) => {
      assert.ok(err instanceof CredentialScanError);
      assert.deepEqual([...err.offenders].sort(), ["GITHUB_TOKEN", "POSTGRES_PASSWORD", "SESSION_COOKIE"]);
      assert.match(err.message, /GITHUB_TOKEN/);
      return true;
    },
  );
});

test("credential scan: cloud/AWS and webhook secrets are caught", () => {
  assert.throws(
    () => assertNoAppCredentials({ AWS_SECRET_ACCESS_KEY: "k", WEBHOOK_SECRET: "whsec", OKTA_API_KEY: "o" }),
    (err: unknown) => err instanceof CredentialScanError && err.offenders.length === 3,
  );
});

test("credential scan: E2AI_/AIHUB_ scoped secrets and benign system keys pass", () => {
  assert.equal(isCredentialShapedKey("E2AI_RUNNER_SERVICE_TOKEN"), false);
  assert.equal(isCredentialShapedKey("AIHUB_API_TOKEN"), false);
  assert.equal(isCredentialShapedKey("SECURITYSESSIONID"), false);
  assert.equal(isCredentialShapedKey("PWD"), false);
  assert.equal(isCredentialShapedKey("GITHUB_TOKEN"), true);
  assert.doesNotThrow(() =>
    assertNoAppCredentials({ PWD: "/x", SECURITYSESSIONID: "y", E2AI_STEP_CEILING: "40", AIHUB_TIMEOUT_MS: "30000" }),
  );
});
