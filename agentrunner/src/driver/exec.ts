// driver/exec.ts — child_process plumbing shared by the mobile drivers.
//
// All external tool invocations (adb, maestro, xcrun) go through ToolExec so
// tests can inject fakes ("spawn stubs") instead of real device tooling — the
// same seam style the loop tests use for the browser. The default impl
// (systemExec) spawns real binaries.
//
// Nonzero exits and spawn errors are RESOLVED, never thrown: classification
// into typed InfraFlakeError values is the driver's job (it knows the tool
// and the phase). Long-running commands (maestro test/record, simctl
// recordVideo) come back as a LongProcess the driver can await or kill —
// spawned detached in its own process group so teardown kills the whole tree.

import { spawn, type ChildProcess } from "node:child_process";

export interface ToolRun {
  /** Exit code; -1 when the process could not be spawned (see spawnError). */
  code: number;
  signal: NodeJS.Signals | null;
  stdout: string;
  stderr: string;
  /** The run hit its timeout and was killed. */
  timedOut: boolean;
  /** Set when spawn itself failed (ENOENT — tool not on PATH, ...). */
  spawnError?: string;
  /** Raw stdout as base64 when the caller passed `binary: true` — binary-safe
   * plumbing for screencap-style byte streams. */
  stdoutBase64?: string;
}

export interface LongProcess {
  pid: number | undefined;
  /** Resolves when the process exits (after kill, teardown exit, or completion). */
  done: Promise<ToolRun>;
  /** SIGTERMs the process group (escalating to SIGKILL). */
  kill(): void;
}

export interface ToolExec {
  run(tool: string, args: string[], opts?: { timeoutMs?: number; binary?: boolean }): Promise<ToolRun>;
  spawnLong(tool: string, args: string[]): Promise<LongProcess>;
}

export const DEFAULT_RUN_TIMEOUT_MS = 60_000;

export function systemExec(): ToolExec {
  return {
    run(tool, args, opts) {
      const child = spawn(tool, args, { stdio: ["ignore", "pipe", "pipe"] });
      return collect(child, opts?.timeoutMs ?? DEFAULT_RUN_TIMEOUT_MS, opts?.binary === true);
    },
    async spawnLong(tool, args) {
      const child = spawn(tool, args, { stdio: ["ignore", "pipe", "pipe"], detached: true });
      const done = collect(child, 0, false);
      return {
        pid: child.pid,
        done,
        kill: () => {
          killTree(child);
        },
      };
    },
  };
}

/** Teardown kill for the process-tree: detached children get group SIGTERM,
 * plain children get direct SIGTERM. */
function killTree(child: { pid?: number | null; kill(signal?: NodeJS.Signals): boolean }): void {
  if (child.pid && child.pid > 0) {
    try {
      process.kill(-child.pid, "SIGTERM");
      return;
    } catch {
      // process group already gone (or not a group leader) — fall through
    }
  }
  try {
    child.kill("SIGTERM");
  } catch {
    // already dead
  }
}

function collect(child: ChildProcess, timeoutMs: number, binary: boolean): Promise<ToolRun> {
  let resolve: (run: ToolRun) => void = () => {};
  const promise = new Promise<ToolRun>((r) => (resolve = r));
  {
    let out = "";
    let outB64 = "";
    let err = "";
    let timedOut = false;
    let settled = false;

    if (binary) {
      const chunks: Buffer[] = [];
      child.stdout?.on("data", (c: Buffer) => chunks.push(c));
      child.stdout?.on("end", () => {
        outB64 = Buffer.concat(chunks).toString("base64");
      });
    } else {
      child.stdout?.on("data", (c: Buffer) => {
        out += c.toString("utf8");
      });
    }
    child.stderr?.on("data", (c: Buffer) => {
      err += c.toString("utf8");
    });

    let timer: NodeJS.Timeout | undefined =
      timeoutMs > 0
        ? setTimeout(() => {
            timedOut = true;
            killTree(child);
            // stubborn processes get SIGKILL after a grace period
            setTimeout(() => {
              try {
                if (child.pid && child.pid > 0) process.kill(-child.pid, "SIGKILL");
              } catch {
                /* already gone */
              }
              try {
                child.kill("SIGKILL");
              } catch {
                /* already gone */
              }
            }, 2_000).unref();
          }, timeoutMs)
        : undefined;

    const finish = (code: number, signal: NodeJS.Signals | null, spawnError?: string): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve({
        code,
        signal,
        stdout: out,
        stderr: err,
        timedOut,
        spawnError,
        ...(binary ? { stdoutBase64: outB64 || Buffer.from(out).toString("base64") } : {}),
      });
    };

    child.on("error", (e: Error) => finish(-1, null, String(e)));
    child.on("close", (code, signal) => finish(code ?? -1, signal));
  }
  return promise;
}
