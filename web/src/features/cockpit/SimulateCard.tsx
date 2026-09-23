/**
 * Webhook simulate card (ACTN-01): the right-rail debug control that fires a
 * properly-signed synthetic webhook at the real ingest seam — the same
 * HMAC-verified handler GitHub events travel, rendered from the chosen app's
 * registration facts (simulate.go simulatePayloadShape). One flavor exists
 * today: `workflow_run` — the run-creating shape; the option list grows with
 * real ingest event kinds, never invented ones.
 *
 * Non-destructive debug actuation (not the arm under test): single click, no
 * arm-confirm. Result is a request/response block — outcome line per the
 * ingest seam's real vocabulary (accepted / rejected / duplicate), with an
 * "open run #{id}" link on acceptance. No polling; the feed shows the
 * ingested event on its next tick.
 */
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { postCockpitSimulate, ApiError } from '../../lib/api'
import type { CockpitSimulateAck } from '../../lib/types'

/** Ingest event kinds the seam actually accepts (ingest.HandleEvent). */
const PAYLOAD_SHAPES = ['workflow_run'] as const

export function SimulateCard({ apps }: { apps: string[] }) {
  const { t } = useI18n('e2ai')

  const [app, setApp] = useState<string | null>(null)
  const [payloadShape, setPayloadShape] = useState<string>(PAYLOAD_SHAPES[0])
  const [firing, setFiring] = useState(false)
  const [ack, setAck] = useState<CockpitSimulateAck | null>(null)
  const [error, setError] = useState<{ status: number; message: string } | null>(null)

  const fire = async () => {
    if (!app) return
    setFiring(true)
    setError(null)
    try {
      const result = await postCockpitSimulate({
        AppID: app,
        PayloadShape: payloadShape,
      })
      setAck(result)
    } catch (err) {
      const status = err instanceof ApiError ? err.status : 500
      setError({ status, message: err instanceof Error ? err.message : String(err) })
    } finally {
      setFiring(false)
    }
  }

  return (
    <section
      aria-label={t('cockpit.simulate.title')}
      className="rounded-lg border bg-card p-4 text-sm"
    >
      <h2 className="font-medium">{t('cockpit.simulate.title')}</h2>

      {apps.length === 0 ? (
        <p className="mt-2 text-xs text-muted-foreground">{t('cockpit.simulate.noApps')}</p>
      ) : (
        <>
          <label className="mt-3 block">
            <span className="text-xs text-muted-foreground">{t('cockpit.simulate.app')}</span>
            <select
              value={app ?? ''}
              onChange={(e) => {
                setApp(e.target.value || null)
                setAck(null)
                setError(null)
              }}
              className="mt-0.5 w-full rounded border bg-card px-2 py-1"
            >
              <option value="">{t('cockpit.trigger.appPlaceholder')}</option>
              {apps.map((id) => (
                <option key={id} value={id}>
                  {id.slice(0, 8)}
                </option>
              ))}
            </select>
          </label>

          <label className="mt-3 block">
            <span className="text-xs text-muted-foreground">
              {t('cockpit.simulate.payloadShape')}
            </span>
            <select
              value={payloadShape}
              onChange={(e) => {
                setPayloadShape(e.target.value)
                setAck(null)
                setError(null)
              }}
              className="mt-0.5 w-full rounded border bg-card px-2 py-1"
            >
              {PAYLOAD_SHAPES.map((shape) => (
                <option key={shape} value={shape}>
                  {shape}
                </option>
              ))}
            </select>
            <span className="mt-1 block text-xs text-muted-foreground">
              {t('cockpit.simulate.shapeNote')}
            </span>
          </label>

          {ack ? (
            <div
              role="status"
              className={`mt-3 rounded border px-3 py-2 text-xs ${
                ack.outcome === 'accepted'
                  ? 'border-emerald-600/40 bg-emerald-50 dark:bg-emerald-950 dark:border-emerald-700 dark:text-emerald-300'
                  : 'bg-muted text-muted-foreground'
              }`}
            >
              {ack.outcome === 'accepted' ? (
                <>
                  {t('cockpit.simulate.accepted')}{' '}
                  <Link
                    to={`/e2ai/runs/${ack.run_id}`}
                    className="font-medium underline hover:no-underline"
                  >
                    {t('cockpit.simulate.openRun', { id: ack.workflow_run_id })}
                  </Link>
                </>
              ) : ack.outcome === 'rejected' ? (
                t('cockpit.simulate.rejected')
              ) : (
                t('cockpit.simulate.duplicate')
              )}
            </div>
          ) : null}

          {error ? (
            <p role="alert" className="mt-3 text-xs text-destructive">
              {t('cockpit.simulate.error', { message: error.message })}
            </p>
          ) : null}

          <button
            type="button"
            disabled={!app || firing}
            onClick={() => void fire()}
            className="mt-3 w-full rounded bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground disabled:opacity-50"
          >
            {firing ? t('cockpit.simulate.firing') : t('cockpit.simulate.cta')}
          </button>

          {!ack && !error ? (
            <p className="mt-2 text-xs text-muted-foreground">{t('cockpit.simulate.hint')}</p>
          ) : null}
        </>
      )}
    </section>
  )
}
