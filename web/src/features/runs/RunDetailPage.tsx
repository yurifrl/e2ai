/**
 * Run detail: header (status/verdict, commit, PR, duration) + test results
 * table + artifact index. Auto-refreshes every 5s while the run is still in
 * flight (pre-completed statuses), stops once terminal or infra-failed.
 * The Plan 05 re-run pair (reproduce same commit / retest latest head)
 * mounts the trigger panel pre-armed with this run as the source.
 */
import { useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useAuth, useI18n } from '@nsxbet/admin-sdk'
import { getRun } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'
import { TERMINAL_STATUS } from '../../lib/runStatus'
import type { TestOutcome } from '../../lib/types'
import { StatusChip, VerdictChip } from './chips'
import { formatDuration, formatTimestamp, shortSha } from './format'
import { TriggerPanel, type TriggerMode } from '../cockpit/TriggerPanel'

const REFRESH_MS = 5_000

const OUTCOME_TONE: Record<TestOutcome, string> = {
  PASS: 'text-emerald-600 dark:text-emerald-400',
  FAIL: 'text-red-600 dark:text-red-400',
  FLAKY: 'text-amber-600 dark:text-amber-400',
  SKIPPED: 'text-muted-foreground',
}

export function RunDetailPage() {
  const { id = '' } = useParams()
  const { t } = useI18n('e2ai')
  const { hasPermission } = useAuth()
  const canEdit = hasPermission('admin.e2ai.edit')
  const [rerunMode, setRerunMode] = useState<TriggerMode | null>(null)
  const loader = useMemo(() => () => getRun(id), [id])
  const { data, error, loading } = usePolling(
    loader,
    REFRESH_MS,
    true,
    (d) => !TERMINAL_STATUS[d.run.status],
  )

  if (loading && !data) return <p className="text-muted-foreground">{t('detail.loading')}</p>
  if (error) return <p className="text-destructive">{t('detail.loadError')}</p>
  if (!data) return <p className="text-muted-foreground">{t('detail.loading')}</p>

  const { run, test_results: tests, artifacts } = data
  const counts = run.counts

  return (
    <div className="space-y-6">
      <div>
        <Link to="/e2ai/runs" className="text-sm text-primary hover:underline">
          ← {t('runs.title')}
        </Link>
        <h1 className="mt-2 flex items-center gap-3 text-xl font-semibold">
          Run #{run.run_number}
          <StatusChip status={run.status} />
          {run.verdict ? <VerdictChip verdict={run.verdict} /> : null}
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          {shortSha(run.head_sha)}
          {run.head_branch ? ` · ${run.head_branch}` : ''}
          {run.pr_number != null ? ` · PR #${run.pr_number}` : ''}
          {' · '}
          {t(`env.label`)}: {run.environment}
        </p>
        {run.triggered_by ? (
          <p className="mt-1 text-sm text-muted-foreground">
            {t('detail.triggeredBy', {
              operator: run.triggered_by,
              at: formatTimestamp(run.started_at ?? run.created_at),
            })}
          </p>
        ) : null}
        {run.error_note ? <p className="mt-1 text-sm text-destructive">{run.error_note}</p> : null}
        {canEdit ? (
          <div className="mt-3 flex items-center gap-2">
            <button
              type="button"
              onClick={() => setRerunMode('reproduce')}
              aria-expanded={rerunMode === 'reproduce'}
              className={`rounded px-3 py-1 text-xs font-medium ${
                rerunMode === 'reproduce'
                  ? 'bg-muted text-muted-foreground hover:bg-muted/70'
                  : 'bg-primary text-primary-foreground hover:bg-primary/90'
              }`}
            >
              {t('detail.rerunReproduce')}
            </button>
            <button
              type="button"
              onClick={() => setRerunMode('retest')}
              aria-expanded={rerunMode === 'retest'}
              className={`rounded px-3 py-1 text-xs font-medium ${
                rerunMode === 'retest'
                  ? 'bg-muted text-muted-foreground hover:bg-muted/70'
                  : 'bg-primary text-primary-foreground hover:bg-primary/90'
              }`}
            >
              {t('detail.rerunRetest')}
            </button>
            <span className="text-xs text-muted-foreground">{t('detail.rerunSubline')}</span>
          </div>
        ) : null}
      </div>

      {rerunMode && canEdit ? (
        <TriggerPanel
          apps={[run.app_id]}
          appId={run.app_id}
          lockApp
          initialMode={rerunMode}
          sourceRun={{
            id: run.id,
            head_sha: run.head_sha,
            environment: run.environment,
          }}
          onClose={() => setRerunMode(null)}
        />
      ) : null}

      {counts ? (
        <div className="grid grid-cols-5 gap-3 text-sm">
          <Kpi label={t('counts.total')} value={counts.total_tests} />
          <Kpi label={t('counts.passed')} value={counts.passed} tone="text-emerald-600" />
          <Kpi label={t('counts.failed')} value={counts.failed} tone="text-red-600" />
          <Kpi label={t('counts.flaky')} value={counts.flaky} tone="text-amber-600" />
          <Kpi label={t('counts.skipped')} value={counts.skipped} />
        </div>
      ) : null}

      <div className="flex gap-2 text-sm">
        {run.duration_ms != null ? <span>{t('detail.duration')}: {formatDuration(run.duration_ms)}</span> : null}
        <span>{t('detail.started')}: {formatTimestamp(run.started_at ?? run.created_at)}</span>
        {run.report_url ? (
          <Link to={`/e2ai/runs/${run.id}/report`} className="text-primary hover:underline">
            {t('detail.openReport')}
          </Link>
        ) : null}
      </div>

      <section>
        <h2 className="mb-2 font-medium">{t('detail.tests')}</h2>
        {tests.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('detail.noTests')}</p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="py-2 pr-3 font-medium">{t('detail.col.outcome')}</th>
                <th className="py-2 pr-3 font-medium">{t('detail.col.title')}</th>
                <th className="py-2 pr-3 font-medium">{t('detail.col.project')}</th>
                <th className="py-2 pr-3 font-medium">{t('detail.col.location')}</th>
                <th className="py-2 font-medium">{t('detail.col.duration')}</th>
              </tr>
            </thead>
            <tbody>
              {tests.map((test) => (
                <tr key={test.id} className="border-b">
                  <td className={`py-2 pr-3 font-medium ${OUTCOME_TONE[test.outcome]}`}>
                    {t(`outcome.${test.outcome}`)}
                  </td>
                  <td className="py-2 pr-3">
                    {test.title}
                    {test.error_message ? (
                      <pre className="mt-1 max-w-prose overflow-auto rounded bg-muted p-2 text-xs text-destructive">
                        {test.error_message}
                      </pre>
                    ) : null}
                  </td>
                  <td className="py-2 pr-3">{test.project}</td>
                  <td className="py-2 pr-3 font-mono text-xs text-muted-foreground">
                    {test.file ? `${test.file}${test.line != null ? `:${test.line}` : ''}` : '—'}
                  </td>
                  <td className="py-2">{test.duration_ms != null ? formatDuration(test.duration_ms) : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {artifacts.length > 0 ? (
        <section>
          <h2 className="mb-2 font-medium">{t('detail.artifacts')}</h2>
          <ul className="space-y-1 text-sm">
            {artifacts.map((artifact) => (
              <li key={artifact.id}>
                <a
                  href={`/e2ai/artifacts/${artifact.path}`}
                  className="text-primary hover:underline"
                  target="_blank"
                  rel="noreferrer"
                >
                  [{artifact.kind}] {artifact.path}
                </a>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </div>
  )
}

function Kpi({ label, value, tone }: { label: string; value: number; tone?: string }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={`text-lg font-semibold ${tone ?? ''}`}>{value}</div>
    </div>
  )
}
