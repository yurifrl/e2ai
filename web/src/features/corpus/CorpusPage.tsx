/**
 * Corpus browser: verified behaviors and captured learnings for one app.
 * The app selector is a plain text field (no apps endpoint exists); it
 * defaults to bet-app — the app e2ai seeds from — and both halves refresh
 * every 30s while visible.
 */
import { useMemo, useState } from 'react'
import { useI18n } from '@nsxbet/admin-sdk'
import { getCorpus, promoteLearning, dismissLearning } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'
import { BehaviorChip, LearningChip } from '../plans/chips'

type LearningAction = 'promote' | 'dismiss'

const DEFAULT_APP = 'bet-app'

export function CorpusPage() {
  const { t } = useI18n('e2ai')
  const [app, setApp] = useState(DEFAULT_APP)
  const [acting, setActing] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const corpus = usePolling(() => getCorpus(app), 30_000)

  const behaviors = corpus.data?.behaviors ?? []
  const learnings = corpus.data?.learnings ?? []
  const verified = useMemo(
    () => behaviors.filter((b) => b.status === 'verified').length,
    [behaviors],
  )

  const act = async (learningId: string, action: LearningAction) => {
    setActing(learningId)
    setActionError(null)
    try {
      if (action === 'promote') await promoteLearning(learningId)
      else await dismissLearning(learningId)
    } catch (error) {
      setActionError(error instanceof Error ? error.message : String(error))
    } finally {
      setActing(null)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">{t('corpus.title')}</h1>
        <div className="flex items-center gap-2">
          <label htmlFor="corpus-app" className="text-sm text-muted-foreground">
            {t('corpus.app')}
          </label>
          <input
            id="corpus-app"
            value={app}
            onChange={(event) => setApp(event.target.value.trim() || DEFAULT_APP)}
            className="w-44 rounded-md border px-2 py-1 text-sm"
          />
        </div>
      </div>

      {corpus.error ? (
        <span className="text-sm text-destructive">{t('corpus.loadError')}</span>
      ) : null}
      {actionError ? (
        <span className="text-sm text-destructive">{actionError}</span>
      ) : null}

      <section>
        <h2 className="mb-2 font-medium">
          {t('corpus.behaviors')}{' '}
          <span className="text-sm font-normal text-muted-foreground">
            {verified}/{behaviors.length}
          </span>
        </h2>
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="px-3 py-2 font-medium">{t('corpus.col.key')}</th>
              <th className="px-3 py-2 font-medium">{t('corpus.col.behavior')}</th>
              <th className="px-3 py-2 font-medium">{t('corpus.col.status')}</th>
              <th className="px-3 py-2 font-medium">{t('corpus.col.provenance')}</th>
            </tr>
          </thead>
          <tbody>
            {behaviors.map((behavior) => (
              <tr key={behavior.key} className="border-b">
                <td className="px-3 py-2 font-mono text-xs">{behavior.key}</td>
                <td className="px-3 py-2">
                  <div className="font-medium">{behavior.title}</div>
                  <div className="text-muted-foreground">{behavior.description}</div>
                </td>
                <td className="px-3 py-2">
                  <BehaviorChip status={behavior.status} />
                  {behavior.verified_by ? (
                    <div className="mt-1 text-xs text-muted-foreground">
                      {t('corpus.verifiedBy', { by: behavior.verified_by })}
                    </div>
                  ) : null}
                </td>
                <td className="px-3 py-2 text-xs text-muted-foreground">
                  {behavior.provenance.kind}
                  {behavior.provenance.path ? ` · ${behavior.provenance.path}` : ''}
                  {behavior.provenance.commit ? ` · ${behavior.provenance.commit.slice(0, 8)}` : ''}
                </td>
              </tr>
            ))}
            {behaviors.length === 0 && !corpus.loading ? (
              <tr>
                <td colSpan={4} className="px-3 py-6 text-center text-muted-foreground">
                  {t('corpus.noBehaviors')}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </section>

      <section>
        <h2 className="mb-2 font-medium">{t('corpus.learnings')}</h2>
        <ul className="space-y-2">
          {learnings.map((learning) => (
            <li key={learning.id} className="flex items-start justify-between gap-4 border-b pb-2">
              <div className="text-sm">
                <div>{learning.observation}</div>
                <div className="text-xs text-muted-foreground">
                  {learning.run_id ? t('corpus.fromRun', { id: learning.run_id.slice(0, 8) }) : t('corpus.manual')}
                </div>
              </div>
              <div className="flex items-center gap-2">
                <LearningChip status={learning.status} />
                {learning.status === 'unverified' ? (
                  <>
                    <button
                      type="button"
                      className="rounded-md border px-2 py-1 text-xs hover:bg-muted disabled:opacity-50"
                      disabled={acting !== null}
                      onClick={() => void act(learning.id, 'promote')}
                    >
                      {t('corpus.promote')}
                    </button>
                    <button
                      type="button"
                      className="rounded-md border px-2 py-1 text-xs hover:bg-muted disabled:opacity-50"
                      disabled={acting !== null}
                      onClick={() => void act(learning.id, 'dismiss')}
                    >
                      {t('corpus.dismiss')}
                    </button>
                  </>
                ) : null}
              </div>
            </li>
          ))}
          {learnings.length === 0 && !corpus.loading ? (
            <li className="py-6 text-center text-sm text-muted-foreground">
              {t('corpus.noLearnings')}
            </li>
          ) : null}
        </ul>
      </section>
    </div>
  )
}
