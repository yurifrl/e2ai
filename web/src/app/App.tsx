import { Fragment } from 'react'
import { useRoutes } from 'react-router-dom'
import { routes } from './routes'
import { ApprovalsBadge } from '../features/approvals/ApprovalsBadge'
import '../index.css'

export function App() {
  const routing = useRoutes(routes())
  return (
    <Fragment>
      <ApprovalsBadge />
      {routing}
    </Fragment>
  )
}
