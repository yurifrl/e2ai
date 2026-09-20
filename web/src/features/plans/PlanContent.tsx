/**
 * Plan markdown rendering panel shared by the plans detail page and the
 * approvals queue: the frontmatter grid + numbered steps view of one plan
 * version.
 */
import { useEffect, useState } from 'react'
import { useI18n } from '@nsxbet/admin-sdk'
import { getPlanVersionContent, getPlanVersionDiff } from '../../lib/api'
import type { PlanVersionContent, PlanVersionDiff } from '../../lib/types'
import { renderPlanView } from './planView'
import { PlanStatusChip } from './chips'

function FieldValue({ value }: { value: string }) {
  return <span className="font-mono text-xs">{value}</span>
}

/** Frontmatter + steps view of one plan version's markdown. */
export function PlanContentPanel({ versionId }: { versionId: string }) {
  const { t } = useI18n('e2ai')
  const [content, setContent] = useState<PlanVersionContent | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setContent(null)
    setError(null)
    getPlanVersionContent(versionId)
      .then((data) => {
        if (!cancelled) setContent(data)
      })
      .catch(() => {
        if (!cancelled) setError(t('plans.loadContentError'))
      })
    return () => {
      cancelled = true
    }
  }, [versionId, t])

  if (error) return <p className="text-sm text-destructive">{error}</p>
  if (!content) return <p className="text-sm text-muted-foreground">{t('plans.loading')}</p>

  const view = renderPlanView(content.content, content.frontmatter)

  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
        {view.frontmatter.map(([key, value]) => (
          <div key={key} className="contents">
            <dt className="text-muted-foreground">{key}</dt>
            <dd className="min-w-0 break-all">
              <FieldValue value={value} />
            </dd>
          </div>
        ))}
      </dl>

      <ol className="space-y-3 text-sm">
        {view.steps.map((step) => (
          <li key={step.number} className="border-b pb-3">
            <div className="flex gap-2">
              <span className="font-mono text-muted-foreground">{step.number}.</span>
              <span>{step.intent}</span>
            </div>
            {step.judge ? (
              <div className="mt-1 pl-6 text-xs text-muted-foreground">
                judge: <FieldValue value={step.judge} />
              </div>
            ) : null}
            {step.uses ? (
              <div className="mt-1 pl-6 text-xs text-muted-foreground">
                uses: <FieldValue value={step.uses} />
              </div>
            ) : null}
            {step.onFailure ? (
              <div className="mt-1 pl-6 text-xs text-muted-foreground">
                on-failure: <FieldValue value={step.onFailure} />
              </div>
            ) : null}
          </li>
        ))}
        {view.steps.length === 0 ? (
          <li className="text-muted-foreground">{t('plans.noSteps')}</li>
        ) : null}
      </ol>

      <div className="flex items-center gap-3 text-xs text-muted-foreground">
        <PlanStatusChip status={content.status} />
        <span className="font-mono">{content.content_hash.slice(0, 12)}</span>
      </div>
    </div>
  )
}

/** Unified diff of one version against its approval baseline (fetch + render). */
export function VersionDiff({ versionId }: { versionId: string }) {
  const { t } = useI18n('e2ai')
  const [diff, setDiff] = useState<PlanVersionDiff | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setDiff(null)
    setError(null)
    getPlanVersionDiff(versionId)
      .then((data) => {
        if (!cancelled) setDiff(data)
      })
      .catch(() => {
        if (!cancelled) setError(t('plans.loadDiffError'))
      })
    return () => {
      cancelled = true
    }
  }, [versionId, t])

  if (error) return <p className="text-sm text-destructive">{error}</p>
  if (!diff) return <p className="text-sm text-muted-foreground">{t('plans.loading')}</p>

  return (
    <div className="space-y-2 text-sm">
      {diff.base_version_id == null ? (
        <p className="text-xs text-muted-foreground">{t('plans.diffNoBaseline')}</p>
      ) : (
        <p className="text-xs text-muted-foreground">
          {t('plans.diffAgainst')}{' '}
          <span className="font-mono">{diff.base_version_id.slice(0, 8)}</span>
        </p>
      )}
      <pre className="max-h-96 overflow-auto rounded bg-muted p-3 font-mono text-xs leading-relaxed">
        {diff.diff === '' ? t('plans.diffEmpty') : diff.diff}
      </pre>
    </div>
  )
}
