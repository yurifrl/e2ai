/**
 * Trends (TREN-01): verdict-flip rate over the recent window, the pass/fail
 * history series, flake/flip alerts and the per-run agent cost rollup. All
 * reads (the trends API is GET-only) refresh every 30s while visible; the
 * cost panel is keyed to one run id, defaulting to the newest history point.
 */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import {
  acknowledgeAlert,
  getAlerts,
  getFlipRate,
  getHistory,
  getRunCost,
} from '../../lib/api'
import type { HistoryPoint, TrendAlert } from '../../lib/types'
import { usePolling } from '../../lib/usePolling'
import { formatTimestamp, shortSha } from '../runs/format'
import { useAppId } from './useAppId'

const REFRESH_MS = 30_000

export function TrendsPage() {
  const { t } = useI18n('e2ai')
  const appId = useAppId()
  const [showAcknowledged, setShowAcknowledged] = useState(false)
  const [costRunId, setCostRunId] = useState('')
  const [ackError, setAckError] = useState<string | null>(null)

  const loadFlips = useCallback(() => getFlipRate(appId ?? ''), [appId])
  const loadHistory = useCallback(
    () => getHistory(appId ?? ''),
    [appId],
  )
  const loadAlerts = useCallback(
    () => getAlerts(appId ?? '', !showAcknowledged),
    [appId, showAcknowledged],
  )
  const loadCost = useCallback(
    () => getRunCost(costRunId),
    [costRunId],
  )

  const flips = usePolling(loadFlips, REFRESH_MS, appId !== null)
  const history = usePolling(loadHistory, REFRESH_MS, appId !== null)
  const alerts = usePolling(loadAlerts, REFRESH_MS, appId !== null)
  const cost = usePolling(loadCost, REFRESH_MS, costRunId !== '')

  // Default the cost panel to the newest analyzed run once history lands.
  const [costSeeded, setCostSeeded] = useState(false)
  useEffect(() => {
    if (costSeeded || costRunId !== '' || !history.data) return
    const last = history.data.points[history.data.points.length - 1]
    if (!last) return
    setCostSeeded(true)
    setCostRunId(last.run_id)
  }, [costSeeded, costRunId, history.data])

  if (appId === null) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('trends.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('trends.detectingApp')}</p>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">{t('trends.title')}</h1>
        <span className="font-mono text-xs text-muted-foreground">{appId}</span>
      </div>

      <section className="grid gap-3 sm:grid-cols-3">
        <div className="rounded-lg border p-3">
          <div className="text-xs text-muted-foreground">{t('trends.flipRate')}</div>
          <div className="text-lg font-semibold">
            {flips.data ? `${Math.round(flips.data.rate * 100)}%` : '—'}
          </div>
          <div className="text-xs text-muted-foreground">
            {flips.data
              ? t('trends.flipDetail', {
                  flips: flips.data.flips,
                  runs: flips.data.window_runs,
                })
              : null}
          </div>
        </div>
        <div className="rounded-lg border p-3">
          <div className="text-xs text-muted-foreground">{t('trends.passRuns')}</div>
          <div className="text-lg font-semibold text-emerald-600 dark:text-emerald-400">
            {history.data?.pass_runs ?? '—'}
          </div>
        </div>
        <div className="rounded-lg border p-3">
          <div className="text-xs text-muted-foreground">{t('trends.failRuns')}</div>
          <div className="text-lg font-semibold text-red-600 dark:text-red-400">
            {history.data?.fail_runs ?? '—'}
          </div>
        </div>
      </section>

      <section className="rounded-lg border">
        <header className="flex items-center justify-between border-b px-4 py-3">
          <h2 className="text-sm font-medium">{t('trends.history')}</h2>
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <input
              type="checkbox"
              checked={showAcknowledged}
              onChange={event => setShowAcknowledged(event.target.checked)}
            />
            {t('trends.showAcknowledged')}
          </label>
        </header>
        {alerts.data && alerts.data.alerts.length > 0 ? (
          <div className="divide-y">
            {alerts.data.alerts.map(alert => (
              <AlertRow
                key={alert.id}
                alert={alert}
                onAcknowledge={async () => {
                  try {
                    await acknowledgeAlert(alert.id)
                    setAckError(null)
                  } catch (error) {
                    setAckError(error instanceof Error ? error.message : String(error))
                  }
                }}
              />
            ))}
          </div>
        ) : null}
        {ackError ? <p className="px-4 py-2 text-sm text-destructive">{ackError}</p> : null}
        {history.data && history.data.points.length > 0 ? (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                <th className="px-4 py-2">{t('trends.col.run')}</th>
                <th className="px-4 py-2">{t('trends.col.verdict')}</th>
                <th className="px-4 py-2">{t('counts.passed')}</th>
                <th className="px-4 py-2">{t('counts.failed')}</th>
                <th className="px-4 py-2">{t('counts.flaky')}</th>
                <th className="px-4 py-2">{t('trends.col.when')}</th>
              </tr>
            </thead>
            <tbody>
              {[...history.data.points].reverse().map(point => (
                <HistoryRow key={point.run_id} point={point} />
              ))}
            </tbody>
          </table>
        ) : (
          <p className="px-4 py-6 text-center text-sm text-muted-foreground">
            {t('trends.noHistory')}
          </p>
        )}
      </section>

      <section className="rounded-lg border">
        <header className="flex items-center gap-2 border-b px-4 py-3">
          <h2 className="text-sm font-medium">{t('trends.cost')}</h2>
          <input
            value={costRunId}
            onChange={event => setCostRunId(event.target.value.trim())}
            placeholder={t('trends.costRunPlaceholder')}
            className="w-80 rounded-md border px-2 py-1 font-mono text-xs"
          />
        </header>
        {cost.data ? (
          <CostTable
            runId={cost.data.run_id}
            sessions={cost.data.sessions}
            steps={cost.data.steps}
            tokensIn={cost.data.tokens_in}
            tokensOut={cost.data.tokens_out}
            costMillicents={cost.data.cost_millicents}
          />
        ) : (
          <p className="px-4 py-6 text-center text-sm text-muted-foreground">
            {cost.error ? t('trends.costError') : t('trends.costPickRun')}
          </p>
        )}
      </section>
    </div>
  )
}

