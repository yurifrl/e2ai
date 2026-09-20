/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** BFF mode switch (Tilt sets it; standalone dev defaults to mock auth). */
  readonly VITE_BFF_AUTH?: string
  /** BFF base URL, e.g. http://localhost:8080. */
  readonly VITE_BFF_BASE_URL?: string
  /** Explicit e2ai API base override (bypasses gateway composition). */
  readonly VITE_E2AI_API?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
