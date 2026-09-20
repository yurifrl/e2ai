import { registerModuleTranslations } from '@nsxbet/admin-sdk'
import enUS from './i18n/locales/en-US.json'
import ptBR from './i18n/locales/pt-BR.json'
import es from './i18n/locales/es.json'
import ro from './i18n/locales/ro.json'
import { App } from './app/App'
import { AppProviders } from './app/AppProviders'

// Register module translations into the shell namespace (deep merge) so all
// t('e2ai.*') calls resolve, plus the e2ai sub-object into its own namespace
// so useI18n('e2ai') + t('key') works regardless of shell/standalone mode.
registerModuleTranslations('shell', { 'en-US': enUS, 'pt-BR': ptBR, es, ro })
registerModuleTranslations('e2ai', {
  'en-US': (enUS as Record<string, unknown>).e2ai as Record<string, unknown>,
  'pt-BR': (ptBR as Record<string, unknown>).e2ai as Record<string, unknown>,
  es: (es as Record<string, unknown>).e2ai as Record<string, unknown>,
  ro: (ro as Record<string, unknown>).e2ai as Record<string, unknown>,
})

// The host shell provides authentication and router context.
export default function E2aiAdminModule() {
  return (
    <AppProviders>
      <App />
    </AppProviders>
  )
}
