/**
 * Wire types for the e2ai API (contract locked with the API owner).
 *
 * Verdicts and statuses are uppercase enums per the wire contract; DB stores
 * lowercase but the HTTP surface serializes uppercase.
 */

export type RunStatus =
  | 'QUEUED'
  | 'WAITING_ROLLOUT'
  | 'PROVISIONING'
  | 'EXECUTING'
  | 'REPORTING'
  | 'COMPLETED'
  | 'FAILED'

export type Verdict = 'PASS' | 'FAIL' | 'FLAKY' | 'INFRA_ERROR' | 'INCONCLUSIVE'

export type TestOutcome = 'PASS' | 'FAIL' | 'FLAKY' | 'SKIPPED'

export type PrState = 'OPEN' | 'MERGED' | 'CLOSED' | 'NONE'

export type ArtifactKind = 'screenshot' | 'trace' | 'video' | 'results'

export interface TestCounts {
  total_tests: number
  passed: number
  failed: number
  flaky: number
  /** infra_error + inconclusive fold in here (runs has no infra bucket). */
  skipped: number
}

export interface Run {
  id: string
  app_id: string
  workflow_run_id: number
  run_number: number
  head_sha: string
  head_branch: string | null
  environment: string
  trigger_source: 'WEBHOOK' | 'RECONCILE' | 'SWEEP' | 'MANUAL' | 'SIMULATE'
  status: RunStatus
  verdict: Verdict | null
  pr_number: number | null
  pr_state: PrState | null
  pr_title: string | null
  commit_message: string | null
  report_url: string | null
  comment_url: string | null
  check_run_id: number | null
  error_note: string | null
  started_at: string | null
  finished_at: string | null
  created_at: string
  /** Set by the report job on completion. */
  duration_ms: number | null
  /** Aggregate test counts; null until the report job populates them. */
  counts: TestCounts | null
}

export interface TestResult {
  id: string
  run_id: string
  suite: string | null
  title: string
  file: string | null
  line: number | null
  project: string
  outcome: TestOutcome
  duration_ms: number | null
  error_message: string | null
}

export interface Artifact {
  id: string
  run_id: string
  kind: ArtifactKind
  path: string
  size_bytes: number | null
  created_at: string
}

export interface RunDetail {
  run: Run
  test_results: TestResult[]
  artifacts: Artifact[]
}

export interface RunsPage {
  runs: Run[]
  limit: number
  offset: number
  next_offset: number | null
}

/** Report page data assembly: one blob the report viewer renders directly. */
export interface ReportData {
  run: Run
  counts: TestCounts
  /** Playwright projects represented in this run. */
  projects: string[]
  failures: TestResult[]
  flaky: TestResult[]
  /** Per-test ordered step timeline for the filmstrip. */
  steps: {
    test_id: string
    title: string
    outcome: TestOutcome
    duration_ms: number | null
    screenshots: string[]
  }[]
  /** Agentic trajectory (phase 5, T3); null for deterministic Playwright runs. */
  trajectory: TrajectorySession[] | null
  /** Run-total agent cost rollup (session accumulators); null for deterministic runs. */
  agent_summary: AgentSummary | null
}

/* ------------------------------------------------------------------ */
/* Plans, approvals, corpus (Phase 2; contract locked in RESEARCH.md). */
/* Plan/approval statuses are lowercase enums — unlike runs, these     */
/* mirror the DB enums directly on the wire. Timestamps RFC3339.       */
/* ------------------------------------------------------------------ */

export type PlanStatus = 'draft' | 'pending_approval' | 'approved' | 'superseded' | 'rejected'

export interface Plan {
  id: string
  app: string
  title: string
  surfaces: string[]
  trigger: string
  status: PlanStatus
  source_path: string
  latest_version_id: string | null
  updated_at: string
}

export interface PlanVersionSummary {
  id: string
  plan_id: string
  version: string
  content_hash: string
  status: PlanStatus
  created_by: string
  created_at: string
}

export interface PlansPage {
  plans: Plan[]
  limit: number
  offset: number
  next_offset: number | null
}

export interface PlanDetail {
  plan: Plan
  versions: PlanVersionSummary[]
}

export interface PlanVersionContent {
  content: string
  frontmatter: Record<string, unknown>
  content_hash: string
  status: PlanStatus
}

export interface PlanVersionDiff {
  diff: string
  /** Version the diff was computed against; null when no approved baseline. */
  base_version_id: string | null
}

/** Attributed decision on one plan version (approval-history row). */
export interface PlanApproval {
  id: string
  plan_id: string
  plan_version_id: string
  decision: 'approved' | 'rejected'
  approver: string
  notes: string | null
  decided_at: string
}

/** One item waiting in the approval queue: the changed version + its plan. */
export interface PendingApproval {
  plan: Plan
  version: PlanVersionSummary
}

export interface ApprovalsQueue {
  pending: PendingApproval[]
  recent: PlanApproval[]
}

/** Result of a sync from the plans source repo (syncAppPlans endpoint). */
export interface PlansSyncResult {
  synced: number
  added: number
  changed: number
  unchanged: number
  /** Plans that failed validation during sync; surfaced in the UI. */
  skipped: { path: string; reason: string }[]
}

/* --------------------------- Corpus ------------------------------- */

