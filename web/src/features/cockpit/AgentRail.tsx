/**
 * Agent rail (COCK-03 web): the live session list — one row per
 * non-terminal agent session, oldest heartbeats first (API ordering).
 * Presentational: the Command Center page owns the getCockpitAgents poll
 * and passes rows via props. Every row links to the session's Run Detail
 * route; stuck rows (API-computed, >90s heartbeat lapse — the same
 * predicate the admin queue renders) get the danger treatment. The status
 * dot pulses while a session is claimed/running (sketch `.st.busy`).
 */
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import type { CockpitAgentSession } from '../../lib/types'

/** Agent roles are fixed platform vocabulary (sketch agent-tag tokens). */
const ROLE_KEYS: Record<string, string> = {
  navigator: 'cockpit.rail.role.navigator',
  judge: 'cockpit.rail.role.judge',
  analyst: 'cockpit.rail.role.analyst',
}

const STATUS_TONE: Record<string, string> = {
  claimed: 'bg-primary',
  running: 'bg-primary',
}

export function AgentRail({
  sessions,
  error,
}: {
  sessions: CockpitAgentSession[] | null
  error: string | null
}) {
  const { t } = useI18n('e2ai')

  return (
    <section aria-label={t('cockpit.rail.title')} className="rounded border bg-card p-4">
      <h3 className="text-sm font-medium text-muted-foreground">{t('cockpit.rail.title')}</h3>

      <ul className="mt-3 space-y-1">
        {(sessions ?? []).map((session) => (
          <RailRow key={`${session.run_id}::${session.role}::${session.status}`} session={session} />
        ))}
      </ul>

      {sessions != null && sessions.length === 0 ? (
        <div className="mt-3">
          <p className="text-sm text-muted-foreground">{t('cockpit.rail.empty')}</p>
          <p className="mt-1 text-xs text-muted-foreground">{t('cockpit.rail.emptyBody')}</p>
        </div>
      ) : null}

      {error && sessions == null ? (
        <p className="mt-3 text-sm text-destructive">{t('cockpit.rail.loadError')}</p>
      ) : null}
    </section>
  )
}

function RailRow({ session }: { session: CockpitAgentSession }) {
  const { t } = useI18n('e2ai')

  const dotTone = session.stuck
    ? 'bg-red-500'
    : (STATUS_TONE[session.status] ?? 'bg-muted-foreground')

  const role = ROLE_KEYS[session.role] ? t(ROLE_KEYS[session.role]) : session.role

  return (
    <li>
      <Link
        to={`/e2ai/runs/${encodeURIComponent(session.run_id)}`}
        className="flex items-center gap-2 rounded px-1 py-1.5 hover:bg-muted/50"
      >
        <span
          aria-hidden
          className={`inline-block h-2 w-2 shrink-0 rounded-full ${dotTone} ${
            session.stuck ? '' : 'animate-pulse'
          }`}
        />
        <span className="min-w-0 truncate">
          <span className={`font-medium ${session.stuck ? 'text-destructive' : ''}`}>{role}</span>
          {session.stuck ? (
            <span className="ml-2 inline-block rounded bg-red-100 px-2 py-0.5 text-xs font-medium text-red-800 dark:bg-red-900 dark:text-red-200">
              {t('cockpit.rail.stuck')}
            </span>
          ) : null}
        </span>
        <span className="ml-auto font-mono text-xs text-muted-foreground tabular-nums">
          {session.heartbeat_age_seconds}s
        </span>
      </Link>
    </li>
  )
}
