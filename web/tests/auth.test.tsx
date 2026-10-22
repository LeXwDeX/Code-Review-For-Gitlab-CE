import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { App } from '../src/app'
import { setCsrfToken } from '../src/api/client'
import { createTestQueryClient, installFetch, jsonResponse } from './test-utils'

describe('authentication boundary', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    setCsrfToken(null)
  })

  it('shows a GitLab login action on 401 and never invents a user', async () => {
    installFetch(() => jsonResponse({ error: { code: 'unauthenticated', message: 'login required' } }, 401))
    render(
      <QueryClientProvider client={createTestQueryClient()}>
        <MemoryRouter initialEntries={['/']}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    const login = await screen.findByRole('link', { name: '通过 GitLab 登录' })
    expect(login).toHaveAttribute('href', '/auth/login')
    expect(screen.getAllByText('会话已过期，请重新登录。')).toHaveLength(1)
    expect(screen.queryByText('Alice')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '退出登录' })).not.toBeInTheDocument()
  })
})
