// budget.test.ts — ceiling trips, cost accumulation math, price table env parsing.
import test from "node:test";
import assert from "node:assert/strict";
import {
  Budget,
  BudgetExhaustedError,
  MissingPriceError,
  budgetLimitsFromEnv,
  priceKey,
  priceTableFromEnv,
  stepCostMillicents,
} from "./budget.js";

test("priceTableFromEnv parses IN/OUT pairs, normalizing model names", () => {
  const table = priceTableFromEnv({
    E2AI_PRICE_GLM_5_3_FLASH_IN_MILLICENTS: "0.15",
    E2AI_PRICE_GLM_5_3_FLASH_OUT_MILLICENTS: "1.5",
    E2AI_PRICE_STRONG_VLM_OUT_MILLICENTS: "12",
  });
  assert.deepEqual(table["GLM_5_3_FLASH"], { inMillicents: 0.15, outMillicents: 1.5 });
  assert.deepEqual(table["STRONG_VLM"], { inMillicents: 0, outMillicents: 12 });
  assert.throws(() => priceTableFromEnv({ E2AI_PRICE_BAD_IN_MILLICENTS: "not-a-number" }), MissingPriceError);
});

test("priceKey maps model names to env key fragments", () => {
  assert.equal(priceKey("glm-5.3-flash"), "GLM_5_3_FLASH");
  assert.equal(priceKey("gpt-4o"), "GPT_4O");
});

test("stepCostMillicents: exact per-token formula", () => {
  const table = priceTableFromEnv({
    E2AI_PRICE_M_IN_MILLICENTS: "0.15",
    E2AI_PRICE_M_OUT_MILLICENTS: "1.5",
  });
  // tokens_in*PRICE_IN + tokens_out*PRICE_OUT
  const cost = stepCostMillicents("m", { tokensIn: 1000, tokensOut: 100 }, table);
  assert.equal(cost, Math.round(1000 * 0.15 + 100 * 1.5));
  assert.equal(cost, 300);
});

test("stepCostMillicents throws MissingPriceError for unpriced models", () => {
  assert.throws(() => stepCostMillicents("unknown-model", { tokensIn: 1, tokensOut: 1 }, {}), MissingPriceError);
});

test("step ceiling trips: beginStep throws after ceiling steps, terminal + non-retryable by type", () => {
  const budget = new Budget({ stepCeiling: 3, costCeilingMillicents: 0 }, { M: { inMillicents: 0, outMillicents: 0 } });
  const usage = { tokensIn: 0, tokensOut: 0 };
  for (let i = 0; i < 3; i++) {
    budget.beginStep();
    budget.recordStep("M", usage);
  }
  assert.throws(() => budget.beginStep(), (err: unknown) => err instanceof BudgetExhaustedError);
  assert.equal(budget.stepsUsed, 3);
});

test("cost accumulator accumulates across steps and trips at cost ceiling", () => {
  const budget = new Budget({ stepCeiling: 0, costCeilingMillicents: 400 }, {
    M: { inMillicents: 0.15, outMillicents: 1.5 },
  });
  const usage = { tokensIn: 1000, tokensOut: 100 }; // 300 millicents per step
  assert.equal(budget.recordStep("M", usage), 300);
  assert.equal(budget.costMillicents, 300);
  budget.beginStep(); // still under ceiling
  assert.throws(() => budget.recordStep("M", usage), (err: unknown) => err instanceof BudgetExhaustedError);
  assert.equal(budget.costMillicents, 600);
});

test("zero ceilings mean unlimited", () => {
  const budget = new Budget({ stepCeiling: 0, costCeilingMillicents: 0 }, {
    M: { inMillicents: 1, outMillicents: 1 },
  });
  for (let i = 0; i < 50; i++) {
    budget.beginStep();
    budget.recordStep("M", { tokensIn: 100, tokensOut: 100 });
  }
  assert.equal(budget.costMillicents, 10_000);
});

test("budgetLimitsFromEnv reads ceilings with 0 defaults", () => {
  assert.deepEqual(budgetLimitsFromEnv({}), { stepCeiling: 0, costCeilingMillicents: 0 });
  assert.deepEqual(
    budgetLimitsFromEnv({ E2AI_STEP_CEILING: "40", E2AI_COST_CEILING_MILLICENTS: "250000" }),
    { stepCeiling: 40, costCeilingMillicents: 250_000 },
  );
});
