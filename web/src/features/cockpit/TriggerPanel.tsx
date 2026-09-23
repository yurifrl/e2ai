/**
 * Trigger panel (TRIG-01..05): the inline expanding panel under the pagehead
 * — one house-built component mounted three ways this phase: cockpit pagehead
 * (primary), PlanDetailPage ("Trigger run" with the version preselected), and
 * the RunDetailPage re-run pair. Step 1 is the pick form (app → approved plan
 * version → head source → optional reason, with the live pre-flight budget
 * strip); step 2 is the armed confirm block restating plan identity, run
 * shape, and budget before the single "Confirm trigger" POST (TRIG-03).
 *
 * The panel never owns mutations besides the trigger POST: pause and budget
 * state come from the cockpit's own read endpoints (no second pause surface),
 * and every blocked condition disables step 2 with its exact error-state copy
 * — never silently queued (plan 06-05 UI-SPEC disabled-with-reason matrix).
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import {
  getCockpitBudget,
  getCockpitState,
  getPlan,
  getPlanVersionContent,
  listPlans,
  listRuns,
  postCockpitTrigger,
  ApiError,
} from '../../lib/api'
import { usePolling } from '../../lib/usePolling'
import type {
  CockpitBudget,
  CockpitState,
  CockpitTriggerAck,
  Plan,
  PlanVersionSummary,
} from '../../lib/types'

const REFRESH_MS = 10_000

/** '' (plain) | 'reproduce' | 'retest' — the trigger.go mode wire value. */
export type TriggerMode = '' | 'reproduce' | 'retest'

/** The Run Detail re-run preset: the run being re-run supplies SHA/env/id. */
export interface SourceRunPreset {
  id: string
  head_sha: string
  environment: string
}

export interface TriggerPanelProps {
  /** App uuids the panel can target (feed-derived on the cockpit mount). */
  apps: string[]
  /** Initial app; null on the cockpit mount lets the user pick from `apps`. */
  appId: string | null
  /** Plan/run mounts lock the app picker to the context app. */
  lockApp?: boolean
  /** Run Detail re-run preset: preselects reproduce (pin + source SHA) or retest (latest head). */
  initialMode?: TriggerMode
  sourceRun?: SourceRunPreset | null
  /** Plan Detail preset: skip the plan fetch; versions come preloaded. */
  presetPlan?: Plan | null
  presetVersions?: PlanVersionSummary[] | null
  onClose: () => void
}

/** Count the plan's numbered steps (plans/parse.go parseSteps shape). */
function countSteps(content: string): number {
  return content.split('\n').filter((line) => /^\s*\d+\.\s/.test(line)).length
}

function formatMoney(millicents: number): string {
  return `$${(millicents / 100_000).toFixed(2)}`
}

