/**
 * Cockpit KPI header zone (COCK-04): the arm-state card — LIVE/PAUSED badge
 * at all times plus the attributed pause/unpause control — and the
 * full-width paused banner strip that derives from the same CockpitState
 * poll (no second fetch), plus the four Plan 04 data cards (runs today,
 * pass rate, budget, pending approvals). KPI data polls via the Command
 * Center page and arrives as props.
 *
 * Mutation contract (ACTN-06): server-ack wait, never optimistic — the
 * button spins until the API answers and the fresh row re-renders; API
 * errors surface verbatim below the control. Pause is two-step: step 1
 * "Pause arm" reveals the reason input; "Confirm pause" stays disabled
 * until a non-empty trimmed reason. Unpause is one step. Polling every
 * 10s while visible (RunsListPage analog).
 */
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { getCockpitState, pauseCockpit, unpauseCockpit } from '../../lib/api'
import { ApiError } from '../../lib/api'
import type { CockpitKpis } from '../../lib/types'
import { usePolling } from '../../lib/usePolling'
import { formatTimestamp } from '../runs/format'

const REFRESH_MS = 10_000
// Server-side mirror: reasons over 200 chars are refused with an error, so
// cap the input and keep the counter honest instead of provoking a 400.
const REASON_MAX = 200

