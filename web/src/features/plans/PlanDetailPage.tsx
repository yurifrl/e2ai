/**
 * Plan detail: header metadata, full version history, and the rendered
 * markdown of the latest (or selected) version with a diff view against the
 * approval baseline. Approval decisions live in the approvals queue; this
 * page is read-only.
 */
import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { getPlan } from '../../lib/api'
import { formatTimestamp } from '../runs/format'
import { PlanContentPanel, VersionDiff } from './PlanContent'
import { PlanStatusChip } from './chips'
import type { PlanDetail } from '../../lib/types'

export function PlanDetailPage() {
  const { id } = useParams()
  const { t } = useI18n('e2ai')
  const [detail, setDetail] = useState<PlanDetail | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [selectedVersionId, setSelectedVersionId] = useState<string | null>(null)
  const [showDiff, setShowDiff] = useState(false)

  useEffect(() => {
    if (!id) return
    let cancelled = false
    getPlan(id)
      .then((data) => {
        if (cancelled) return
        setDetail(data)
        setSelectedVersionId(data.plan.latest_version_id)
      })
      .catch(() => {
        if (!cancelled) setError(t('plans.loadError'))
      })
    return () => {
      cancelled = true
    }
  }, [id, t])

  if (error) {
    return (
      <div className="space-y-2">
        <p className="text-sm text-destructive">{error}</p>
        <Link to="/e2ai/plans" className="text-sm hover:underline">
          {t('plans.backToList')}
        </Link>
      </div>
    )
  }
  if (!detail) return <p className="text-sm text-muted-foreground">{t('plans.loading')}</p>

  const { plan, versions } = detail
  const selected = selectedVersionId ?? plan.latest_version_id

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <Link to="/e2ai/plans" className="text-sm text-muted-foreground hover:underline">
          {t('plans.backToList')}
        </Link>
        <div className="flex items-center justify-between">
          <h1 className="text-xl font-semibold">{plan.title}</h1>
          <PlanStatusChip status={plan.status} />
        </div>
        <p className="text-sm text-muted-foreground">
          <span className="font-mono">{plan.app}</span>
          {' · '}
          {plan.surfaces.join(', ')}
          {' · '}
          {plan.trigger}
        </p>
      </div>

      <section>
        <h2 className="mb-2 font-medium">{t('plans.versions')}</h2>
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-3 font-medium">{t('plans.col.version')}</th>
              <th className="py-2 pr-3 font-medium">{t('plans.col.hash')}</th>
              <th className="py-2 pr-3 font-medium">{t('plans.col.status')}</th>
              <th className="py-2 pr-3 font-medium">{t('plans.col.createdBy')}</th>
              <th className="py-2 font-medium">{t('plans.col.created')}</th>
            </tr>
          </thead>
          <tbody>
            {versions.map((version) => (
              <tr
                key={version.id}
                className={`border-b hover:bg-muted/50 ${
                  version.id === selected ? 'bg-muted/50' : ''
                }`}
              >
                <td className="py-2 pr-3">
                  <button
                    type="button"
                    className="font-mono hover:underline"
                    onClick={() => setSelectedVersionId(version.id)}
                  >
                    v{version.version}
                  </button>
                </td>
                <td className="py-2 pr-3 font-mono text-xs">{version.content_hash.slice(0, 12)}</td>
                <td className="py-2 pr-3"><PlanStatusChip status={version.status} /></td>
                <td className="py-2 pr-3 text-xs">{version.created_by}</td>
                <td className="py-2 text-xs">{formatTimestamp(version.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="font-medium">{t('plans.content')}</h2>
          {selected ? (
            <button
              type="button"
              className="rounded-md border px-3 py-1 text-xs hover:bg-muted"
              onClick={() => setShowDiff((current) => !current)}
            >
              {showDiff ? t('plans.hideDiff') : t('plans.showDiff')}
            </button>
          ) : null}
        </div>
        {showDiff && selected ? (
          <VersionDiff versionId={selected} />
        ) : selected ? (
          <PlanContentPanel versionId={selected} />
        ) : null}
      </section>
    </div>
  )
}
