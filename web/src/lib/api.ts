/**
 * Minimal fetch client for the e2ai API surface (contract locked with the API
 * owner). Endpoints, mounted plain on the e2ai API and reached through the BFF
 * prefix `/e2ai` (stripped before forwarding):
 *
 *   GET /api/runs?limit=&offset=      → RunsPage  (paginated newest-first)
 *   GET /api/runs/{id}                → RunDetail
 *   GET /api/runs/{id}/report         → ReportData
 *   GET /artifacts/{path...}          → raw artifact bytes
 *
 *   GET /api/cockpit/state            → CockpitState (arm state of record)
 *   POST /api/cockpit/pause           → CockpitState (200 after commit; 403/400)
 *   POST /api/cockpit/unpause         → CockpitState (same contract)
 *
 * Non-2xx responses throw a typed ApiError carrying the body's error field.
 */
import { authenticatedFetch } from '../shared/authenticatedFetch'
import { e2aiApiBase } from '../shared/gateway'
import type {
  RunDetail,
  ReportData,
  RunsPage,
  PlanStatus,
  PlansPage,
  PlanDetail,
  PlanVersionContent,
  PlanVersionDiff,
  ApprovalsQueue,
  PlanApproval,
  PlansSyncResult,
  CorpusData,
  CorpusBehavior,
  CorpusLearning,
  FlakeRate,
  QuarantineActivateRequest,
  QuarantineEntry,
  FlipRate,
  History,
  RunCost,
  TrendAlerts,
  CockpitState,
  CockpitAgentSession,
  CockpitBudget,
  CockpitFeedPage,
  CockpitKpis,
  CockpitTriggerRequest,
  CockpitTriggerAck,
  CockpitSimulateRequest,
  CockpitSimulateAck,
} from './types'

