import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders, viewerMe } from './test-utils'

describe('viewer write gating', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('does not show create or revoke controls to a viewer', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />, { me: viewerMe })
    await waitFor(() => expect(screen.getByText('还没有 API token')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '创建 token' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '撤销' })).not.toBeInTheDocument()
    expect(screen.getByText(/只读账号可查看 token 列表/)).toBeInTheDocument()
  })

  it('shows write controls to an admin with a CSRF-bearing session', async () => {
    installFetch((url) => {
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />)
    await waitFor(() => expect(screen.getByRole('button', { name: '创建 token' })).toBeInTheDocument())
  })
})
