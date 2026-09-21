/**
 * Flake governance (FLAK-01/02): per-test flake rates over the analysis
 * window plus the quarantine queue — activate/lift are the human-gated
 * mutations over the test_quarantine store. Quarantining only flips the
 * store's own rows (the report stage reads them as the skip signal);
 * nothing here touches the app under test. Queue and rates refresh every
 * 30s while visible.
 */
import { useCallback, useState } from 'react'
import { useI18n } from '@nsxbet/admin-sdk'
import {
  activateQuarantine,
  getFlakeRates,
  getQuarantine,
  liftQuarantine,
} from '../../lib/api'
import type { FlakeRate, QuarantineEntry } from '../../lib/types'
import { usePolling } from '../../lib/usePolling'
import { formatTimestamp } from '../runs/format'
import { useAppId } from './useAppId'

const REFRESH_MS = 30_000

export function FlakesPage() {
  const { t } = useI18n('e2ai')
  const appId = useAppId()
  const [actionError, setActionError] = useState<string | null>(null)
  const [acting, setActing] = useState<string | null>(null)
  const [showLifted, setShowLifted] = useState(false)

  const loadRates = useCallback(() => getFlakeRates(appId ?? ''), [appId])
  const loadQueue = useCallback(
    () => getQuarantine(appId ?? '', showLifted ? undefined : 'active'),
    [appId, showLifted],
  )

  const rates = usePolling(loadRates, REFRESH_MS, appId !== null)
  const queue = usePolling(loadQueue, REFRESH_MS, appId !== null)

  const quarantine = async (rate: FlakeRate) => {
    setActing(`${rate.file}::${rate.title}`)
    try {
      await activateQuarantine({
        app_id: rate.app_id,
        file: rate.file,
        title: rate.title,
        reason: t('flakes.quarantineReason', { rate: `${Math.round(rate.rate * 100)}%` }),
      })
      setActionError(null)
    } catch (error) {
      setActionError(error instanceof Error ? error.message : String(error))
    } finally {
      setActing(null)
    }
  }

  const lift = async (entry: QuarantineEntry) => {
    setActing(entry.id)
    try {
      await liftQuarantine(entry.id)
      setActionError(null)
    } catch (error) {
      setActionError(error instanceof Error ? error.message : String(error))
    } finally {
      setActing(null)
    }
  }

  if (appId === null) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('flakes.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('trends.detectingApp')}</p>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">{t('flakes.title')}</h1>
        <span className="font-mono text-xs text-muted-foreground">{appId}</span>
      </div>

      {rates.error ? <p className="text-sm text-destructive">{t('flakes.loadError')}</p> : null}
      {actionError ? <p className="text-sm text-destructive">{actionError}</p> : null}

      <section className="rounded-lg border">
        <h2 className="border-b px-4 py-3 text-sm font-medium">{t('flakes.rates')}</h2>
        {rates.data && rates.data.length > 0 ? (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                <th className="px-4 py-2">{t('flakes.col.test')}</th>
                <th className="px-4 py-2">{t('flakes.col.rate')}</th>
                <th className="px-4 py-2">{t('flakes.col.flaky')}</th>
                <th className="px-4 py-2">{t('flakes.col.fail')}</th>
                <th className="px-4 py-2">{t('flakes.col.window')}</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {rates.data.map(rate => (
                <tr key={`${rate.file}::${rate.title}`} className="border-b last:border-b-0">
                  <td className="px-4 py-2">
                    <div className="font-medium">{rate.title}</div>
                    <div className="font-mono text-xs text-muted-foreground">{rate.file}</div>
                  </td>
                  <td className="px-4 py-2 font-semibold">
                    {Math.round(rate.rate * 100)}%
                    {rate.unstable ? (
                      <span className="ml-2 text-xs text-amber-600 dark:text-amber-400">
                        {t('flakes.unstable')}
                      </span>
                    ) : null}
                  </td>
                  <td className="px-4 py-2">{rate.flaky_runs}</td>
                  <td className="px-4 py-2">{rate.fail_runs}</td>
                  <td className="px-4 py-2 text-muted-foreground">{rate.window_runs}</td>
                  <td className="px-4 py-2 text-right">
                    <button
                      type="button"
                      disabled={acting !== null}
                      onClick={() => quarantine(rate)}
                      className="rounded-md border px-2 py-1 text-xs hover:bg-muted disabled:opacity-50"
                    >
                      {t('flakes.quarantine')}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <p className="px-4 py-6 text-center text-sm text-muted-foreground">
            {t('flakes.noRates')}
          </p>
        )}
      </section>

      <section className="rounded-lg border">
        <header className="flex items-center justify-between border-b px-4 py-3">
          <h2 className="text-sm font-medium">{t('flakes.queue')}</h2>
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <input
              type="checkbox"
              checked={showLifted}
              onChange={event => setShowLifted(event.target.checked)}
            />
            {t('flakes.showLifted')}
          </label>
        </header>
        {queue.error ? <p className="text-sm text-destructive">{t('flakes.loadError')}</p> : null}
        {queue.data && queue.data.length > 0 ? (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                <th className="px-4 py-2">{t('flakes.col.test')}</th>
                <th className="px-4 py-2">{t('flakes.col.status')}</th>
                <th className="px-4 py-2">{t('flakes.col.reason')}</th>
                <th className="px-4 py-2">{t('flakes.col.by')}</th>
                <th className="px-4 py-2">{t('flakes.col.expires')}</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {queue.data.map(entry => (
                <tr key={entry.id} className="border-b last:border-b-0">
                  <td className="px-4 py-2">
                    <div className="font-medium">{entry.title}</div>
                    <div className="font-mono text-xs text-muted-foreground">{entry.file}</div>
                  </td>
                  <td className="px-4 py-2">
                    <QuarantineChip status={entry.status} />
                  </td>
                  <td className="px-4 py-2 text-muted-foreground">{entry.reason}</td>
                  <td className="px-4 py-2 text-muted-foreground">
                    {entry.quarantined_by === 'agent'
                      ? t('flakes.byAgent')
                      : t('flakes.byHuman')}
                  </td>
                  <td className="px-4 py-2 text-muted-foreground">
                    {formatTimestamp(entry.expires_at ?? null)}
                  </td>
                  <td className="px-4 py-2 text-right">
                    {entry.status === 'active' ? (
                      <button
                        type="button"
                        disabled={acting !== null}
                        onClick={() => lift(entry)}
                        className="rounded-md border px-2 py-1 text-xs hover:bg-muted disabled:opacity-50"
                      >
                        {t('flakes.lift')}
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <p className="px-4 py-6 text-center text-sm text-muted-foreground">
            {t('flakes.noQueue')}
          </p>
        )}
      </section>
    </div>
  )
}

const QUARANTINE_TONE: Record<QuarantineEntry['status'], string> = {
  active: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
  lifted: 'bg-muted text-muted-foreground',
}

function QuarantineChip({ status }: { status: QuarantineEntry['status'] }) {
  const { t } = useI18n('e2ai')
  return (
    <span
      className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${QUARANTINE_TONE[status]}`}
    >
      {t(`flakes.status.${status}`)}
    </span>
  )
}
