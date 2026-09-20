/**
 * Report viewer: live report assembled from the report-data endpoint.
 * Failure detail (message + evidence) first, then the full filmstrip of
 * per-test step timelines with screenshot links.
 */
import { Link, useParams } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { useMemo } from 'react'
import { getRunReport } from '../../lib/api'
import type { TestOutcome, TestResult } from '../../lib/types'
import { usePolling } from '../../lib/usePolling'
import { formatDuration, shortSha } from './format'
import { StatusChip, VerdictChip } from './chips'
import { TERMINAL_STATUS } from '../../lib/runStatus'

const REFRESH_MS = 5_000

export function RunReportPage() {
  const { id = '' } = useParams()
  const { t } = useI18n('e2ai')
  const loader = useMemo(() => () => getRunReport(id), [id])
  const { data, error, loading } = usePolling(
    loader,
    REFRESH_MS,
    true,
    (d) => !TERMINAL_STATUS[d.run.status],
  )

  if (loading && !data) return <p className="text-muted-foreground">{t('detail.loading')}</p>
  if (error) return <p className="text-destructive">{t('detail.loadError')}</p>
  if (!data) return <p className="text-muted-foreground">{t('detail.loading')}</p>

  const { run, counts, projects, failures, flaky, steps } = data

  return (
    <div className="space-y-6">
      <div>
        <Link to={`/e2ai/runs/${run.id}`} className="text-sm text-primary hover:underline">
          ← {t('detail.title')}
        </Link>
        <h1 className="mt-2 flex items-center gap-3 text-xl font-semibold">
          {t('report.title')} #{run.run_number}
          <StatusChip status={run.status} />
          {run.verdict ? <VerdictChip verdict={run.verdict} /> : null}
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          {shortSha(run.head_sha)} · {projects.join(', ')}
          {run.duration_ms != null ? ` · ${formatDuration(run.duration_ms)}` : ''}
        </p>
      </div>

      <div className="grid grid-cols-5 gap-3 text-sm">
        <Kpi label={t('counts.total')} value={counts.total_tests} />
        <Kpi label={t('counts.passed')} value={counts.passed} tone="text-emerald-600" />
        <Kpi label={t('counts.failed')} value={counts.failed} tone="text-red-600" />
        <Kpi label={t('counts.flaky')} value={counts.flaky} tone="text-amber-600" />
        <Kpi label={t('counts.skipped')} value={counts.skipped} />
      </div>

      {failures.length > 0 ? (
        <section>
          <h2 className="mb-2 font-medium">{t('report.failures')}</h2>
          <div className="space-y-4">
            {failures.map((failure) => (
              <FailureCard key={failure.id} failure={failure} />
            ))}
          </div>
        </section>
      ) : null}

      {flaky.length > 0 ? (
        <section>
          <h2 className="mb-2 font-medium">{t('report.flaky')}</h2>
          <div className="space-y-4">
            {flaky.map((flakyTest) => (
              <FailureCard key={flakyTest.id} failure={flakyTest} />
            ))}
          </div>
        </section>
      ) : null}

      <section>
        <h2 className="mb-2 font-medium">{t('report.filmstrip')}</h2>
        <div className="space-y-2">
          {steps.map((step) => (
            <div key={step.test_id} className="rounded-lg border p-3">
              <div className="flex items-center gap-2 text-sm">
                <OutcomeTag outcome={step.outcome} />
                <span className="font-medium">{step.title}</span>
                {step.duration_ms != null ? (
                  <span className="text-muted-foreground">{formatDuration(step.duration_ms)}</span>
                ) : null}
              </div>
              {step.screenshots.length > 0 ? (
                <div className="mt-2 flex flex-wrap gap-2">
                  {step.screenshots.map((shot) => (
                    <a
                      key={shot}
                      href={`/e2ai/artifacts/${shot}`}
                      target="_blank"
                      rel="noreferrer"
                      className="text-xs text-primary hover:underline"
                    >
                      {t('report.screenshot')} · {shot.split('/').pop()}
                    </a>
                  ))}
                </div>
              ) : null}
            </div>
          ))}
        </div>
      </section>
    </div>
  )
}

function FailureCard({ failure }: { failure: TestResult }) {
  return (
    <div className="rounded-lg border p-4">
      <div className="flex items-center gap-2 text-sm">
        <OutcomeTag outcome={failure.outcome} />
        <span className="font-medium">{failure.title}</span>
        <span className="text-muted-foreground">{failure.project}</span>
      </div>
      {failure.file ? (
        <p className="mt-1 font-mono text-xs text-muted-foreground">
          {failure.file}
          {failure.line != null ? `:${failure.line}` : ''}
        </p>
      ) : null}
      {failure.error_message ? (
        <pre className="mt-2 max-w-prose overflow-auto rounded bg-muted p-2 text-xs text-destructive">
          {failure.error_message}
        </pre>
      ) : null}
    </div>
  )
}

const OUTCOME_TONE: Record<TestOutcome, string> = {
  PASS: 'text-emerald-600 dark:text-emerald-400',
  FAIL: 'text-red-600 dark:text-red-400',
  FLAKY: 'text-amber-600 dark:text-amber-400',
  SKIPPED: 'text-muted-foreground',
}

function OutcomeTag({ outcome }: { outcome: TestOutcome }) {
  const { t } = useI18n('e2ai')
  return <span className={`font-medium ${OUTCOME_TONE[outcome]}`}>{t(`outcome.${outcome}`)}</span>
}

function Kpi({ label, value, tone }: { label: string; value: number; tone?: string }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={`text-lg font-semibold ${tone ?? ''}`}>{value}</div>
    </div>
  )
}
