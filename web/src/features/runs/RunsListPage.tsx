/**
 * Runs list: newest-first table of pipeline runs with status/verdict chips,
 * per-test counts and duration. "Load more" pages via next_offset; the first
 * page auto-refreshes every 10s while visible (report-only surface).
 */
import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { listRuns } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'
import type { Run, RunsPage } from '../../lib/types'
import { StatusChip, VerdictChip } from './chips'
import { formatDuration, formatTimestamp, shortSha } from './format'

const PAGE_SIZE = 50
const REFRESH_MS = 10_000

export function RunsListPage() {
  const { t } = useI18n('e2ai')
  const [pages, setPages] = useState<RunsPage[]>([])
  const [appending, setAppending] = useState(false)

  const loader = useMemo(() => () => listRuns(PAGE_SIZE, 0), [])
  const firstPage = usePolling(loader, REFRESH_MS)

  // The polling hook owns page 1; accumulated "load more" pages ride along.
  const runs: Run[] = useMemo(() => {
    const head = firstPage.data ? [firstPage.data] : pages.slice(0, 1)
    const rest = pages.length > 1 ? pages.slice(1) : []
    return [...head, ...rest].flatMap((p) => p.runs)
  }, [firstPage.data, pages])

  // Keep accumulated pages coherent with a refreshed page 1.
  const effectivePages = useMemo(
    () => (firstPage.data ? [firstPage.data, ...pages.slice(1)] : pages),
    [firstPage.data, pages],
  )

  const lastPage = effectivePages[effectivePages.length - 1]
  const hasMore = lastPage?.next_offset != null

  const loadMore = async () => {
    if (lastPage?.next_offset == null) return
    setAppending(true)
    try {
      const next = await listRuns(PAGE_SIZE, lastPage.next_offset)
      setPages([...effectivePages, next])
    } finally {
      setAppending(false)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">{t('runs.title')}</h1>
        {firstPage.error ? (
          <span className="text-sm text-destructive">{t('runs.loadError')}</span>
        ) : null}
      </div>

      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="py-2 pr-3 font-medium">#</th>
            <th className="py-2 pr-3 font-medium">{t('runs.col.status')}</th>
            <th className="py-2 pr-3 font-medium">{t('runs.col.verdict')}</th>
            <th className="py-2 pr-3 font-medium">{t('runs.col.commit')}</th>
            <th className="py-2 pr-3 font-medium">{t('runs.col.env')}</th>
            <th className="py-2 pr-3 font-medium">{t('runs.col.tests')}</th>
            <th className="py-2 pr-3 font-medium">{t('runs.col.duration')}</th>
            <th className="py-2 font-medium">{t('runs.col.started')}</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => (
            <tr key={run.id} className="border-b hover:bg-muted/50">
              <td className="py-2 pr-3 font-mono">
                <Link to={`/e2ai/runs/${run.id}`} className="hover:underline">
                  {run.run_number}
                </Link>
              </td>
              <td className="py-2 pr-3"><StatusChip status={run.status} /></td>
              <td className="py-2 pr-3">
                {run.verdict ? <VerdictChip verdict={run.verdict} /> : <span>—</span>}
              </td>
              <td className="py-2 pr-3 font-mono">
                {shortSha(run.head_sha)}
                {run.head_branch ? <span className="text-muted-foreground"> · {run.head_branch}</span> : null}
              </td>
              <td className="py-2 pr-3">{run.environment}</td>
              <td className="py-2 pr-3 font-mono">
                {run.counts && run.counts.total_tests > 0
                  ? `${run.counts.passed}✓ ${run.counts.failed}✕ ${run.counts.flaky}~ ${run.counts.skipped}−`
                  : '—'}
              </td>
              <td className="py-2 pr-3">{run.duration_ms != null ? formatDuration(run.duration_ms) : '—'}</td>
              <td className="py-2">{formatTimestamp(run.started_at ?? run.created_at)}</td>
            </tr>
          ))}
          {runs.length === 0 && !firstPage.loading ? (
            <tr>
              <td colSpan={8} className="py-8 text-center text-muted-foreground">
                {t('runs.empty')}
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>

      {hasMore ? (
        <div className="flex justify-center">
          <button
            type="button"
            className="rounded-md border px-4 py-2 text-sm hover:bg-muted"
            disabled={appending}
            onClick={() => void loadMore()}
          >
            {appending ? t('runs.loading') : t('runs.loadMore')}
          </button>
        </div>
      ) : null}
    </div>
  )
}
