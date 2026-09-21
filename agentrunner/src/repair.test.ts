// repair.test.ts — the navigator's refuse-to-heal gate (FLAK-02, Phase 5 T7).
// The centerpiece is the report-only invariant: a below-floor or error-state
// suggestion NEVER becomes a proposal — it escalates as an observation — and
// the client-side gate mirrors the corpus store's server-side checks.
import test from "node:test";
import assert from "node:assert/strict";
import {
  REPAIR_CONFIDENCE_FLOOR,
  escalationObservation,
  healDecision,
  isErrorStatePage,
  type RepairSuggestion,
} from "./repair.js";

function suggestion(overrides: Partial<RepairSuggestion> = {}): RepairSuggestion {
  return {
    behaviorKey: "PWSPEC-LIVE-BADGE",
    brokenSelector: "css=.bet-button-old-class",
    proposedSelector: "role=button[name=\"Place bet\"]",
    rationale: "a11y snapshot shows an accessible-name button; the css class was renamed",
    confidence: 0.9,
    ...overrides,
  };
}

test("REPAIR_CONFIDENCE_FLOOR matches the Go control plane", () => {
  // Mirrors corpus.MinRepairConfidence in api/internal/corpus/repair.go.
  assert.equal(REPAIR_CONFIDENCE_FLOOR, 0.7);
});

test("complete, confident suggestion on a healthy page is allowed", () => {
  assert.deepEqual(healDecision(suggestion(), { url: "https://bet.example/deposit", title: "Deposit" }), {
    allowed: true,
  });
});

test("below the floor is refused — escalate as an observation instead", () => {
  const decision = healDecision(suggestion({ confidence: 0.69 }));
  assert.equal(decision.allowed, false);
  if (!decision.allowed) {
    assert.match(decision.reason, /below refuse-to-heal floor/);
  }
});

test("the floor itself (0.7) is allowed; 0.7 - epsilon is not", () => {
  assert.deepEqual(healDecision(suggestion({ confidence: 0.7 })), { allowed: true });
  assert.equal(healDecision(suggestion({ confidence: 0.699 })).allowed, false);
});

test("out-of-range or non-numeric confidence is refused", () => {
  assert.equal(healDecision(suggestion({ confidence: 1.1 })).allowed, false);
  assert.equal(healDecision(suggestion({ confidence: -0.1 })).allowed, false);
  // @ts-expect-error — malformed model output is exactly what the gate guards.
  assert.equal(healDecision(suggestion({ confidence: "0.9" })).allowed, false);
});

test("incomplete suggestions are refused with the missing fields named", () => {
  const decision = healDecision(suggestion({ rationale: "  " }));
  assert.equal(decision.allowed, false);
  if (!decision.allowed) {
    assert.match(decision.reason, /rationale/);
  }
});

test("never heal on an error-state page (RESEARCH decision 4)", () => {
  const url = "https://bet.example/500";
  assert.equal(isErrorStatePage(url, "Error - BetWeb"), true);
  assert.equal(healDecision(suggestion(), { url, title: "Error - BetWeb" }).allowed, false);
  // A page that merely mentions the word inside a larger token is not an error state.
  assert.equal(isErrorStatePage("https://bet.example/errors-guide", "Help"), false);
});

test("escalationObservation embeds the refusal reason and the broken selector", () => {
  const obs = escalationObservation(suggestion({ confidence: 0.4 }), "confidence below floor");
  assert.match(obs, /REFUSED/);
  assert.match(obs, /confidence below floor/);
  assert.match(obs, /bet-button-old-class/);
  assert.match(obs, /PWSPEC-LIVE-BADGE/);
});
