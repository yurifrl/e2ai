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