export function TriggerPanel({
  apps,
  appId,
  lockApp = false,
  initialMode = '',
  sourceRun = null,
  presetPlan = null,
  presetVersions = null,
  onClose,
}: TriggerPanelProps) {
  const { t } = useI18n('e2ai')

  const [app, setApp] = useState<string | null>(appId)
  // Mode is fixed per mount (the cockpit picker offers plain only; the run
  // detail pair arms reproduce/retest) — only the head source can change.
  const mode = initialMode
  // Head source: 'latest' default; reproduce pins the original run's SHA.
  const [headSource, setHeadSource] = useState<'latest' | 'pin'>(
    initialMode === 'reproduce' ? 'pin' : 'latest',
  )
  const [pinSha, setPinSha] = useState(sourceRun?.head_sha ?? '')
  const [headBranch, setHeadBranch] = useState('')
  const [environment, setEnvironment] = useState(sourceRun?.environment ?? '')
  const [reason, setReason] = useState('')
  const [armed, setArmed] = useState(false)

  // Plan/version pick state.
  const [plans, setPlans] = useState<Plan[] | null>(presetPlan ? [presetPlan] : null)
  const [versions, setVersions] = useState<PlanVersionSummary[] | null>(
    presetVersions ?? null,
  )
  const [planId, setPlanId] = useState<string | null>(presetPlan?.id ?? null)
  const [versionId, setVersionId] = useState<string | null>(null)
  const [steps, setSteps] = useState<number | null>(null)
  const [planFetchError, setPlanFetchError] = useState<string | null>(null)

  const [submitting, setSubmitting] = useState(false)
  const [ack, setAck] = useState<CockpitTriggerAck | null>(null)
  const [submitError, setSubmitError] = useState<{ status: number; message: string } | null>(
    null,
  )

  // Arm-state poll: the paused warning adjacent to the budget strip.
  const { data: state } = usePolling<CockpitState>(
    useCallback(() => getCockpitState(), []),
    REFRESH_MS,
  )

  // Pre-flight budget strip: live poll scoped to the chosen app.
  const budgetLoader = useCallback(
    () => getCockpitBudget(app ?? ''),
    [app],
  )
  const { data: budget } = usePolling<CockpitBudget>(budgetLoader, REFRESH_MS, !!app)

  // Head prefill: the app's latest stored run supplies environment + head.
  // One-shot per app — stored runs don't churn fast enough to poll.
  useEffect(() => {
    if (!app || sourceRun) return
    let cancelled = false
    setPinSha('')
    setEnvironment('')
    setHeadBranch('')
    void listRuns(100)
      .then((page) => {
        if (cancelled) return
        const last = page.runs.find((r) => r.app_id === app)
        if (last) {
          setPinSha(last.head_sha)
          setHeadBranch(last.head_branch ?? '')
          setEnvironment(last.environment)
        }
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
  }, [app, sourceRun])

  // Cockpit mount: approved plans for the chosen app (listPlans filters by
  // owner/repo, the wire carries app uuids — filter client-side by app_id).
  useEffect(() => {
    if (presetPlan) {
      setPlans([presetPlan])
      return
    }
    if (!app) {
      setPlans(null)
      return
    }
    let cancelled = false
    setPlans(null)
    setPlanFetchError(null)
    void listPlans({ status: 'approved', limit: 100 })
      .then((page) => {
        if (cancelled) return
        const forApp = page.plans.filter((p) => p.app_id === app)
        setPlans(forApp)
        if (forApp.length === 1) setPlanId(forApp[0].id)
      })
      .catch((err: Error) => {
        if (cancelled) return
        setPlanFetchError(err.message)
      })
    return () => {
      cancelled = true
    }
  }, [app, presetPlan])

  // Versions for the chosen plan (skipped on the Plan Detail preset).
  useEffect(() => {
    if (presetVersions) {
      setVersions(presetVersions)
      return
    }
    if (!planId) {
      setVersions(null)
      return
    }
    let cancelled = false
    setVersions(null)
    setVersionId(null)
    void getPlan(planId)
      .then((detail) => {
        if (cancelled) return
        setVersions(detail.versions)
        const approved = detail.versions.filter((v) => v.status === 'approved')
        if (approved.length === 1) setVersionId(approved[0].id)
      })
      .catch((err: Error) => {
        if (cancelled) return
        setPlanFetchError(err.message)
      })
    return () => {
      cancelled = true
    }
  }, [planId, presetVersions])

  // Step count for the armed confirm: numbered steps in the version content.
  useEffect(() => {
    if (!versionId) {
      setSteps(null)
      return
    }
    let cancelled = false
    void getPlanVersionContent(versionId)
      .then((content) => {
        if (!cancelled) setSteps(countSteps(content.content))
      })
      .catch(() => {
        if (!cancelled) setSteps(null)
      })
    return () => {
      cancelled = true
    }
  }, [versionId])

  const selectedPlan = (plans ?? []).find((p) => p.id === planId) ?? null
  const approvedVersions = useMemo(
    () => (versions ?? []).filter((v) => v.status === 'approved'),
    [versions],
  )
  const zeroApproved = versions !== null && approvedVersions.length === 0
  const selectedVersion = approvedVersions.find((v) => v.id === versionId) ?? null

  const paused = !!state?.paused
  const budgetExhausted = !!budget?.refused
  const ceilingZero = !!budget?.capped && budget.cap_millicents === 0
  const budgetWarn = !!budget?.capped && !ceilingZero && !budgetExhausted

  // The effective head SHA: plain triggers send the pin (or latest prefill);
  // reproduce/retest send '' — the server sources the SHA itself (TRIG-04).
  const headSha = mode === '' && headSource === 'pin' ? pinSha.trim() : ''

  const canArm =
    !!app &&
    !!versionId &&
    environment.trim() !== '' &&
    (mode !== '' || headSource === 'latest' || /^[0-9a-f]{7,40}$/i.test(headSha))

  const armDisabled =
    !canArm || paused || budgetExhausted || ceilingZero || zeroApproved

  const submit = async () => {
    if (!app || !versionId) return
    setSubmitting(true)
    setSubmitError(null)
    try {
      const result = await postCockpitTrigger({
        app_id: app,
        head_sha: headSha,
        head_branch: headBranch || undefined,
        environment: environment.trim(),
        reason: reason.trim() || undefined,
        mode: mode || undefined,
        source_run_id: mode ? sourceRun?.id : undefined,
        plan_version_id: versionId,
      })
      setAck(result)
    } catch (err) {
      const status = err instanceof ApiError ? err.status : 500
      setSubmitError({ status, message: err instanceof Error ? err.message : String(err) })
    } finally {
      setSubmitting(false)
      setArmed(false)
    }
  }

  // Success line: a 202 turns the panel into the link to the created run.
  if (ack) {
    return (
      <section
        role="status"
        className="rounded-lg border border-emerald-600/40 bg-emerald-50 p-4 text-sm dark:bg-emerald-950 dark:border-emerald-700"
      >
        <Link
          to={`/e2ai/runs/${ack.run_id}`}
          className="font-medium text-emerald-700 hover:underline dark:text-emerald-300"
        >
          {t('cockpit.trigger.successOpen', { id: ack.workflow_run_id })}
        </Link>
        <button
          type="button"
          onClick={onClose}
          className="ml-3 text-muted-foreground underline hover:no-underline"
        >
          {t('cockpit.trigger.done')}
        </button>
      </section>
    )
  }

  return (
    <section
      aria-label={t('cockpit.trigger.title')}
      className="rounded-lg border bg-card p-4 text-sm"
    >
      <div className="flex items-center justify-between">
        <h2 className="font-medium">{t('cockpit.trigger.title')}</h2>
        <button
          type="button"
          onClick={onClose}
          className="text-muted-foreground hover:text-foreground"
          aria-label={t('cockpit.trigger.close')}
        >
          ✕
        </button>
      </div>

      {/* Step 1 — pick */}
      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <label className="block">
          <span className="text-xs text-muted-foreground">{t('cockpit.trigger.app')}</span>
          {lockApp ? (
            <input
              disabled
              value={app ? app.slice(0, 8) : ''}
              className="mt-0.5 w-full rounded border bg-muted px-2 py-1 font-mono text-xs"
            />
          ) : (
            <select
              value={app ?? ''}
              onChange={(e) => setApp(e.target.value || null)}
              className="mt-0.5 w-full rounded border bg-card px-2 py-1"
            >
              <option value="">{t('cockpit.trigger.appPlaceholder')}</option>
              {apps.map((id) => (
                <option key={id} value={id}>
                  {id.slice(0, 8)}
                </option>
              ))}
            </select>
          )}
        </label>

        {zeroApproved ? (
          <div className="sm:col-span-2 rounded border border-amber-600/40 bg-amber-50 p-3 dark:bg-amber-950 dark:border-amber-700 dark:text-amber-200">
            <p>{t('cockpit.trigger.zeroApproved', { app: app ? app.slice(0, 8) : '' })}</p>
            <Link
              to="/e2ai/approvals"
              className="mt-1.5 inline-block font-medium underline hover:no-underline"
            >
              {t('cockpit.trigger.openApprovals')}
            </Link>
          </div>
        ) : (
          <label className="block">
            <span className="text-xs text-muted-foreground">
              {t('cockpit.trigger.planVersion')}
            </span>
            {versions === null ? (
              <select disabled className="mt-0.5 w-full rounded border bg-muted px-2 py-1">
                <option value="">{t('cockpit.trigger.loadingVersions')}</option>
              </select>
            ) : planId === null ? (
              <select
                value=""
                onChange={(e) => setPlanId(e.target.value || null)}
                className="mt-0.5 w-full rounded border bg-card px-2 py-1"
              >
                <option value="">{t('cockpit.trigger.planPlaceholder')}</option>
                {(plans ?? []).map((plan) => (
                  <option key={plan.id} value={plan.id}>
                    {plan.title}
                  </option>
                ))}
              </select>
            ) : (
              <select
                value={versionId ?? ''}
                onChange={(e) => setVersionId(e.target.value || null)}
                className="mt-0.5 w-full rounded border bg-card px-2 py-1"
              >
                <option value="">{t('cockpit.trigger.versionPlaceholder')}</option>
                {versions!.map((version) => (
                  <option
                    key={version.id}
                    value={version.status === 'approved' ? version.id : ''}
                    disabled={version.status !== 'approved'}
                  >
                    {version.status === 'approved'
                      ? `v${version.version}`
                      : t('cockpit.trigger.draftRow')}
                  </option>
                ))}
              </select>
            )}
          </label>
        )}
      </div>

      {planFetchError ? (
        <p className="mt-2 text-xs text-destructive">{planFetchError}</p>
      ) : null}

      {/* Head source radio pair (plain mode only — reproduce pins the source
          SHA, retest forces latest head per TRIG-04 resolver semantics). */}
      {mode === '' && app ? (
        <div className="mt-3 flex flex-wrap items-center gap-4">
          <label className="flex items-center gap-1.5">
            <input
              type="radio"
              name="cockpit-trigger-head"
              checked={headSource === 'latest'}
              onChange={() => setHeadSource('latest')}
            />
            <span>{t('cockpit.trigger.headLatest')}</span>
          </label>
          <label className="flex items-center gap-1.5">
            <input
              type="radio"
              name="cockpit-trigger-head"
              checked={headSource === 'pin'}
              onChange={() => setHeadSource('pin')}
            />
            <span>{t('cockpit.trigger.headPin')}</span>
          </label>
          {headSource === 'pin' ? (
            <input
              value={pinSha}
              onChange={(e) => setPinSha(e.target.value)}
              placeholder="abc1234"
              aria-label={t('cockpit.trigger.headPin')}
              className="w-48 rounded border bg-card px-2 py-1 font-mono text-xs"
            />
          ) : null}
        </div>
      ) : null}

      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <label className="block">
          <span className="text-xs text-muted-foreground">
            {t('cockpit.trigger.environment')}
          </span>
          <input
            value={environment}
            onChange={(e) => setEnvironment(e.target.value)}
            className="mt-0.5 w-full rounded border bg-card px-2 py-1"
          />
        </label>
        <label className="block sm:col-span-2">
          <span className="text-xs text-muted-foreground">{t('cockpit.trigger.reason')}</span>
          <textarea
            value={reason}
            maxLength={200}
            rows={1}
            onChange={(e) => setReason(e.target.value)}
            placeholder={t('cockpit.trigger.reasonPlaceholder')}
            className="mt-0.5 w-full resize-none rounded border bg-card px-2 py-1"
          />
        </label>
      </div>

      {/* Pre-flight budget strip — always visible, live. */}
      {app ? (
        <div
          className={`mt-3 rounded border px-3 py-2 font-mono text-xs ${
            budgetExhausted || ceilingZero
              ? 'border-red-600/40 bg-red-50 text-red-700 dark:bg-red-950 dark:text-red-300'
              : budgetWarn
                ? 'border-amber-600/40 bg-amber-50 text-amber-700 dark:bg-amber-950 dark:text-amber-300'
                : 'bg-muted text-muted-foreground'
          }`}
        >
          {ceilingZero ? (
            t('cockpit.trigger.ceilingZero')
          ) : budgetExhausted ? (
            t('cockpit.trigger.budgetExhausted', {
              app: app.slice(0, 8),
              spend: formatMoney(budget?.spent_today ?? 0),
              cap: formatMoney(budget?.cap_millicents ?? 0),
            })
          ) : budget?.capped ? (
            t('cockpit.trigger.budgetStrip', {
              spend: formatMoney(budget.spent_today),
              cap: formatMoney(budget.cap_millicents),
              remaining: formatMoney(Math.max(budget.remaining_millicents, 0)),
            })
          ) : budget ? (
            t('cockpit.trigger.budgetNoCap')
          ) : (
            t('cockpit.trigger.loadingBudget')
          )}
          {budgetWarn ? ` — ${t('cockpit.trigger.budgetWarn')}` : null}
        </div>
      ) : null}

      {paused ? (
        <p
          role="status"
          className="mt-2 rounded border border-amber-600/40 bg-amber-50 px-3 py-2 text-xs dark:bg-amber-950 dark:border-amber-700 dark:text-amber-200"
        >
          {t('cockpit.trigger.paused')}
          {state?.paused_reason ? ` — “${state.paused_reason}”` : ''}
        </p>
      ) : null}

      {/* Step 2 — armed confirm block: restates plan identity, run shape,
          and budget before the single Confirm trigger POST (TRIG-03). */}
      {armed && app && selectedVersion ? (
        <div className="mt-3 rounded border p-3">
          <p className="font-medium">{t('cockpit.trigger.confirmTitle')}</p>
          <ul className="mt-2 space-y-1 text-xs text-muted-foreground">
            <li>
              {t('cockpit.trigger.confirmPlan', {
                name: selectedPlan?.title ?? app.slice(0, 8),
                version: selectedVersion.version,
              })}
              <span className="ml-1.5 rounded bg-muted px-1.5 py-0.5 font-mono">
                {t('cockpit.trigger.hashPinned')}
              </span>
            </li>
            <li>
              {t('cockpit.trigger.confirmShape', {
                steps: steps ?? '—',
                targets: selectedPlan?.surfaces.join(', ') ?? '—',
              })}
            </li>
            {budget?.capped && !ceilingZero ? (
              <li>
                {t('cockpit.trigger.confirmBudget', {
                  spend: formatMoney(budget.spent_today),
                  cap: formatMoney(budget.cap_millicents),
                  remaining: formatMoney(Math.max(budget.remaining_millicents, 0)),
                })}
              </li>
            ) : (
              <li>
                {t('cockpit.trigger.confirmBudgetUncapped', { app: app.slice(0, 8) })}
              </li>
            )}
          </ul>
        </div>
      ) : null}

      {submitError?.status === 409 ? (
        <p role="alert" className="mt-2 text-xs text-amber-700 dark:text-amber-300">
          {t('cockpit.trigger.conflict', { app: app ? app.slice(0, 8) : '' })}
        </p>
      ) : submitError ? (
        <p role="alert" className="mt-2 text-xs text-destructive">
          {t('cockpit.trigger.error', { message: submitError.message })}
        </p>
      ) : null}

      <div className="mt-3 flex items-center justify-end gap-2">
        <button
          type="button"
          onClick={onClose}
          className="rounded border px-3 py-1.5 text-xs hover:bg-muted"
        >
          {t('cockpit.trigger.cancel')}
        </button>
        {armed ? (
          <button
            type="button"
            disabled={armDisabled || submitting}
            onClick={() => void submit()}
            className="rounded bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground disabled:opacity-50"
          >
            {submitting ? t('cockpit.trigger.submitting') : t('cockpit.trigger.confirm')}
          </button>
        ) : (
          <button
            type="button"
            disabled={armDisabled}
            onClick={() => setArmed(true)}
            className="rounded bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground disabled:opacity-50"
          >
            {t('cockpit.trigger.review')}
          </button>
        )}
      </div>
    </section>
  )
}
