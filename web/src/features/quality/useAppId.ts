/**
 * App-id resolution for the phase 5 analytics surfaces (flakes, trends): the
 * `?app=<uuid>` query param wins; without one the app id of the newest run is
 * used (the app with recent activity is the one being operated on). Once
 * resolved the value is written back into the URL so links and refreshes keep
 * the scope — same pattern CorpusPage uses for its app field, minus the apps
 * endpoint that does not exist.
 */
import { useEffect, useRef } from 'react'
import { useSearchParams } from 'react-router-dom'
import { listRuns } from '../../lib/api'
import type { RunsPage } from '../../lib/types'

export function useAppId(): string | null {
  const [params, setParams] = useSearchParams()
  const appId = params.get('app')
  // Guard against a failed detection retry-looping: one attempt per app-less URL.
  const tried = useRef(false)

  useEffect(() => {
    if (appId || tried.current) return
    tried.current = true
    listRuns(1)
      .then((page: RunsPage) => {
        const id = page.runs[0]?.app_id
        if (id) {
          const next = new URLSearchParams(params)
          next.set('app', id)
          setParams(next, { replace: true })
        }
      })
      .catch(() => undefined)
  }, [appId, params, setParams])

  return appId
}
