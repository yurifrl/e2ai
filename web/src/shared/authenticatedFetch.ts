let sdkAuthenticatedFetch: typeof fetch | undefined

export function installAuthenticatedFetch(fetcher: typeof fetch): void {
  sdkAuthenticatedFetch = fetcher
}

export function resetAuthenticatedFetch(): void {
  sdkAuthenticatedFetch = undefined
}

export function authenticatedFetch(
  input: RequestInfo | URL,
  init?: RequestInit,
): Promise<Response> {
  if (!sdkAuthenticatedFetch) {
    return Promise.reject(new Error('SDK authenticated fetch is unavailable'))
  }

  return sdkAuthenticatedFetch(input, init)
}
