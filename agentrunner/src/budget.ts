// budget.ts — per-run budget engine (BUDG-01, mirrored from Go
// internal/agents/budget.go semantics): step ceiling + cost accumulator.
//
// Cost model: gateway `usage` tokens × per-token price table from env.
//   millicents = tokens_in * PRICE_IN + tokens_out * PRICE_OUT
// Price env vars are PER-TOKEN prices in millicents of a US cent:
//   E2AI_PRICE_<MODEL>_IN_MILLICENTS / E2AI_PRICE_<MODEL>_OUT_MILLICENTS
// <MODEL> is the model name uppercased with non-alphanumerics → "_" (e.g.
// glm-5.3-flash → E2AI_PRICE_GLM_5_3_FLASH_IN_MILLICENTS).
//
// BudgetExhaustedError is TERMINAL and NON-RETRYABLE everywhere (T1 status
// machine, T4 river MaxAttempts 1): a budgeted run that hits the cap must
// never be re-spawned — the report says budget_exhausted.

import type { Usage } from "./gateway.js";

export class BudgetExhaustedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "BudgetExhaustedError";
  }
}

export class MissingPriceError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "MissingPriceError";
  }
}

export interface ModelPrice {
  inMillicents: number;
  outMillicents: number;
}

export type PriceTable = Record<string, ModelPrice>;

/** Normalized env key suffix for a model name. */
export function priceKey(model: string): string {
  return model.toUpperCase().replace(/[^A-Z0-9]+/g, "_");
}

/** Parse E2AI_PRICE_<MODEL>_{IN,OUT}_MILLICENTS pairs into a price table. */
export function priceTableFromEnv(env: NodeJS.ProcessEnv = process.env): PriceTable {
  const table: PriceTable = {};
  const pattern = /^E2AI_PRICE_([A-Z0-9_]+)_(IN|OUT)_MILLICENTS$/;
  for (const [key, raw] of Object.entries(env)) {
    const m = pattern.exec(key);
    if (!m || raw === undefined) continue;
    const value = Number(raw);
    if (!Number.isFinite(value) || value < 0) {
      throw new MissingPriceError(`price env ${key} must be a non-negative number, got ${raw}`);
    }
    const normalized = m[1] ?? "";
    const entry = (table[normalized] ??= { inMillicents: 0, outMillicents: 0 });
    if (m[2] === "IN") entry.inMillicents = value;
    else entry.outMillicents = value;
  }
  return table;
}

/** Cost of one gateway call in millicents (per-token prices, exact formula above). */
export function stepCostMillicents(model: string, usage: Usage, table: PriceTable): number {
  const price = table[priceKey(model)];
  if (!price) {
    throw new MissingPriceError(
      `no price table entry for model ${model} (set E2AI_PRICE_${priceKey(model)}_{IN,OUT}_MILLICENTS)`,
    );
  }
  return Math.round(usage.tokensIn * price.inMillicents + usage.tokensOut * price.outMillicents);
}

export interface BudgetLimits {
  /** Hard stop on steps per session. 0 = unlimited (matches DDL default). */
  stepCeiling: number;
  /** Hard stop on accumulated cost in millicents. 0 = unlimited. */
  costCeilingMillicents: number;
}

export function budgetLimitsFromEnv(env: NodeJS.ProcessEnv = process.env): BudgetLimits {
  return {
    stepCeiling: numberEnv(env, "E2AI_STEP_CEILING", 0),
    costCeilingMillicents: numberEnv(env, "E2AI_COST_CEILING_MILLICENTS", 0),
  };
}

function numberEnv(env: NodeJS.ProcessEnv, key: string, fallback: number): number {
  const raw = env[key];
  if (raw === undefined || raw === "") return fallback;
  const v = Number(raw);
  if (!Number.isFinite(v) || v < 0) {
    throw new BudgetExhaustedError(`${key} must be a non-negative number, got ${raw}`);
  }
  return Math.floor(v);
}

/**
 * Per-run budget: step ceiling + cost accumulator. Throw-only API — callers
 * (loop.ts) treat BudgetExhaustedError as terminal, non-retryable.
 */
export class Budget {
  readonly stepCeiling: number;
  readonly costCeilingMillicents: number;
  private readonly table: PriceTable;
  private readonly pricesKnown: Set<string>;

  stepsUsed = 0;
  costMillicents = 0;

  constructor(limits: BudgetLimits, table: PriceTable) {
    this.stepCeiling = limits.stepCeiling;
    this.costCeilingMillicents = limits.costCeilingMillicents;
    this.table = table;
    this.pricesKnown = new Set(Object.keys(table));
  }

  /**
   * Gate before starting a step. Trips when the step ceiling is reached or
   * accumulated cost has already crossed the cost ceiling.
   */
  beginStep(): void {
    if (this.stepCeiling > 0 && this.stepsUsed >= this.stepCeiling) {
      throw new BudgetExhaustedError(`step ceiling ${this.stepCeiling} reached`);
    }
    if (this.costCeilingMillicents > 0 && this.costMillicents >= this.costCeilingMillicents) {
      throw new BudgetExhaustedError(
        `cost ceiling ${this.costCeilingMillicents} millicents reached at ${this.costMillicents}`,
      );
    }
  }

  /**
   * Accumulate one step's gateway cost. Throws BudgetExhaustedError when the
   * cost ceiling is crossed (0 = unlimited). Returns the step's cost.
   */
  recordStep(model: string, usage: Usage): number {
    const cost = stepCostMillicents(model, usage, this.table);
    this.stepsUsed += 1;
    this.costMillicents += cost;
    if (this.costCeilingMillicents > 0 && this.costMillicents >= this.costCeilingMillicents) {
      throw new BudgetExhaustedError(
        `cost ceiling ${this.costCeilingMillicents} millicents reached at ${this.costMillicents}`,
      );
    }
    return cost;
  }

  /** Price-table entry presence (callers may pre-flight before looping). */
  hasPrice(model: string): boolean {
    return this.pricesKnown.has(priceKey(model));
  }
}
