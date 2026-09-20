/**
 * Plans list: every synced plan, newest activity first, filterable by status.
 * Queue-relevant rows (pending_approval) are what approvals work starts from;
 * approved rows show what the pipeline currently executes.
 */
import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { listPlans } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'
import { formatTimestamp } from '../runs/format'
import { PlanStatusChip } from './chips'
import type { Plan, PlanStatus, PlansPage } from '../../lib/types'

const PAGE_SIZE = 50

type StatusFilter = 'all' | PlanStatus

const FILTERS: StatusFilter[] = [
  'all',
  'draft',
  'pending_approval',
  'approved',
  'superseded',
  'rejected',
]

function matchesFilter(plan: Plan, filter: StatusFilter): boolean {
  return filter === 'all' || plan.status === filter
}

export function PlansListPage() {
  const { t } = useI18n('e2ai')
  const [appending, setAppending] = useState(false)
  const [morePages, setMorePages] = useState<PlansPage[]>([])
  const [filter, setFilter] = useState<StatusFilter>('all')

  const firstPage = usePolling(() => listPlans({ limit: PAGE_SIZE }), 10_000)

  const effectivePages = useMemo(
    () => (firstPage.data ? [firstPage.data, ...morePages] : morePages),
    [firstPage.data, morePages],
  )
  const plans = effectivePages.flatMap((page) => page.plans)
  const filtered = useMemo(
    () => plans.filter((plan) => matchesFilter(plan, filter)),
    [plans, filter],
  )

  const lastPage = effectivePages[effectivePages.length - 1]
  const hasMore = lastPage?.next_offset != null

  const loadMore = async () => {
    if (lastPage?.next_offset == null) return
    setAppending(true)
    try {
      const next = await listPlans({ limit: PAGE_SIZE, offset: lastPage.next_offset })
      setMorePages((prev) => [...prev, next])
    } finally {
      setAppending(false)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">{t('plans.title')}</h1>
        {firstPage.error ? (
          <span className="text-sm text-destructive">{t('plans.loadError')}</span>
        ) : null}
      </div>

      <div className="flex flex-wrap gap-2">
        {FILTERS.map((option) => (
          <button
            key={option}
            type="button"
            className={`rounded-md border px-3 py-1 text-xs ${
              filter === option ? 'bg-primary text-primary-foreground' : 'hover:bg-muted'
            }`}
            onClick={() => setFilter(option)}
          >
            {t(option === 'all' ? 'plans.filter.all' : `plans.status.${option}`)}
          </button>
        ))}
      </div>

      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="py-2 pr-3 font-medium">{t('plans.col.plan')}</th>
            <th className="py-2 pr-3 font-medium">{t('plans.col.app')}</th>
            <th className="py-2 pr-3 font-medium">{t('plans.col.surfaces')}</th>
            <th className="py-2 pr-3 font-medium">{t('plans.col.trigger')}</th>
            <th className="py-2 pr-3 font-medium">{t('plans.col.status')}</th>
            <th className="py-2 font-medium">{t('plans.col.updated')}</th>
          </tr>
        </thead>
        <tbody>
          {filtered.map((plan) => (
            <tr key={plan.id} className="border-b hover:bg-muted/50">
              <td className="py-2 pr-3">
                <Link to={`/e2ai/plans/${encodeURIComponent(plan.id)}`} className="hover:underline">
                  {plan.title}
                </Link>
                <span className="ml-2 font-mono text-xs text-muted-foreground">{plan.id}</span>
              </td>
              <td className="py-2 pr-3 font-mono text-xs">{plan.app}</td>
              <td className="py-2 pr-3 text-xs">{plan.surfaces.join(', ')}</td>
              <td className="py-2 pr-3 text-xs">{plan.trigger}</td>
              <td className="py-2 pr-3"><PlanStatusChip status={plan.status} /></td>
              <td className="py-2 text-xs">{formatTimestamp(plan.updated_at)}</td>
            </tr>
          ))}
          {filtered.length === 0 && !firstPage.loading ? (
            <tr>
              <td colSpan={6} className="py-8 text-center text-muted-foreground">
                {t('plans.empty')}
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
            {appending ? t('plans.loading') : t('runs.loadMore')}
          </button>
        </div>
      ) : null}
    </div>
  )
}
