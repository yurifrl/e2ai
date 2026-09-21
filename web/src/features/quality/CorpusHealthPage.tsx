/**
 * Corpus health (CORP-03): stale/flaky/verified rollups over the corpus plus
 * the promotion queue (unverified learnings, human-promote only). Read-only
 * rollups on top of the existing corpus endpoint; nothing mutates the corpus
 * here — promotion/dismissal stays on the Corpus page.
 */
import { useCallback } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '@nsxbet/admin-sdk'
import { getCorpus } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'
import { useAppId } from './useAppId'

const REFRESH_MS = 30_000

export function CorpusHealthPage() {
  const { t } = useI18n('e2ai')
  const appId = useAppId()

  const load = useCallback(() => getCorpus(appId ?? ''), [appId])
  const corpus = usePolling(load, REFRESH_MS, appId !== null)

  if (appId === null) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('corpusHealth.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('trends.detectingApp')}</p>
      </div>
    )
  }

  if (corpus.error) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('corpusHealth.title')}</h1>
        <p className="text-sm text-destructive">{t('corpus.loadError')}</p>
      </div>
    )
  }
  if (!corpus.data) {
    return (
      <div className="space-y-4">
        <h1 className="text-xl font-semibold">{t('corpusHealth.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('runs.loading')}</p>
      </div>
    )
  }

  const behaviors = corpus.data.behaviors
  const learnings = corpus.data.learnings
  const stale = behaviors.filter(behavior => behavior.status === 'stale')
  const verified = behaviors.filter(behavior => behavior.status === 'verified')
  const unverified = behaviors.filter(behavior => behavior.status === 'unverified')
  const promotionQueue = learnings.filter(learning => learning.status === 'unverified')
  const dismissed = learnings.filter(learning => learning.status === 'dismissed')

  const rollup = [
    { label: t('corpusHealth.verified'), value: verified.length, tone: 'text-emerald-600 dark:text-emerald-400' },
    { label: t('corpusHealth.unverified'), value: unverified.length, tone: undefined },
    { label: t('corpusHealth.stale'), value: stale.length, tone: 'text-amber-600 dark:text-amber-400' },
    { label: t('corpusHealth.promotionQueue'), value: promotionQueue.length, tone: undefined },
    { label: t('corpusHealth.dismissed'), value: dismissed.length, tone: undefined },
  ]

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">{t('corpusHealth.title')}</h1>
        <span className="font-mono text-xs text-muted-foreground">{appId}</span>
      </div>

      <section className="grid gap-3 sm:grid-cols-5">
        {rollup.map(item => (
          <div key={item.label} className="rounded-lg border p-3">
            <div className="text-xs text-muted-foreground">{item.label}</div>
            <div className={`text-lg font-semibold ${item.tone ?? ''}`}>{item.value}</div>
          </div>
        ))}
      </section>

      {stale.length > 0 ? (
        <section className="rounded-lg border">
          <h2 className="border-b px-4 py-3 text-sm font-medium">{t('corpusHealth.staleTitle')}</h2>
          <ul className="divide-y">
            {stale.map(behavior => (
              <li key={behavior.key} className="flex items-start gap-3 px-4 py-3 text-sm">
                <span className="mt-0.5 inline-block rounded bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-900 dark:text-amber-200">
                  {t('corpus.behavior.stale')}
                </span>
                <div>
                  <div className="font-medium">{behavior.title}</div>
                  <p className="text-muted-foreground">{behavior.description}</p>
                </div>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <section className="rounded-lg border">
        <header className="flex items-center justify-between border-b px-4 py-3">
          <h2 className="text-sm font-medium">{t('corpusHealth.queueTitle')}</h2>
          <Link to="/e2ai/corpus" className="text-xs text-primary hover:underline">
            {t('corpusHealth.openCorpus')}
          </Link>
        </header>
        {promotionQueue.length > 0 ? (
          <ul className="divide-y">
            {promotionQueue.map(learning => (
              <li key={learning.id} className="px-4 py-3 text-sm">
                <p>{learning.observation}</p>
                {learning.run_id ? (
                  <p className="font-mono text-xs text-muted-foreground">
                    {t('corpus.fromRun', { id: learning.run_id })}
                  </p>
                ) : null}
              </li>
            ))}
          </ul>
        ) : (
          <p className="px-4 py-6 text-center text-sm text-muted-foreground">
            {t('corpusHealth.noQueue')}
          </p>
        )}
      </section>
    </div>
  )
}
