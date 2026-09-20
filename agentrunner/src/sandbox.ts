// sandbox.ts — navigation egress allowlist + boot-time credential scan (SAFE-01).
//
// Egress: deny-by-default URL allowlist for every agent-driven page
// navigation. Allowed origins = staging base URL(s) (E2AI_STAGING_BASE_URL,
// comma/space-separated list) + the aihub gateway base (AIHUB_BASE_URL) + the
// e2ai api base (E2AI_API_BASE_URL). Protocols: https only, except plain http
// on loopback when E2AI_ENV is dev/local (fixture/unit work). Violations are
// logged to stderr, then thrown as EgressViolation.
//
// This module is the IN-PROCESS gate; the container network policy
// (compose/tilt iptables + DNS allowlist) is the outer enforcement layer —
// documented in local/agent-runner.yaml (P1 runner container precedent).

export class SandboxConfigError extends Error {}

/** A navigation (or any agent-controlled URL fetch) tried to leave the
 * allowlist. Terminal: the step fails; the loop must not retry the URL. */
export class EgressViolation extends Error {
  readonly url: string;
  readonly host: string;
  readonly reason: "protocol-not-allowed" | "host-not-allowlisted" | "unresolvable-href";
  constructor(url: string, host: string, reason: EgressViolation["reason"]) {
    super(`[sandbox] egress violation (${reason}): ${url}`);
    this.name = "EgressViolation";
    this.url = url;
    this.host = host;
    this.reason = reason;
  }
}

export interface SandboxOptions {
  /** Allowlisted origins, `protocol//host[:port]`, default ports stripped. */
  allowedOrigins: string[];
  /** dev/local only: allow plain http on loopback (fixture servers). */
  allowHttpLoopback: boolean;
}

const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);
const DEFAULT_PORT: Record<string, string> = { "https:": "443", "http:": "80" };

function isLoopback(hostname: string): boolean {
  return LOOPBACK.has(hostname.toLowerCase()) || hostname.toLowerCase().endsWith(".localhost");
}

/** Canonical origin form: lowercase host, default ports stripped. */
export function originOf(url: URL): string {
  const defaultPort = DEFAULT_PORT[url.protocol] ?? "";
  const port = url.port && url.port !== defaultPort ? `:${url.port}` : "";
  return `${url.protocol}//${url.hostname.toLowerCase()}${port}`;
}

/** Builds sandbox options from the env contract:
 *   E2AI_STAGING_BASE_URL — staging base URL(s), comma/space-separated
 *   AIHUB_BASE_URL, E2AI_API_BASE_URL — control surfaces added to the allowlist
 *   E2AI_ENV — dev|development|local ⇒ http loopback allowed for fixtures */
export function sandboxOptionsFromEnv(env: NodeJS.ProcessEnv = process.env): SandboxOptions {
  const allowedOrigins: string[] = [];
  const add = (raw: string | undefined, name: string) => {
    if (!raw) return;
    for (const part of raw.split(/[,\s]+/).filter(Boolean)) {
      let url: URL;
      try {
        url = new URL(part);
      } catch {
        throw new SandboxConfigError(`${name} entry is not a valid URL: ${part}`);
      }
      allowedOrigins.push(originOf(url));
    }
  };
  add(env["E2AI_STAGING_BASE_URL"], "E2AI_STAGING_BASE_URL");
  add(env["AIHUB_BASE_URL"], "AIHUB_BASE_URL");
  add(env["E2AI_API_BASE_URL"], "E2AI_API_BASE_URL");
  const e2aiEnv = (env["E2AI_ENV"] ?? "").toLowerCase();
  return {
    allowedOrigins: [...new Set(allowedOrigins)],
    allowHttpLoopback: e2aiEnv === "dev" || e2aiEnv === "development" || e2aiEnv === "local",
  };
}

export class Sandbox {
  readonly #allowed: Set<string>;
  readonly #allowHttpLoopback: boolean;
  constructor(options: SandboxOptions) {
    this.#allowed = new Set(options.allowedOrigins);
    this.#allowHttpLoopback = options.allowHttpLoopback;
  }

  /** Validates one navigation target. `href` may be relative — it is resolved
   * against `currentUrl` (the page the agent is on) before the checks. Returns
   * the resolved absolute URL; logs and throws EgressViolation otherwise. */
  assertNavigationAllowed(href: string, currentUrl?: string): string {
    let url: URL;
    try {
      url = new URL(href, currentUrl);
    } catch {
      const err = new EgressViolation(href, "", "unresolvable-href");
      console.error(err.message);
      throw err;
    }
    if (url.protocol === "http:") {
      if (!this.#allowHttpLoopback || !isLoopback(url.hostname)) {
        const err = new EgressViolation(url.toString(), url.hostname, "protocol-not-allowed");
        console.error(err.message);
        throw err;
      }
    } else if (url.protocol !== "https:") {
      const err = new EgressViolation(url.toString(), url.hostname, "protocol-not-allowed");
      console.error(err.message);
      throw err;
    }
    if (!this.#allowed.has(originOf(url))) {
      const err = new EgressViolation(url.toString(), url.hostname, "host-not-allowlisted");
      console.error(err.message);
      throw err;
    }
    return url.toString();
  }
}

/** Standalone gate: builds a Sandbox from env and validates one absolute URL. */
export function assertNavigationAllowed(url: string, env: NodeJS.ProcessEnv = process.env): string {
  return new Sandbox(sandboxOptionsFromEnv(env)).assertNavigationAllowed(url);
}

// ---------------------------------------------------------------------------
// Boot-time credential scan (SAFE-01: the agent container mounts nothing but
// e2ai/AIHUB-scoped tokens).
// ---------------------------------------------------------------------------

export class CredentialScanError extends Error {
  readonly offenders: string[];
  constructor(message: string, offenders: string[]) {
    super(message);
    this.name = "CredentialScanError";
    this.offenders = offenders;
  }
}

/** Keys that name an app credential — passwords, cookies, tokens, keys,
 * sessions — UNLESS they carry the runner's own scoped prefixes (E2AI_,
 * AIHUB_). Word-boundary so benign system vars (SECURITYSESSIONID, PWD) don't
 * false-positive. */
const SECRET_SHAPED_KEY =
  /(?:^|_)(PASSWORDS?|PASSWD|SECRETS?|TOKENS?|COOKIES?|CREDENTIALS?|API_?KEYS?|ACCESS_?KEYS?|SECRET_?KEYS?|PRIVATE_?KEYS?|SESSION_?IDS?|AUTHORIZATION|BEARER|JWT)(?:_|$)/i;
const SCOPED_PREFIX = /^(E2AI_|AIHUB_)/;

export function isCredentialShapedKey(key: string): boolean {
  if (SCOPED_PREFIX.test(key)) return false;
  return SECRET_SHAPED_KEY.test(key);
}

/** Refuses to boot if any env var looks like an app credential that the agent
 * container must never see (GITHUB_TOKEN, POSTGRES_PASSWORD, session cookies,
 * cloud keys...). The only credentials allowed are E2AI_/AIHUB_-prefixed,
 * e2ai-scoped runner tokens. Wired into index.ts boot, before the loop. */
export function assertNoAppCredentials(env: NodeJS.ProcessEnv = process.env): void {
  const offenders = Object.keys(env).filter(isCredentialShapedKey);
  if (offenders.length === 0) return;
  throw new CredentialScanError(
    `refusing to boot: agent container carries app credentials (SAFE-01): ${offenders.join(", ")}`,
    offenders,
  );
}