export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function getJson<T>(path: string): Promise<T> {
  const response = await authenticatedFetch(`${e2aiApiBase()}${path}`)

  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`
    try {
      const body = (await response.json()) as { error?: string }
      if (body.error) message = body.error
    } catch {
      // HTML/plain error pages: keep the status line.
    }
    throw new ApiError(response.status, message)
  }

  return (await response.json()) as T
}

export function listRuns(limit = 50, offset = 0): Promise<RunsPage> {
  return getJson(`/api/runs?limit=${limit}&offset=${offset}`)
}

export function getRun(id: string): Promise<RunDetail> {
  return getJson(`/api/runs/${encodeURIComponent(id)}`)
}

export function getRunReport(id: string): Promise<ReportData> {
  return getJson(`/api/runs/${encodeURIComponent(id)}/report`)
}

/* ------------------------------------------------------------------ */
/* Phase 2: plans, approvals, corpus. Mutations go through postJson    */
/* (authenticated; 409 on superseded approvals surfaces as ApiError).  */
/* ------------------------------------------------------------------ */

async function postJson<T>(path: string, body: unknown): Promise<T> {
  const response = await authenticatedFetch(`${e2aiApiBase()}${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })

  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`
    try {
      const errBody = (await response.json()) as { error?: string }
      if (errBody.error) message = errBody.error
    } catch {
      // HTML/plain error pages: keep the status line.
    }
    throw new ApiError(response.status, message)
  }

  return (await response.json()) as T
}

export function listPlans(filters: {
  app?: string
  status?: PlanStatus
  limit?: number
  offset?: number
} = {}): Promise<PlansPage> {
  const params = new URLSearchParams()
  if (filters.app) params.set('app', filters.app)
  if (filters.status) params.set('status', filters.status)
  params.set('limit', String(filters.limit ?? 50))
  params.set('offset', String(filters.offset ?? 0))
  return getJson(`/api/plans?${params.toString()}`)
}

export function getPlan(id: string): Promise<PlanDetail> {
  return getJson(`/api/plans/${encodeURIComponent(id)}`)
}

export function getPlanVersionContent(versionId: string): Promise<PlanVersionContent> {
  return getJson(`/api/plan-versions/${encodeURIComponent(versionId)}/content`)
}

export function getPlanVersionDiff(
  versionId: string,
  baseVersionId?: string,
): Promise<PlanVersionDiff> {
  const suffix = baseVersionId ? `?base_version_id=${encodeURIComponent(baseVersionId)}` : ''
  return getJson(`/api/plan-versions/${encodeURIComponent(versionId)}/diff${suffix}`)
}

export function getApprovalsQueue(): Promise<ApprovalsQueue> {
  return getJson('/api/approvals')
}

export function approvePlanVersion(
  versionId: string,
  notes?: string,
): Promise<PlanApproval> {
  return postJson(`/api/plan-versions/${encodeURIComponent(versionId)}/approve`, { notes })
}

export function rejectPlanVersion(
  versionId: string,
  notes?: string,
): Promise<PlanApproval> {
  return postJson(`/api/plan-versions/${encodeURIComponent(versionId)}/reject`, { notes })
}

export function syncAppPlans(app: string): Promise<PlansSyncResult> {
  return postJson(`/api/apps/${encodeURIComponent(app)}/plans-sync`, {})
}

export function getCorpus(app: string): Promise<CorpusData> {
  return getJson(`/api/corpus?app=${encodeURIComponent(app)}`)
}

export function promoteLearning(learningId: string): Promise<CorpusBehavior> {
  return postJson(`/api/corpus/learnings/${encodeURIComponent(learningId)}/promote`, {})
}

export function dismissLearning(learningId: string): Promise<CorpusLearning> {
  return postJson(`/api/corpus/learnings/${encodeURIComponent(learningId)}/dismiss`, {})
}

/* ------------------------------------------------------------------ */
/* Phase 5: flake governance (rates + quarantine), trends (flips,      */
/* history, cost, alerts) and trajectory consumption. Quarantine       */
/* mutations are the human-gated queue actions; 409 means the test is   */
/* already quarantined.                                                */
/* ------------------------------------------------------------------ */

export function getFlakeRates(appId: string, window?: number): Promise<FlakeRate[]> {
  const w = window ? `&window=${window}` : ''
  return getJson(`/api/flakes?app_id=${encodeURIComponent(appId)}${w}`)
}

export function getQuarantine(
  appId: string,
  status?: 'active' | 'lifted',
): Promise<QuarantineEntry[]> {
  const s = status ? `&status=${status}` : ''
  return getJson(`/api/quarantine?app_id=${encodeURIComponent(appId)}${s}`)
}

export function activateQuarantine(
  request: QuarantineActivateRequest,
): Promise<QuarantineEntry> {
  return postJson('/api/quarantine', request)
}

export function liftQuarantine(id: string): Promise<QuarantineEntry> {
  return postJson(`/api/quarantine/${encodeURIComponent(id)}/lift`, {})
}

export function getFlipRate(appId: string, window?: number): Promise<FlipRate> {
  const w = window ? `&window=${window}` : ''
  return getJson(`/api/trends/flips?app_id=${encodeURIComponent(appId)}${w}`)
}

export function getHistory(appId: string, window?: number): Promise<History> {
  const w = window ? `&window=${window}` : ''
  return getJson(`/api/trends/history?app_id=${encodeURIComponent(appId)}${w}`)
}

export function getRunCost(runId: string): Promise<RunCost> {
  return getJson(`/api/trends/cost/${encodeURIComponent(runId)}`)
}

export function getAlerts(appId: string, unacknowledgedOnly = true): Promise<TrendAlerts> {
  const u = unacknowledgedOnly ? 'true' : 'false'
  return getJson(`/api/trends/alerts?app_id=${encodeURIComponent(appId)}&unacknowledged=${u}`)
}

export function acknowledgeAlert(id: string): Promise<{ acknowledged: boolean }> {
  return postJson(`/api/trends/alerts/${encodeURIComponent(id)}/acknowledge`, {})
}

/* Cockpit arm state (COCK-04): the pause control reads this row and both
 * mutations answer with the fresh state, so the button spins until the API
 * answers and the card re-renders from the ack — never optimistic. The
 * pause body is {reason}; the route itself picks the direction (locked to
 * api/internal/cockpit/http.go).
 */
export function getCockpitState(): Promise<CockpitState> {
  return getJson('/api/cockpit/state')
}

export function pauseCockpit(reason?: string): Promise<CockpitState> {
  return postJson('/api/cockpit/pause', { reason })
}

export function unpauseCockpit(): Promise<CockpitState> {
  return postJson('/api/cockpit/unpause', {})
}

/* Cockpit read models (Plan 03 endpoints, GET /api/cockpit/*). The app
 * filter is the literal UUID of a registered app; feed pagination is the
 * server's opaque keyset cursor. */

export function getCockpitFeed(opts?: {
  limit?: number
  appId?: string
  cursor?: string
}): Promise<CockpitFeedPage> {
  const params = new URLSearchParams()
  if (opts?.limit) params.set('limit', String(opts.limit))
  if (opts?.appId) params.set('app_id', opts.appId)
  if (opts?.cursor) params.set('cursor', opts.cursor)
  const qs = params.toString()
  return getJson(`/api/cockpit/feed${qs ? `?${qs}` : ''}`)
}

export function getCockpitKpis(appId?: string): Promise<CockpitKpis> {
  return getJson(`/api/cockpit/kpis${appId ? `?app_id=${encodeURIComponent(appId)}` : ''}`)
}

export function getCockpitAgents(appId?: string): Promise<CockpitAgentSession[]> {
  return getJson(`/api/cockpit/agents${appId ? `?app_id=${encodeURIComponent(appId)}` : ''}`)
}

export function getCockpitBudget(appId: string): Promise<CockpitBudget> {
  return getJson(`/api/cockpit/budget?app_id=${encodeURIComponent(appId)}`)
}

/* Manual trigger (TRIG-01..05, Plan 05): the ONLY success state is the
 * server's 202 ack — no optimistic UI. Refusals (409 pause/budget/active-run/
 * unapproved-plan, 422 validation) arrive as ApiError with the server's
 * specific reason text, rendered verbatim in the panel. */
export function postCockpitTrigger(req: CockpitTriggerRequest): Promise<CockpitTriggerAck> {
  return postJson('/api/cockpit/triggers', req)
}

/* Webhook simulate (ACTN-01/02, Plan 05): same ack-only posture. The body's
 * keys are the server's Go field names (SimulateRequest has no json tags) —
 * see CockpitSimulateRequest in types.ts. */
export function postCockpitSimulate(req: CockpitSimulateRequest): Promise<CockpitSimulateAck> {
  return postJson('/api/cockpit/simulate', req)
}
