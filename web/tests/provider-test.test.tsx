import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ModelsPage } from '../src/pages/ModelsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('provider connectivity test', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('does not treat a response without ok:boolean as success', async () => {
    const user = userEvent.setup()
    installFetch((url) => {
      if (url === '/api/v1/providers/claude/test') return jsonResponse({ message: '模型可用' })
      if (url.startsWith('/api/v1/providers')) {
        return jsonResponse({ providers: [{ name: 'claude', protocol: 'anthropic', base_url: 'https://api.anthropic.com', model: 'test', stats_24h: { calls: 1, errors: 0, error_rate: 0, p50_latency_ms: 100 } }] })
      }
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
      return jsonResponse({})
    })
    renderWithProviders(<ModelsPage />, { route: '/models' })
    await user.click(await screen.findByRole('button', { name: '测试连通性' }))
    expect(await screen.findByText(/缺少 ok 布尔字段/)).toBeInTheDocument()
    expect(screen.queryByText(/^测试成功/)).not.toBeInTheDocument()
  })
})
