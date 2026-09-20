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
  CorpusLearning,
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

export function promoteLearning(learningId: string): Promise<CorpusLearning> {
  return postJson(`/api/corpus/learnings/${encodeURIComponent(learningId)}/promote`, {})
}

export function dismissLearning(learningId: string): Promise<CorpusLearning> {
  return postJson(`/api/corpus/learnings/${encodeURIComponent(learningId)}/dismiss`, {})
}
