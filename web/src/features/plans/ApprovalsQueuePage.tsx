/**
 * Approvals queue: pending plan versions awaiting a human decision, plus the
 * recent decision log. Approve/reject posts the attributed decision (notes
 * optional); the queue refreshes every 5s while visible. A 409 from the API
 * means the version was superseded while the reviewer was reading — surfaced
 * inline, the queue itself refreshes it away on the next tick.
 */
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { usePolling } from '../../lib/usePolling'
import {
  approvePlanVersion,
  getApprovalsQueue,
  rejectPlanVersion,
} from '../../lib/api'
import { formatTimestamp } from '../runs/format'
import { PlanContentPanel, VersionDiff } from './PlanContent'
import { PlanStatusChip } from './chips'
import type { PendingApproval } from '../../lib/types'

export function ApprovalsQueuePage() {
  const { t } = useI18n('e2ai')
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [showDiffId, setShowDiffId] = useState<string | null>(null)
  const [notes, setNotes] = useState<Record<string, string>>({})
  const [busyId, setBusyId] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const queue = usePolling(getApprovalsQueue, 5000)

  if (queue.error) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('approvals.title')}</h1>
        <p className="text-sm text-destructive">{t('approvals.loadError')}</p>
      </div>
    )
  }
  if (!queue.data) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('approvals.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('approvals.loading')}</p>
      </div>
    )
  }

  const { pending, recent } = queue.data

  const decide = async (entry: PendingApproval, decision: 'approve' | 'reject') => {
    setBusyId(entry.version.id)
    setActionError(null)
    try {
      const note = notes[entry.version.id]?.trim()
      if (decision === 'approve') {
        await approvePlanVersion(entry.version.id, note || undefined)
      } else {
        await rejectPlanVersion(entry.version.id, note || undefined)
      }
      setNotes((prev) => {
        const next = { ...prev }
        delete next[entry.version.id]
        return next
      })
    } catch (err) {
      if (err instanceof Error && err.message.includes('409')) {
        setActionError(t('approvals.superseded'))
      } else {
        setActionError(t('approvals.actionError'))
      }
    } finally {
      setBusyId(null)
    }
  }

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">{t('approvals.title')}</h1>

      <section>
        <h2 className="mb-2 font-medium">
          {t('approvals.pending')}{' '}
          <span className="text-sm text-muted-foreground">({pending.length})</span>
        </h2>
        {actionError ? <p className="mb-2 text-sm text-destructive">{actionError}</p> : null}
        {pending.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('approvals.empty')}</p>
        ) : (
          <div className="space-y-3">
            {pending.map((entry) => {
              const versionId = entry.version.id
              const expanded = expandedId === versionId
              const showDiff = showDiffId === versionId
              const busy = busyId === versionId
              return (
                <div key={versionId} className="rounded-lg border p-4">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="space-y-1">
                      <div className="flex items-center gap-2">
                        <Link
                          to={`/e2ai/plans/${entry.plan.id}`}
                          className="font-medium hover:underline"
                        >
                          {entry.plan.title}
                        </Link>
                        <PlanStatusChip status={entry.version.status} />
                      </div>
                      <p className="text-xs text-muted-foreground">
                        {entry.plan.app} · v{entry.version.version} ·{' '}
                        <span className="font-mono">{entry.version.content_hash.slice(0, 12)}</span>
                        {entry.version.created_by ? ` · ${entry.version.created_by}` : ''}
                      </p>
                    </div>
                    <div className="flex flex-wrap items-center gap-2">
                      <button
                        type="button"
                        className="rounded-md border px-2 py-1 text-xs hover:bg-muted"
                        onClick={() => setExpandedId(expanded ? null : versionId)}
                      >
                        {expanded ? t('plans.hideContent') : t('plans.showContent')}
                      </button>
                      <button
                        type="button"
                        className="rounded-md border px-2 py-1 text-xs hover:bg-muted"
                        onClick={() => setShowDiffId(showDiff ? null : versionId)}
                      >
                        {showDiff ? t('plans.hideDiff') : t('plans.showDiff')}
                      </button>
                      <input
                        type="text"
                        className="w-48 rounded-md border px-2 py-1 text-xs"
                        placeholder={t('approvals.notesPlaceholder')}
                        value={notes[versionId] ?? ''}
                        onChange={(e) =>
                          setNotes((prev) => ({ ...prev, [versionId]: e.target.value }))
                        }
                      />
                      <button
                        type="button"
                        className="rounded-md bg-emerald-600 px-3 py-1 text-xs font-medium text-white hover:bg-emerald-700 disabled:opacity-50"
                        disabled={busy}
                        onClick={() => void decide(entry, 'approve')}
                      >
                        {t('approvals.approve')}
                      </button>
                      <button
                        type="button"
                        className="rounded-md bg-red-600 px-3 py-1 text-xs font-medium text-white hover:bg-red-700 disabled:opacity-50"
                        disabled={busy}
                        onClick={() => void decide(entry, 'reject')}
                      >
                        {t('approvals.reject')}
                      </button>
                    </div>
                  </div>
                  {showDiff ? (
                    <div className="mt-3 border-t pt-3">
                      <VersionDiff versionId={versionId} />
                    </div>
                  ) : expanded ? (
                    <div className="mt-3 border-t pt-3">
                      <PlanContentPanel versionId={versionId} />
                    </div>
                  ) : null}
                </div>
              )
            })}
          </div>
        )}
      </section>

      <section>
        <h2 className="mb-2 font-medium">{t('approvals.recent')}</h2>
        {recent.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('approvals.noDecisions')}</p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-3 py-2 font-medium">{t('approvals.col.decision')}</th>
                <th className="px-3 py-2 font-medium">{t('approvals.col.approver')}</th>
                <th className="px-3 py-2 font-medium">{t('approvals.col.notes')}</th>
                <th className="px-3 py-2 font-medium">{t('approvals.col.when')}</th>
              </tr>
            </thead>
            <tbody>
              {recent.map((decision) => (
                <tr key={decision.id} className="border-b">
                  <td className="px-3 py-2">
                    <span
                      className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${
                        decision.decision === 'approved'
                          ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200'
                          : 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200'
                      }`}
                    >
                      {decision.decision === 'approved'
                        ? t('approvals.approved')
                        : t('approvals.rejected')}
                    </span>
                  </td>
                  <td className="px-3 py-2">{decision.approver}</td>
                  <td className="px-3 py-2 text-muted-foreground">{decision.notes ?? '—'}</td>
                  <td className="px-3 py-2 text-muted-foreground">
                    {formatTimestamp(decision.decided_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  )
}
