import { useRoutes } from 'react-router-dom'
import manifest from '../../admin.module.json'
import { routes } from './routes'
import '../index.css'

// Canonical module base: every route target below is relative to the module
// mount (`/e2ai/*`, see admin.module.json routeBase).
const ROUTE_BASE = manifest.routeBase

export function App() {
  const routing = useRoutes(routes(ROUTE_BASE))
  return routing
}
