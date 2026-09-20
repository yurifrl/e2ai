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
  trigger_source: 'WEBHOOK' | 'RECONCILE'
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
