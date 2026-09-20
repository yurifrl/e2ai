import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
import { adminModule } from '@nsxbet/admin-sdk/vite'

// Build/externals config comes from the SDK plugin (shared deps are provided
// by the shell import map); only module-specific options live here.
export default defineConfig(() => {
  // Tilt (task run) sets VITE_BFF_AUTH / VITE_BFF_BASE_URL in the process
  // env. Vite only reads .env files for import.meta.env, so forward them
  // explicitly.
  const define: Record<string, string> = {}
  for (const key of ['VITE_BFF_AUTH', 'VITE_BFF_BASE_URL', 'VITE_E2AI_API'])
    if (process.env[key] !== undefined)
      define[`import.meta.env.${key}`] = JSON.stringify(process.env[key])

  return {
    define,
    plugins: [react(), ...adminModule()],
    resolve: {
      alias: {
        '@': path.resolve(__dirname, './src'),
      },
    },
    server: {
      host: '::',
      port: 3005,
      allowedHosts: true,
      proxy: {
        // Module is served under routeBase /e2ai (admin.module.json) and code
        // built for the BFF calls /e2ai/api/* and /e2ai/artifacts/* — the
        // shell's BFF prefix. Strip the /e2ai prefix and forward to the Go API
        // so those calls work in standalone dev too. The BFF does the same
        // strip in shell mode (contract locked with the API owner).
        '/e2ai/artifacts': {
          target: process.env.E2AI_API_PROXY ?? 'http://localhost:8082',
          changeOrigin: true,
          configure: (proxy) => {
            // http-proxy 1.x forwards the upstream chunked transfer-encoding
            // while Node's own server also chunk-encodes → duplicate TE
            // header on the wire; delete it (same fix as incidents-admin).
            proxy.on('proxyRes', (proxyRes) => {
              delete proxyRes.headers['transfer-encoding']
            })
          },
          rewrite: (path: string) => path.replace(/^\/e2ai/, ''),
        },
        '/e2ai/api': {
          target: process.env.E2AI_API_PROXY ?? 'http://localhost:8082',
          changeOrigin: true,
          configure: (proxy) => {
            proxy.on('proxyRes', (proxyRes) => {
              delete proxyRes.headers['transfer-encoding']
            })
          },
          rewrite: (path: string) => path.replace(/^\/e2ai/, ''),
          // Standalone mock auth has no real BFF session. Forward the same dev
          // identity headers the API's DEBUG fallback expects.
          headers: {
            'X-User': 'admin@example.com',
            'X-Permissions': 'admin.e2ai.view',
          },
        },
      },
    },
  }
})
