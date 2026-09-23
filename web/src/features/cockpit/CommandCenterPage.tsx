/**
 * Command Center (COCK-01/02/03 web half): the cockpit's page composition —
 * KPI header on top, then the sketch 001-A two-column body: the live feed
 * (reverse-chronological, keyset-paginated, app-filterable, rows expand
 * inline) on the left and the agent rail on the right. The page owns the
 * polls for its read endpoints; the arm-state card inside KpiCards keeps its
 * own state poll (Plan 01) so the pause control stays a single source.
 *
 * Report-only surface: everything here renders polled API truth — no
 * optimistic state. The Plan 05 mutation surfaces hang off this composition:
 * the trigger panel (TRIG-01..05) expands under the pagehead behind the
 * admin.e2ai.edit permission gate, and the webhook simulate card (ACTN-01)
 * lives in the right rail.
 */
import { useCallback, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useAuth, useI18n } from '@nsxbet/admin-sdk'
import { KpiCards } from './KpiCards'
import { AgentRail } from './AgentRail'
import { TriggerPanel } from './TriggerPanel'
import { SimulateCard } from './SimulateCard'
import { StatusChip } from '../runs/chips'
import { formatTimestamp } from '../runs/format'
import type { RunStatus } from '../../lib/types'
import type { CockpitFeedEvent, CockpitFeedPage } from '../../lib/types'
import { getCockpitAgents, getCockpitFeed, getCockpitKpis } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'

const REFRESH_MS = 10_000
const PAGE_SIZE = 50

/** Wire run-status → render tone; feed summaries end in these words. */
type RunTone = 'live' | 'final'

const RUN_TONE: Record<RunStatus, RunTone> = {
  QUEUED: 'live',
  WAITING_ROLLOUT: 'live',
  PROVISIONING: 'live',
  EXECUTING: 'live',
  REPORTING: 'live',
  COMPLETED: 'final',
  FAILED: 'final',
}

export function CommandCenterPage() {
  const { t } = useI18n('e2ai')
  const [appFilter, setAppFilter] = useState<string | null>(null)
  const [openId, setOpenId] = useState<string | null>(null)
  const [older, setOlder] = useState<CockpitFeedPage[]>([])
  const [appending, setAppending] = useState(false)
  const [triggerOpen, setTriggerOpen] = useState(false)
  const { hasPermission } = useAuth()
  const canEdit = hasPermission('admin.e2ai.edit')

  // The loaders key on the app filter: a changed identity restarts the poll
  // against the narrowed endpoints (usePolling effect deps).
  const feedLoader = useCallback(
    () => getCockpitFeed({ limit: PAGE_SIZE, appId: appFilter ?? undefined }),
    [appFilter],
  )
  const kpisLoader = useCallback(() => getCockpitKpis(), [])
  const railLoader = useCallback(
    () => getCockpitAgents(appFilter ?? undefined),
    [appFilter],
  )

  const {
    data: firstPage,
    error: feedError,
    loading: feedLoading,
  } = usePolling<CockpitFeedPage>(feedLoader, REFRESH_MS)
  const { data: kpis, error: kpisError } = usePolling(kpisLoader, REFRESH_MS)
  const { data: sessions, error: railError } = usePolling(railLoader, REFRESH_MS)

  const pages = useMemo(
    () => (firstPage ? [firstPage, ...older] : []),
    [firstPage, older],
  )
  const entries = useMemo(() => pages.flatMap((p) => p.entries), [pages])
  const cursor = pages.length ? pages[pages.length - 1].cursor : undefined

  // Chip row: the apps visible in the polled feed. The wire carries app
  // uuids (registry naming is a later phase), so chips show a short label.
  const apps = useMemo(() => {
    const seen = new Set<string>()
    for (const e of entries) seen.add(e.app_id)
    return [...seen]
  }, [entries])

  const loadMore = async () => {
    if (!cursor || appending) return
    setAppending(true)
    try {
      const next = await getCockpitFeed({
        limit: PAGE_SIZE,
        appId: appFilter ?? undefined,
        cursor,
      })
      setOlder((prev) => [...prev, next])
    } finally {
      setAppending(false)
    }
  }

  const switchFilter = (id: string | null) => {
    setAppFilter(id)
    setOlder([])
    setOpenId(null)
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">{t('cockpit.page.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('cockpit.page.subline')}</p>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {canEdit ? (
            <button
              type="button"
              onClick={() => setTriggerOpen((open) => !open)}
              aria-expanded={triggerOpen}
              className={`rounded px-3 py-1 text-xs font-medium ${
                triggerOpen
                  ? 'bg-muted text-muted-foreground hover:bg-muted/70'
                  : 'bg-primary text-primary-foreground hover:bg-primary/90'
              }`}
            >
              {t('cockpit.trigger.cta')}
            </button>
          ) : null}
          <AppChip
            label={t('cockpit.feed.allApps')}
            active={appFilter == null}
            onClick={() => switchFilter(null)}
          />
          {apps.map((id) => (
            <AppChip
              key={id}
              label={id.slice(0, 8)}
              active={appFilter === id}
              onClick={() => switchFilter(id)}
            />
          ))}
        </div>
      </div>

      {triggerOpen && canEdit ? (
        <TriggerPanel
          apps={apps}
          appId={appFilter}
          onClose={() => setTriggerOpen(false)}
        />
      ) : null}

      <KpiCards kpis={kpis ?? null} kpisError={kpisError?.message ?? null} />

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[1fr_320px]">
        <FeedSection
          entries={entries}
          cursor={!!cursor}
          loading={feedLoading}
          hasError={!!feedError}
          hasData={entries.length > 0}
          openId={openId}
          appending={appending}
          onToggle={setOpenId}
          onLoadMore={() => void loadMore()}
        />

        <aside aria-label={t('cockpit.rail.title')} className="space-y-6">
          <AgentRail sessions={sessions ?? null} error={railError?.message ?? null} />
          {canEdit ? <SimulateCard apps={apps} /> : null}
        </aside>
      </div>
    </div>
  )
}

