import type { RouteObject } from 'react-router-dom'
import { RunsListPage } from '../features/runs/RunsListPage'
import { RunDetailPage } from '../features/runs/RunDetailPage'
import { RunReportPage } from '../features/runs/RunReportPage'
import { PlansListPage } from '../features/plans/PlansListPage'
import { PlanDetailPage } from '../features/plans/PlanDetailPage'
import { ApprovalsQueuePage } from '../features/plans/ApprovalsQueuePage'
import { CorpusPage } from '../features/corpus/CorpusPage'

// Route targets are relative to the module mount (routeBase `/e2ai` in
// admin.module.json). The shell mounts the module under
// `<Route path="/e2ai/*">`, so useRoutes matches against the remaining
// pathname — paths here MUST be mount-relative ("plans"), not absolute
// ("/e2ai/plans"), or no route ever matches. Detail/report/plan-detail are
// parameterized, not nav destinations.
export function routes(): RouteObject[] {
  return [
    {
      index: true,
      element: <RunsListPage />,
    },
    {
      path: 'runs/:id',
      element: <RunDetailPage />,
    },
    {
      path: 'runs/:id/report',
      element: <RunReportPage />,
    },
    {
      path: 'plans',
      element: <PlansListPage />,
    },
    {
      path: 'plans/:id',
      element: <PlanDetailPage />,
    },
    {
      path: 'approvals',
      element: <ApprovalsQueuePage />,
    },
    {
      path: 'corpus',
      element: <CorpusPage />,
    },
  ]
}