export type BehaviorStatus = 'unverified' | 'verified' | 'stale'
export type LearningStatus = 'unverified' | 'promoted' | 'dismissed'

export interface BehaviorProvenance {
  kind: 'playwright-spec' | 'run' | 'learning'
  path?: string
  commit?: string
  run_id?: string
  learning_id?: string
}

export interface CorpusBehavior {
  key: string
  title: string
  description: string
  status: BehaviorStatus
  provenance: BehaviorProvenance
  verified_by: string | null
  verified_at: string | null
}

export interface CorpusLearning {
  id: string
  run_id: string | null
  observation: string
  status: LearningStatus
}

export interface CorpusData {
  behaviors: CorpusBehavior[]
  learnings: CorpusLearning[]
}

/* ------------------------------------------------------------------ */
/* Phase 5 hardening (T6): flakes/quarantine, trends, alerts and the   */
/* agentic trajectory report extension. These mirror the Go DTOs in    */
/* api/internal/{flakes,trends,report} exactly (additive contract).    */
/* Timestamps RFC3339; rates are fractions in [0,1] unless noted.      */
/* ------------------------------------------------------------------ */

/* ------------------------------ Trajectory -------------------------------- */

/** One typed artifact citation ({kind, path} plus a token-gated page URL). */
export interface EvidenceRef {
  kind: string
  path: string
  /** Null when the path failed artifact-path sanitization. */
  url: string | null
}

/** One append-only trajectory entry (session_steps row). */
export interface TrajectoryStep {
  idx: number
  /** navigate | act | observe | judge | assert | error */
  kind: string
  /** The agent's raw decision payload, passed through verbatim. */
  decision: unknown
  /** The agent's raw action payload, passed through verbatim. */
  action: unknown
  evidence: EvidenceRef[]
  tokens_in: number
  tokens_out: number
  cost_millicents: number
  created_at: string
}

/** One agent session's full trajectory. */
export interface TrajectorySession {
  session_id: string
  role: string
  status: string
  /** Authoritative step count; `steps` may be tail-truncated on huge sessions. */
  steps_used: number
  cost_millicents: number
  truncated: boolean
  steps: TrajectoryStep[]
}

export interface AgentSummarySession {
  role: string
  status: string
  cost_millicents: number
}

export interface AgentSummary {
  sessions: AgentSummarySession[]
  total_cost_millicents: number
}

/* --------------------------- Flakes + quarantine -------------------------- */

/** One test's flake profile over the analysis window. */
export interface FlakeRate {
  app_id: string
  file: string
  title: string
  /** Runs (in window) where the test actually executed. */
  window_runs: number
  /** Runs whose report marked it outcome=flaky. */
  flaky_runs: number
  fail_runs: number
  pass_runs: number
  /** flaky+fail over window_runs; unstable tests excluded at the row level. */
  rate: number
  /** Both a pass run and a fail run exist in the window. */
  unstable: boolean
}

/** One test_quarantine row (active | lifted). */
export interface QuarantineEntry {
  id: string
  app_id: string
  file: string
  title: string
  reason: string
  evidence_artifact?: string
  status: 'active' | 'lifted'
  quarantined_by: string
  created_at: string
  expires_at?: string | null
  lifted_at?: string | null
  lifted_by?: string
}

/** Body of POST /api/quarantine (human-gated activation). */
export interface QuarantineActivateRequest {
  app_id: string
  file: string
  title: string
  reason: string
  evidence_artifact?: string
  /** RFC3339; empty/absent = no expiry (lift manually). */
  expires_at?: string
}

/* ------------------------------- Trends ----------------------------------- */

export interface FlipRate {
  app_id: string
  /** Completed runs analyzed (chronological). */
  window_runs: number
  /** Consecutive verdict changes. */
  flips: number
  /** Flips / max(WindowRuns-1, 1). */
  rate: number
}

export interface HistoryPoint {
  run_id: string
  /** pass | fail | flaky (DB-lowercase on this surface). */
  verdict: string
  passed: number
  failed: number
  flaky: number
  created_at: string
  workflow_run_id: number
}

export interface History {
  app_id: string
  window: number
  /** Oldest first. */
  points: HistoryPoint[]
  pass_runs: number
  fail_runs: number
}

export interface SessionCost {
  session_id: string
  role: string
  status: string
  steps: number
  tokens_in: number
  tokens_out: number
  cost_millicents: number
}

export interface RunCost {
  run_id: string
  sessions: SessionCost[]
  steps: number
  tokens_in: number
  tokens_out: number
  cost_millicents: number
}

/** One alert_events row (flake_threshold | flip_rate). */
export interface TrendAlert {
  id: string
  app_id: string
  kind: 'flake_threshold' | 'flip_rate'
  /** Kind-specific payload (thresholds, observed rates). */
  payload: Record<string, unknown>
  acknowledged: boolean
  created_at: string
}

export interface TrendAlerts {
  alerts: TrendAlert[]
}

/* ------------------------------------------------------------------ */
/* Cockpit (Phase 6; locked verbatim to api/internal/cockpit/http.go). */
/* Arm state of record — the single-row cockpit_state read model.      */
/* Timestamps RFC3339Nano; nullable fields are null while unpaused.    */
/* ------------------------------------------------------------------ */

export interface CockpitState {
  paused: boolean
  paused_reason: string | null
  paused_by: string | null
  paused_at: string | null
  updated_at: string
}
