/**
 * Standalone dev entry (`bun run dev`): hosts the module inside a local
 * AdminShell with mock in-memory auth so the module can be developed without
 * the platform shell. Shell mode uses spa.tsx instead.
 */
import React from 'react'
import ReactDOM from 'react-dom/client'
import {
  AdminShell,
  createInMemoryAuthClient,
  createMockUsersFromRoles,
  initI18n,
  registerModuleTranslations,
} from '@nsxbet/admin-sdk'
import enUS from './i18n/locales/en-US.json'
import ptBR from './i18n/locales/pt-BR.json'
import es from './i18n/locales/es.json'
import ro from './i18n/locales/ro.json'
import manifest from '../admin.module.json'
// `App` imports "../index.css" so the stylesheet is reachable in both modes.
import { App } from './app/App'
import { AppProviders } from './app/AppProviders'

initI18n()
// Register module translations into the shell namespace (deep merge) so all
// t('e2ai.*') calls resolve correctly. Also register the e2ai sub-object into
// its own namespace so useI18n('e2ai') + t('key') works.
registerModuleTranslations('shell', { 'en-US': enUS, 'pt-BR': ptBR, es, ro })
registerModuleTranslations('e2ai', {
  'en-US': (enUS as Record<string, unknown>).e2ai as Record<string, unknown>,
  'pt-BR': (ptBR as Record<string, unknown>).e2ai as Record<string, unknown>,
  es: (es as Record<string, unknown>).e2ai as Record<string, unknown>,
  ro: (ro as Record<string, unknown>).e2ai as Record<string, unknown>,
})

const moduleManifest = manifest as import('@nsxbet/admin-sdk').AdminModuleManifest

// Standalone dev: the SDK AdminShell owns auth (mock user selector or BFF/Okta).
const useBffAuth = import.meta.env.VITE_BFF_AUTH === 'true'
const bffBaseUrl = import.meta.env.VITE_BFF_BASE_URL || 'http://localhost:8080'
const mockUsers = createMockUsersFromRoles({
  // Plan 05: dev identities need the edit rung too — every cockpit mutation
  // (trigger / pause / simulate) otherwise 403s in standalone dev.
  admin: ['admin.e2ai.view', 'admin.e2ai.edit'],
  editor: ['admin.e2ai.view', 'admin.e2ai.edit'],
  viewer: ['admin.e2ai.view'],
  noAccess: [],
})
const authClient = createInMemoryAuthClient({ users: mockUsers })

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    {useBffAuth ? (
      <AdminShell modules={[moduleManifest]} bff bffBaseUrl={bffBaseUrl} apiUrl="/api">
        <AppProviders>
          <App />
        </AppProviders>
      </AdminShell>
    ) : (
      <AdminShell modules={[moduleManifest]} authClient={authClient}>
        <AppProviders>
          <App />
        </AppProviders>
      </AdminShell>
    )}
  </React.StrictMode>,
)
