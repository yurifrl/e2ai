/**
 * Sidebar badge for the approvals queue: polls the queue and reflects the
 * pending-approval count on the approvals sidebar command. Renders nothing.
 * Badge updates are keyed by the command's route (admin.module.json).
 */
import { useEffect } from 'react'
import { useI18n } from '@nsxbet/admin-sdk'
import { getApprovalsQueue } from '../../lib/api'
import { usePolling } from '../../lib/usePolling'

/** Must match the approvals command route in admin.module.json. */
const BADGE_ROUTE = '/e2ai/approvals'

export function ApprovalsBadge() {
  const { t } = useI18n('e2ai')
  const queue = usePolling(getApprovalsQueue, 30_000)
  const pending = queue.data?.pending.length ?? null

  useEffect(() => {
    const api = window.__ADMIN_PLATFORM_API__
    if (!api) return
    if (pending === null) return
    api.nav.setBadge(
      BADGE_ROUTE,
      pending > 0
        ? { variant: 'warning', count: pending, label: t('approvals.badge') }
        : null,
    )
    return () => {
      api.nav.setBadge(BADGE_ROUTE, null)
    }
  }, [pending, t])

  return null
}
