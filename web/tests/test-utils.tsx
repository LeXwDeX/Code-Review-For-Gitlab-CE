import type { ReactNode } from 'react'
import { vi } from 'vitest'
import { render } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { ToastProvider } from '../src/components/ui'
import { SessionProvider } from '../src/components/RoleContext'
import type { Me } from '../src/api/types'

export const adminMe: Me = { id: 1, username: 'alice', name: 'Alice', role: 'admin', csrf_token: 'csrf-test' }
export const viewerMe: Me = { id: 2, username: 'bob', name: 'Bob', role: 'viewer', csrf_token: 'csrf-viewer' }

export function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  })
}

export function renderWithProviders(
  ui: ReactNode,
  options: { me?: Me; route?: string; queryClient?: QueryClient } = {},
) {
  const queryClient = options.queryClient || createTestQueryClient()
  const result = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[options.route || '/']}>
        <SessionProvider me={options.me || adminMe}>
          <ToastProvider>{ui}</ToastProvider>
        </SessionProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
  return { ...result, queryClient }
}

export function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
}

export function installFetch(routes: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    return Promise.resolve(routes(url, init))
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