export function KpiCards({
  kpis,
  kpisError,
}: {
  kpis: CockpitKpis | null
  kpisError: string | null
}) {
  const { t } = useI18n('e2ai')
  const { data: state, error: loadError } = usePolling(getCockpitState, REFRESH_MS)

  const [arming, setArming] = useState(false)
  const [reason, setReason] = useState('')
  const [acting, setActing] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)

  const paused = state?.paused ?? false

  const act = async (run: () => Promise<unknown>) => {
    setActing(true)
    try {
      await run()
      setActionError(null)
      setArming(false)
      setReason('')
    } catch (error) {
      setActionError(error instanceof ApiError ? error.message : String(error))
    } finally {
      setActing(false)
    }
  }

  const confirmPause = () =>
    act(() => pauseCockpit(reason.trim()))
  const resume = () =>
    act(() => unpauseCockpit())

  return (
    <section aria-label={t('cockpit.kpi.zone')} className="space-y-4">
      {paused ? (
        <div
          role="status"
          aria-live="polite"
          className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-900 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200"
        >
          <span className="font-medium">
            {state?.paused_reason
              ? t('cockpit.banner.pausedReason', { reason: state.paused_reason })
              : t('cockpit.banner.paused')}
          </span>
          {state?.paused_by ? (
            <span className="text-muted-foreground">
              {t('cockpit.banner.pausedBy', {
                operator: state.paused_by,
                timestamp: formatTimestamp(state.paused_at),
              })}
            </span>
          ) : null}
        </div>
      ) : null}

      {kpisError && kpis == null ? (
        <p className="text-sm text-destructive">{t('cockpit.kpi.kpisLoadError')}</p>
      ) : null}

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-5">
        <div className="rounded border bg-card p-4">
          <div className="flex items-center justify-between gap-2">
            <h3 className="text-sm font-medium text-muted-foreground">{t('cockpit.arm.title')}</h3>
            <span
              className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${
                paused
                  ? 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200'
                  : 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200'
              }`}
            >
              {paused ? (
                t('cockpit.arm.paused')
              ) : (
                <>
                  <span
                    aria-hidden
                    className="mr-1.5 inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-current"
                  />
                  {t('cockpit.arm.live')}
                </>
              )}
            </span>
          </div>

          {!paused ? (
            <div className="mt-3">
              {arming ? (
                <div className="space-y-2">
                  <input
                    type="text"
                    value={reason}
                    maxLength={REASON_MAX}
                    onChange={(event) => setReason(event.target.value)}
                    placeholder={t('cockpit.pause.reasonPlaceholder')}
                    aria-describedby="cockpit-pause-consequence"
                    className="w-full rounded border bg-transparent px-2 py-1.5 text-sm"
                  />
                  <div className="flex items-center gap-2">
                    <button
                      type="button"
                      disabled={acting || reason.trim().length === 0}
                      onClick={confirmPause}
                      className="rounded bg-amber-600 px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
                    >
                      {acting ? t('cockpit.pause.waiting') : t('cockpit.pause.confirm')}
                    </button>
                    <button
                      type="button"
                      disabled={acting}
                      onClick={() => {
                        setArming(false)
                        setReason('')
                      }}
                      className="rounded border px-3 py-1.5 text-sm"
                    >
                      {t('cockpit.pause.cancel')}
                    </button>
                  </div>
                  <p id="cockpit-pause-consequence" className="text-xs text-muted-foreground">
                    {t('cockpit.pause.consequence')}
                  </p>
                </div>
              ) : (
                <button
                  type="button"
                  disabled={acting}
                  onClick={() => setArming(true)}
                  className="rounded border border-amber-300 px-3 py-1.5 text-sm font-medium text-amber-800 dark:border-amber-700 dark:text-amber-200"
                >
                  {t('cockpit.pause.pause')}
                </button>
              )}
            </div>
          ) : (
            <div className="mt-3">
              <button
                type="button"
                disabled={acting}
                onClick={resume}
                className="rounded bg-emerald-600 px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
              >
                {acting ? t('cockpit.pause.waiting') : t('cockpit.pause.resume')}
              </button>
            </div>
          )}

          {actionError ? <p className="mt-2 text-sm text-destructive">{actionError}</p> : null}
          {loadError && state == null ? (
            <p className="mt-2 text-sm text-destructive">{t('cockpit.state.loadError')}</p>
          ) : null}
        </div>

        {/* Runs today — numeral only (the locked wire carries no yesterday
            aggregate, so the sketch's "vs yesterday" delta line is omitted). */}
        <div className="rounded border bg-card p-4">
          <h3 className="text-sm font-medium text-muted-foreground">
            {t('cockpit.kpi.runsToday')}
          </h3>
          <div className="mt-1 text-2xl font-semibold tabular-nums">
            {kpis ? kpis.runs_today : '—'}
          </div>
        </div>

        {/* Pass rate over completed runs — house verdict colors (pass = emerald). */}
        <div className="rounded border bg-card p-4">
          <h3 className="text-sm font-medium text-muted-foreground">{t('cockpit.kpi.passRate')}</h3>
          <div
            className={`mt-1 text-2xl font-semibold tabular-nums ${
              kpis && kpis.pass_rate_today >= 0.8
                ? 'text-emerald-600 dark:text-emerald-400'
                : kpis && kpis.pass_rate_today > 0
                  ? 'text-amber-600 dark:text-amber-400'
                  : ''
            }`}
          >
            {kpis ? <>{Math.round(kpis.pass_rate_today * 100)}<span className="text-sm">%</span></> : '—'}
          </div>
        </div>

        {/* Pending approvals — the whole card links to the queue; warning
            tone while anything is pending. */}
        <Link
          to="/e2ai/approvals"
          className={`block rounded border p-4 hover:bg-muted ${
            kpis && kpis.pending_approvals > 0
              ? 'border-amber-300 bg-amber-50 text-amber-900 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200'
              : 'bg-card'
          }`}
        >
          <h3 className="text-sm font-medium text-muted-foreground">
            {t('cockpit.kpi.pendingApprovals')}
          </h3>
          <div className="mt-1 text-2xl font-semibold tabular-nums">
            {kpis ? kpis.pending_approvals : '—'}
          </div>
        </Link>

        {/* Budget — spend against the sum of resolved caps; uncapped apps
            surface as a count, and "no cap" is explicit, never a fabricated
            zero ceiling (CONTEXT decision 1). Warning tone ≥80% utilization. */}
        <div
          className={`rounded border bg-card p-4 ${
            kpis && kpis.budget.caps_sum > 0 && kpis.budget.spent_today >= kpis.budget.caps_sum
              ? 'border-red-300 dark:border-red-700'
              : kpis &&
                  kpis.budget.caps_sum > 0 &&
                  kpis.budget.spent_today / kpis.budget.caps_sum >= 0.8
                ? 'border-amber-300 dark:border-amber-700'
                : ''
          }`}
        >
          <h3 className="text-sm font-medium text-muted-foreground">{t('cockpit.kpi.budget')}</h3>
          <div className="mt-1 font-mono text-lg font-semibold tabular-nums">
            {kpis ? (
              kpis.budget.caps_sum > 0 ? (
                t('cockpit.kpi.budgetSpendCap', {
                  spend: formatMillicents(kpis.budget.spent_today),
                  cap: formatMillicents(kpis.budget.caps_sum),
                })
              ) : (
                <span className={kpis.budget.uncapped_count > 0 ? 'text-amber-600 dark:text-amber-400' : ''}>
                  {t('cockpit.kpi.budgetNoCap', { count: kpis.budget.uncapped_count })}
                </span>
              )
            ) : (
              '—'
            )}
          </div>
          {kpis && kpis.budget.caps_sum > 0 && kpis.budget.uncapped_count > 0 ? (
            <p className="mt-1 text-xs text-muted-foreground">
              {t('cockpit.kpi.budgetUncappedNote', { count: kpis.budget.uncapped_count })}
            </p>
          ) : null}
        </div>
      </div>
    </section>
  )
}

/** Budgets accumulate in millicents; surface them as dollars+cents. */
function formatMillicents(millicents: number): string {
  return `$${(millicents / 100_000).toFixed(4)}`
}
