/**
 * Bridge between the SDK's authenticated fetch and module API calls.
 *
 * The AdminShell owns authentication: standalone mode uses the mock auth
 * client's fetch, shell/BFF mode uses the shell's `useFetch()`. Installing it
 * here lets the module's fetch layer add cookies/headers transparently, without
 * the module ever touching auth itself.
 */
import { useFetch } from '@nsxbet/admin-sdk'
import type { ReactNode } from 'react'
import { installAuthenticatedFetch } from './authenticatedFetch'

type AuthenticatedFetchProviderProps = {
  children: ReactNode
}

export function AuthenticatedFetchProvider({ children }: AuthenticatedFetchProviderProps) {
  installAuthenticatedFetch(useFetch())

  return children
}
