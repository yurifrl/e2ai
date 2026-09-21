// repair.ts — the navigator's side of FLAK-02 selector repair (Phase 5 T7).
//
// Report-only posture (RESEARCH decision 1): a failed locator act may produce
// a structured repair PROPOSAL for human review — never a silent locator
// rewrite, never a plan/corpus/app mutation from the runner. The Go control
// plane (api/internal/corpus/repair.go) owns the store and the refuse-to-heal
// floor; this module mirrors the same gate client-side so a below-floor
// suggestion never even hits the wire (defense in depth), and escalates to a
// plain observation instead.
//
// Wire contract: POST /api/agent-sessions/{id}/repair-proposals — snake_case
// body mirroring Go agents.repairProposalRequest (additive, see
// agents/repair.go).

/** Refuse-to-heal floor (RESEARCH decision 4). MUST stay equal to
 * corpus.MinRepairConfidence in api/internal/corpus/repair.go — the control
 * plane re-checks and returns 422 below the floor. */
export const REPAIR_CONFIDENCE_FLOOR = 0.7;

/** One structured selector-repair suggestion from the navigator. Every field
 * is required except evidenceRef (the run-relative artifact path, empty when
 * the driver captured nothing). */
export interface RepairSuggestion {
  /** Corpus behavior key the broken selector was acting on (plan `uses:` key
   * or a LEARN- id). */
  behaviorKey: string;
  /** The selector that failed, verbatim. */
  brokenSelector: string;
  /** The locator-first replacement the navigator can SEE in the a11y
   * snapshot. */
  proposedSelector: string;
  /** Why the replacement is better (snapshot ref, renamed class, …). */
  rationale: string;
  /** Self-assessed confidence in [0, 1]; below the floor the proposal is
   * refused — escalate as an observation for human review instead. */
  confidence: number;
  /** Run-relative artifact path backing the suggestion (a11y json, …). */
  evidenceRef?: string;
}

export type HealDecision = { allowed: true } | { allowed: false; reason: string };

const ERROR_STATE_RE = /(^|[^a-z])(error|404|500|503|forbidden|denied|unauthor[iz]ed|exception|maintenance|alert)([^a-z]|$)/i;

/** True when the page the act failed on is itself an error state (alert /
 * 500 / crashed). RESEARCH decision 4: never heal on error-state pages — the
 * "fix" would just encode the failure page's layout. */
export function isErrorStatePage(url: string | undefined, title: string | undefined): boolean {
  return ERROR_STATE_RE.test(url ?? "") || ERROR_STATE_RE.test(title ?? "");
}

/** The refuse-to-heal gate. Returns allowed=false with the reason when the
 * suggestion must NOT become a proposal — the caller escalates with an
 * observation instead. Mirrors the corpus store's server-side checks
 * (AppendRepairProposal) so the navigator's behavior matches the API even
 * before the request leaves the process. */
export function healDecision(
  suggestion: RepairSuggestion,
  page: { url?: string; title?: string } = {},
): HealDecision {
  const missing: string[] = [];
  if (!suggestion.behaviorKey.trim()) missing.push("behaviorKey");
  if (!suggestion.brokenSelector.trim()) missing.push("brokenSelector");
  if (!suggestion.proposedSelector.trim()) missing.push("proposedSelector");
  if (!suggestion.rationale.trim()) missing.push("rationale");
  if (missing.length > 0) {
    return { allowed: false, reason: `incomplete repair suggestion (missing: ${missing.join(", ")})` };
  }
  if (typeof suggestion.confidence !== "number" || Number.isNaN(suggestion.confidence) ||
    suggestion.confidence < 0 || suggestion.confidence > 1) {
    return { allowed: false, reason: "confidence must be a number in [0, 1]" };
  }
  if (suggestion.confidence < REPAIR_CONFIDENCE_FLOOR) {
    return {
      allowed: false,
      reason: `confidence ${suggestion.confidence} below refuse-to-heal floor ${REPAIR_CONFIDENCE_FLOOR} — escalate as an observation`,
    };
  }
  if (isErrorStatePage(page.url, page.title)) {
    return { allowed: false, reason: "refusing to heal on an error-state page" };
  }
  return { allowed: true };
}

/** The escalation observation a refused suggestion leaves behind (append-only
 * learning stream, human-reviewed like any other observation). */
export function escalationObservation(suggestion: RepairSuggestion, reason: string): string {
  return `Selector repair REFUSED (${reason}): behavior ${suggestion.behaviorKey}, broken selector ${suggestion.brokenSelector}, proposed ${suggestion.proposedSelector}. ${suggestion.rationale}`;
}
