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
import type { RunDetail, ReportData, RunsPage } from './types'

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
