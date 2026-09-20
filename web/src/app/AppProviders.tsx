import type { ReactNode } from 'react'
import { useFetch } from '@nsxbet/admin-sdk'
import { installAuthenticatedFetch } from '../shared/authenticatedFetch'

// Module-owned providers only. Authentication and routing belong to the
// surrounding AdminShell (host shell in production, standalone shell in dev).
export function AppProviders({ children }: { children: ReactNode }) {
  installAuthenticatedFetch(useFetch())

  return children
}