const VERDICT_TONE: Record<string, string> = {
  pass: 'text-emerald-600 dark:text-emerald-400',
  fail: 'text-red-600 dark:text-red-400',
  flaky: 'text-amber-600 dark:text-amber-400',
}

function HistoryRow({ point }: { point: HistoryPoint }) {
  const { t } = useI18n('e2ai')
  return (
    <tr className="border-b last:border-b-0">
      <td className="px-4 py-2">
        <Link to={`/e2ai/runs/${point.run_id}`} className="text-primary hover:underline">
          #{point.workflow_run_id} {shortSha(point.run_id)}
        </Link>
      </td>
      <td className={`px-4 py-2 font-medium ${VERDICT_TONE[point.verdict] ?? ''}`}>
        {t(`verdict.${point.verdict.toUpperCase()}`)}
      </td>
      <td className="px-4 py-2">{point.passed}</td>
      <td className="px-4 py-2">{point.failed}</td>
      <td className="px-4 py-2">{point.flaky}</td>
      <td className="px-4 py-2 text-muted-foreground">{formatTimestamp(point.created_at)}</td>
    </tr>
  )
}

function AlertRow({ alert, onAcknowledge }: { alert: TrendAlert; onAcknowledge: () => void }) {
  const { t } = useI18n('e2ai')
  return (
    <div className="flex items-center justify-between px-4 py-3">
      <div>
        <span className="font-medium">{t(`trends.alert.${alert.kind}`)}</span>
        <span className="ml-2 text-xs text-muted-foreground">
          {formatTimestamp(alert.created_at)}
        </span>
        <pre className="mt-1 overflow-x-auto whitespace-pre-wrap text-xs text-muted-foreground">
          {JSON.stringify(alert.payload)}
        </pre>
      </div>
      {!alert.acknowledged ? (
        <button
          type="button"
          onClick={onAcknowledge}
          className="rounded-md border px-2 py-1 text-xs hover:bg-muted"
        >
          {t('trends.acknowledge')}
        </button>
      ) : (
        <span className="text-xs text-muted-foreground">{t('trends.acknowledged')}</span>
      )}
    </div>
  )
}

function CostTable(props: {
  runId: string
  sessions: {
    session_id: string
    role: string
    status: string
    steps: number
    tokens_in: number
    tokens_out: number
    cost_millicents: number
  }[]
  steps: number
  tokensIn: number
  tokensOut: number
  costMillicents: number
}) {
  const { t } = useI18n('e2ai')
  return (
    <div>
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-xs text-muted-foreground">
            <th className="px-4 py-2">{t('trends.col.session')}</th>
            <th className="px-4 py-2">{t('trends.col.role')}</th>
            <th className="px-4 py-2">{t('trends.col.steps')}</th>
            <th className="px-4 py-2">{t('trends.col.tokensIn')}</th>
            <th className="px-4 py-2">{t('trends.col.tokensOut')}</th>
            <th className="px-4 py-2">{t('trends.col.cost')}</th>
          </tr>
        </thead>
        <tbody>
          {props.sessions.map(session => (
            <tr key={session.session_id} className="border-b last:border-b-0">
              <td className="px-4 py-2 font-mono text-xs">{shortSha(session.session_id)}</td>
              <td className="px-4 py-2">{session.role}</td>
              <td className="px-4 py-2">{session.steps}</td>
              <td className="px-4 py-2">{session.tokens_in}</td>
              <td className="px-4 py-2">{session.tokens_out}</td>
              <td className="px-4 py-2">{formatMillicents(session.cost_millicents)}</td>
            </tr>
          ))}
          {props.sessions.length === 0 ? (
            <tr>
              <td colSpan={6} className="px-4 py-6 text-center text-sm text-muted-foreground">
                {t('trends.noSessions')}
              </td>
            </tr>
          ) : null}
        </tbody>
        <tfoot>
          <tr className="border-t font-medium">
            <td className="px-4 py-2">{t('trends.total')}</td>
            <td className="px-4 py-2" />
            <td className="px-4 py-2">{props.steps}</td>
            <td className="px-4 py-2">{props.tokensIn}</td>
            <td className="px-4 py-2">{props.tokensOut}</td>
            <td className="px-4 py-2">{formatMillicents(props.costMillicents)}</td>
          </tr>
        </tfoot>
      </table>
    </div>
  )
}

/** Agent costs accumulate in millicents; surface them as dollars+cents. */
function formatMillicents(millicents: number): string {
  return `$${(millicents / 100_000).toFixed(4)}`
}
