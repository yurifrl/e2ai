/** Status/verdict badge chips with the fixed pipeline taxonomy colors. */
import { useI18n } from '@nsxbet/admin-sdk'
import type { RunStatus, Verdict } from '../../lib/types'

const STATUS_TONE: Record<RunStatus, string> = {
  QUEUED: 'bg-muted text-muted-foreground',
  WAITING_ROLLOUT: 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200',
  PROVISIONING: 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200',
  EXECUTING: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200',
  REPORTING: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200',
  COMPLETED: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200',
  FAILED: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
}

const VERDICT_TONE: Record<Verdict, string> = {
  PASS: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200',
  FAIL: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
  FLAKY: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200',
  INFRA_ERROR: 'bg-violet-100 text-violet-800 dark:bg-violet-900 dark:text-violet-200',
  INCONCLUSIVE: 'bg-muted text-muted-foreground',
}

export function StatusChip({ status }: { status: RunStatus }) {
  const { t } = useI18n('e2ai')
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${STATUS_TONE[status]}`}>
      {t(`status.${status}`)}
    </span>
  )
}

export function VerdictChip({ verdict }: { verdict: Verdict }) {
  const { t } = useI18n('e2ai')
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${VERDICT_TONE[verdict]}`}>
      {t(`verdict.${verdict}`)}
    </span>
  )
}
