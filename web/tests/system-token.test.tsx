import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SystemPage } from '../src/pages/SystemPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('one-time API token display', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('renders the secret only once and clears it when the dialog closes', async () => {
    const user = userEvent.setup()
    let created = false
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/api-tokens') && init?.method === 'POST') {
        created = true
        return jsonResponse({ id: 't1', token: 'crsec_once_only' })
      }
      if (url.startsWith('/api/v1/api-tokens')) return jsonResponse({ tokens: created ? [{ id: 't1', name: 'ci' }] : [] })
      return jsonResponse({})
    })
    renderWithProviders(<SystemPage />)
    await user.click(await screen.findByRole('button', { name: '创建 token' }))
    await user.type(screen.getByLabelText('名称'), 'ci')
    const createButtons = screen.getAllByRole('button', { name: '创建', exact: true })
    await user.click(createButtons[createButtons.length - 1])
    const secretInput = await screen.findByDisplayValue('crsec_once_only')
    expect(secretInput).toHaveAttribute('readonly')
    await user.click(screen.getByRole('button', { name: '我已复制，关闭' }))
    await waitFor(() => expect(screen.queryByDisplayValue('crsec_once_only')).not.toBeInTheDocument())
  })
})
