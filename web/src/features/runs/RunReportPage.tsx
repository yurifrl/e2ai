/**
 * Report viewer: live report assembled from the report-data endpoint.
 * Failure detail (message + evidence) first, then the full filmstrip of
 * per-test step timelines with screenshot links, then the agentic
 * trajectory (phase 5, T3) when the run carried agent sessions.
 */
import { Link, useParams } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { useMemo } from 'react'
import { getRunReport } from '../../lib/api'
import type { TrajectorySession, TestOutcome, TestResult } from '../../lib/types'
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

      {data.trajectory && data.trajectory.length > 0 ? (
        <section>
          <div className="mb-2 flex items-baseline gap-3">
            <h2 className="font-medium">{t('report.trajectory')}</h2>
            {data.agent_summary ? (
              <span className="text-xs text-muted-foreground">
                {t('report.agentCost', { cost: formatCost(data.agent_summary.total_cost_millicents) })}
              </span>
            ) : null}
          </div>
          <div className="space-y-4">
            {data.trajectory.map((session) => (
              <TrajectoryCard key={session.session_id} session={session} />
            ))}
          </div>
        </section>
      ) : null}
    </div>
  )
}

/** Renders a jsonb payload (decision/action) as a one-line JSON preview. */
function compactJson(payload: unknown): string {
  if (payload == null) return ''
  if (typeof payload === 'string') return payload
  try {
    return JSON.stringify(payload)
  } catch {
    return String(payload)
  }
}

function formatCost(millicents: number): string {
  return `$${(millicents / 100_000).toFixed(4)}`
}

const STEP_KIND_TONE: Record<string, string> = {
  navigate: 'text-sky-600 dark:text-sky-400',
  act: 'text-violet-600 dark:text-violet-400',
  observe: 'text-muted-foreground',
  judge: 'text-amber-600 dark:text-amber-400',
  assert: 'text-emerald-600 dark:text-emerald-400',
  error: 'text-red-600 dark:text-red-400',
}

function TrajectoryCard({ session }: { session: TrajectorySession }) {
  const { t } = useI18n('e2ai')
  return (
    <div className="rounded-lg border">
      <div className="flex flex-wrap items-center gap-3 border-b px-4 py-3 text-sm">
        <span className="font-medium">{session.role}</span>
        <span className="text-muted-foreground">{session.status}</span>
        <span className="text-muted-foreground">
          {session.steps_used} {t('trajectory.steps')}
          {session.truncated ? ` · ${t('trajectory.truncated')}` : ''}
        </span>
        <span className="font-mono text-xs text-muted-foreground">{session.session_id}</span>
        <span className="ml-auto font-mono text-xs">{formatCost(session.cost_millicents)}</span>
      </div>
      <ol className="divide-y">
        {session.steps.map((step) => (
          <li key={step.idx} className="px-4 py-2 text-sm">
            <div className="flex items-baseline gap-2">
              <span className="font-mono text-xs text-muted-foreground">#{step.idx}</span>
              <span className={`text-xs font-medium ${STEP_KIND_TONE[step.kind] ?? ''}`}>
                {step.kind}
              </span>
            </div>
            {compactJson(step.decision) ? (
              <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                {compactJson(step.decision)}
              </p>
            ) : null}
            {compactJson(step.action) ? (
              <p className="mt-1 break-all font-mono text-xs">{compactJson(step.action)}</p>
            ) : null}
            {step.evidence.length > 0 ? (
              <div className="mt-1 flex flex-wrap gap-2">
                {step.evidence.map((ref, i) => (
                  <EvidenceLink key={`${ref.path}:${i}`} ref_={ref} />
                ))}
              </div>
            ) : null}
            <div className="mt-1 font-mono text-[10px] text-muted-foreground">
              {step.tokens_in}/{step.tokens_out} tok · {formatCost(step.cost_millicents)}
            </div>
          </li>
        ))}
      </ol>
    </div>
  )
}

function EvidenceLink({ ref_ }: { ref_: { kind: string; path: string; url: string | null } }) {
  const { t } = useI18n('e2ai')
  const label = `${t('trajectory.evidence')} · ${ref_.kind} · ${ref_.path.split('/').pop()}`
  if (ref_.url) {
    return (
      <a href={ref_.url} target="_blank" rel="noreferrer" className="text-xs text-primary hover:underline">
        {label}
      </a>
    )
  }
  return <span className="text-xs text-muted-foreground">{label}</span>
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
