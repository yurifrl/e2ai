/** Status badge chips for plans, approvals and the corpus taxonomy. */
import { useI18n } from '@nsxbet/admin-sdk'
import type { BehaviorStatus, LearningStatus, PlanStatus } from '../../lib/types'

const PLAN_TONE: Record<PlanStatus, string> = {
  draft: 'bg-muted text-muted-foreground',
  pending_approval: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200',
  approved: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200',
  superseded: 'bg-violet-100 text-violet-800 dark:bg-violet-900 dark:text-violet-200',
  rejected: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
}

const BEHAVIOR_TONE: Record<BehaviorStatus, string> = {
  unverified: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200',
  verified: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200',
  stale: 'bg-muted text-muted-foreground',
}

const LEARNING_TONE: Record<LearningStatus, string> = {
  unverified: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200',
  promoted: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200',
  dismissed: 'bg-muted text-muted-foreground',
}

const DECISION_TONE: Record<'approved' | 'rejected', string> = {
  approved: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200',
  rejected: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200',
}

export function PlanStatusChip({ status }: { status: PlanStatus }) {
  const { t } = useI18n('e2ai')
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${PLAN_TONE[status]}`}>
      {t(`plans.status.${status}`)}
    </span>
  )
}

export function BehaviorChip({ status }: { status: BehaviorStatus }) {
  const { t } = useI18n('e2ai')
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${BEHAVIOR_TONE[status]}`}>
      {t(`corpus.behavior.${status}`)}
    </span>
  )
}

export function LearningChip({ status }: { status: LearningStatus }) {
  const { t } = useI18n('e2ai')
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${LEARNING_TONE[status]}`}>
      {t(`corpus.learning.${status}`)}
    </span>
  )
}

export function DecisionChip({ decision }: { decision: 'approved' | 'rejected' }) {
  const { t } = useI18n('e2ai')
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${DECISION_TONE[decision]}`}>
      {t(`approvals.decision.${decision}`)}
    </span>
  )
}
