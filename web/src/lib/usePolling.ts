/**
 * Polling hook: re-runs `loader` on an interval while `enabled`. Stops on
 * document hidden (report-only surface; avoid hammering a busy API), resumes
 * on visible. Latest successful payload wins; errors surface via `error` and
 * leave the previous payload untouched so the UI degrades gracefully.
 */
import { useEffect, useState } from 'react'
import { ApiError } from '../lib/api'

export interface PollingState<T> {
  data: T | null
  error: Error | null
  loading: boolean
}

/**
 * `keepGoing` receives each successfully fetched payload; returning false
 * stops the loop (e.g. run reached a terminal status). Taking it as a
 * callback over the payload — instead of a plain boolean — lets callers stop
 * polling without referencing the hook's own data in its initializer.
 */
export function usePolling<T>(
  loader: () => Promise<T>,
  intervalMs: number,
  enabled = true,
  keepGoing?: (data: T) => boolean,
): PollingState<T> {
  const [state, setState] = useState<PollingState<T>>({
    data: null,
    error: null,
    loading: true,
  })

  useEffect(() => {
    if (!enabled) return

    let cancelled = false
    let timer: ReturnType<typeof setTimeout>

    const tick = async () => {
      try {
        const data = await loader()
        if (cancelled) return
        setState({ data, error: null, loading: false })
        if (keepGoing && !keepGoing(data)) return
      } catch (error) {
        if (cancelled) return
        // Polling continues on transient errors (API restart, artifact proxy
        // blip); a 404 means the run was deleted — stop polling that.
        const stop = error instanceof ApiError && error.status === 404
        setState((prev) => ({
          data: prev.data,
          error: error instanceof Error ? error : new Error(String(error)),
          loading: false,
        }))
        if (stop) return
      }
      if (cancelled || document.hidden) return
      timer = setTimeout(tick, intervalMs)
    }

    const onVisibility = () => {
      if (!document.hidden) {
        clearTimeout(timer)
        timer = setTimeout(tick, 250)
      }
    }
    document.addEventListener('visibilitychange', onVisibility)
    tick()

    return () => {
      cancelled = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, loader, intervalMs])

  return state
}