function AppChip({
  label,
  active,
  onClick,
}: {
  label: string
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={`rounded px-2 py-0.5 text-xs font-medium ${
        active
          ? 'bg-primary text-primary-foreground'
          : 'bg-muted text-muted-foreground hover:bg-muted/70'
      }`}
    >
      {label}
    </button>
  )
}

interface FeedSectionProps {
  entries: CockpitFeedEvent[]
  cursor: boolean
  loading: boolean
  hasError: boolean
  hasData: boolean
  openId: string | null
  appending: boolean
  onToggle: (id: string | null) => void
  onLoadMore: () => void
}

function FeedSection({
  entries,
  cursor,
  loading,
  hasError,
  hasData,
  openId,
  appending,
  onToggle,
  onLoadMore,
}: FeedSectionProps) {
  const { t } = useI18n('e2ai')

  return (
    <section aria-label={t('cockpit.feed.title')} className="space-y-1">
      <h2 className="text-sm font-medium text-muted-foreground">{t('cockpit.feed.title')}</h2>

      {hasError && !hasData ? (
        <p className="text-sm text-destructive">{t('cockpit.feed.loadError')}</p>
      ) : null}
      {loading && !hasData && !hasError ? (
        <p className="text-sm text-muted-foreground">{t('runs.loading')}</p>
      ) : null}
      {!loading && !hasData && !hasError ? (
        <div className="rounded border bg-card p-4">
          <p className="text-sm font-medium">{t('cockpit.feed.empty')}</p>
          <p className="mt-1 text-sm text-muted-foreground">{t('cockpit.feed.emptyBody')}</p>
        </div>
      ) : null}

      <div aria-live="polite" className="sr-only">
        {t('cockpit.feed.announce', { count: entries.length })}
      </div>

      <ul className="divide-y">
        {entries.map((e) => (
          <FeedRow key={e.id} event={e} open={openId === e.id} onToggle={() => onToggle(openId === e.id ? null : e.id)} />
        ))}
      </ul>

      {cursor ? (
        <div className="flex justify-center pt-2">
          <button
            type="button"
            className="rounded-md border px-4 py-2 text-sm hover:bg-muted"
            disabled={appending}
            onClick={onLoadMore}
          >
            {appending ? t('runs.loading') : t('runs.loadMore')}
          </button>
        </div>
      ) : null}
    </section>
  )
}

function FeedRow({
  event,
  open,
  onToggle,
}: {
  event: CockpitFeedEvent
  open: boolean
  onToggle: () => void
}) {
  const { t } = useI18n('e2ai')
  const statusWord = event.kind === 'run' ? event.summary.split(' — ').pop() ?? '' : ''
  const statusIsRun = event.kind === 'run' && (statusWord in RUN_TONE)
  const inFlight = statusIsRun && RUN_TONE[statusWord as RunStatus] === 'live'

  return (
    <li>
      <button
        type="button"
        aria-expanded={open}
        onClick={onToggle}
        className="grid w-full grid-cols-[58px_1fr_150px_24px] items-center gap-3 px-1 py-2 text-left text-sm hover:bg-muted/40"
      >
        <span className="font-mono text-xs text-muted-foreground">
          {new Date(event.created_at).toLocaleTimeString([], { hour12: false })}
        </span>
        <span className="flex min-w-0 items-center gap-2">
          {statusIsRun ? (
            <StatusChip status={statusWord as RunStatus} />
          ) : (
            <KindChip kind={event.kind} />
          )}
          {inFlight ? (
            <span className="inline-flex items-center gap-1 rounded bg-blue-100 px-2 py-0.5 text-xs font-medium text-blue-800 dark:bg-blue-900 dark:text-blue-200">
              <span className="h-1.5 w-1.5 rounded-full bg-blue-600" />
              {t('cockpit.arm.live')}
            </span>
          ) : null}
          <span className="truncate">{event.summary}</span>
        </span>
        <span className="font-mono text-xs text-muted-foreground">{event.app_id.slice(0, 8)}</span>
        <span aria-hidden className="text-muted-foreground">{open ? '⌄' : '›'}</span>
      </button>

      {open ? (
        <div aria-label={t('cockpit.feed.detailLabel')} className="border-l-2 border-muted bg-muted/30 px-4 py-2 text-sm">
          <p className="font-mono text-xs text-muted-foreground">
            {formatTimestamp(event.created_at)}
          </p>
          {event.run_id ? (
            <Link to={`/e2ai/runs/${event.run_id}`} className="text-sm text-primary underline-offset-2 hover:underline">
              {t('cockpit.feed.runDetail')}
            </Link>
          ) : (
            <Link to="/e2ai/trends" className="text-sm text-primary underline-offset-2 hover:underline">
              {t('cockpit.feed.viewTrends')}
            </Link>
          )}
        </div>
      ) : null}
    </li>
  )
}

function KindChip({ kind }: { kind: CockpitFeedEvent['kind'] }) {
  const { t } = useI18n('e2ai')
  if (kind === 'alert') {
    return (
      <span className="inline-block rounded bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-900 dark:text-amber-200">
        {t('cockpit.feed.alertChip')}
      </span>
    )
  }
  return (
    <span className="inline-block rounded bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
      {t('cockpit.feed.sessionChip')}
    </span>
  )
}
