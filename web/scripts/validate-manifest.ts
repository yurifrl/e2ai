/**
 * Manifest validation gate (acceptance: manifest passes admin.module.schema.json).
 * Fetches the canonical schema from the admin repo and validates
 * web/admin.module.json with ajv-draft-07. Run: bun run validate:manifest
 */
import Ajv from 'ajv'
import manifest from '../admin.module.json'

const SCHEMA_URL =
  'https://raw.githubusercontent.com/NSXBet/admin/main/packages/admin-sdk/schemas/admin.module.schema.json'

// The canonical remote is authoritative but not always reachable (private
// repo / CDN gaps), so the admin-sdk schema is vendored at web/schemas/ as a
// pinned fallback.
const response = await fetch(SCHEMA_URL)
let schema: unknown
if (response.ok) {
  schema = await response.json()
} else {
  console.warn(`remote schema unavailable (${response.status}); using vendored copy`)
  schema = await Bun.file(new URL('../schemas/admin.module.schema.json', import.meta.url)).json()
}

const ajv = new Ajv({ allErrors: true, strict: false })
const validate = ajv.compile(schema)
if (!validate(manifest)) {
  console.error('manifest INVALID:')
  for (const err of validate.errors ?? []) {
    console.error(`  ${err.instancePath || '/'} ${err.message}`)
  }
  process.exit(1)
}
console.log(`manifest OK: ${manifest.id} (routeBase ${manifest.routeBase})`)
