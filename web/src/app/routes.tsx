import type { RouteObject } from 'react-router-dom'
import { RunsListPage } from '../features/runs/RunsListPage'
import { RunDetailPage } from '../features/runs/RunDetailPage'
import { RunReportPage } from '../features/runs/RunReportPage'

// Route targets are relative to the module mount (routeBase `/e2ai`, passed
// in from App.tsx). Detail/report are parameterized, not nav destinations.
export function routes(base: string): RouteObject[] {
  return [
    {
      path: base,
      element: <RunsListPage />,
    },
    {
      path: `${base}/runs/:id`,
      element: <RunDetailPage />,
    },
    {
      path: `${base}/runs/:id/report`,
      element: <RunReportPage />,
    },
  ]
}
