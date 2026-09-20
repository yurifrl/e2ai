/**
 * Admin-gateway URL resolution for module-mode API calls.
 *
 * The shell serves this module under routeBase `/e2ai`, but its origin only
 * proxies `/api/` (admin-api) and `/bff/` (the admin BFF). The e2ai API is
 * reached through the BFF, which routes the `/e2ai` prefix to e2ai-api and
 * strips it. So module API calls must go through the BFF as
 * `${bffBaseUrl}/e2ai/api/*` — mirroring what the built module code targets in
 * production (`/e2ai/api/*` with the shell's BFF in front).
 *
 * In standalone dev (`vite --port 3005`) the vite dev-server proxy handles the
 * same `/e2ai/*` → `:8082` strip, so the composed base below resolves to a
 * same-origin URL either way.
 */
import { env } from '@nsxbet/admin-sdk'

const DEV_FALLBACK = 'http://localhost:8080'

export function e2aiApiBase(): string {
  const explicit = import.meta.env.VITE_E2AI_API
  if (explicit) return explicit.endsWith('/') ? explicit.slice(0, -1) : explicit
  if (env.adminGatewayUrl) {
    const url = env.adminGatewayUrl
    return url.endsWith('/') ? url.slice(0, -1) : url
  }
  if (import.meta.env.DEV) return DEV_FALLBACK
  return ''
}
