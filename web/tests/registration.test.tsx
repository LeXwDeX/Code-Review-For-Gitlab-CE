import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ProjectsPage } from '../src/pages/ProjectsPage'
import { project } from './fixtures'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

describe('project registration', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows a one-time webhook secret and clears it when the result is closed', async () => {
    const user = userEvent.setup()
    let posted = false
    installFetch((url, init) => {
      if (url.startsWith('/api/v1/projects') && init?.method === 'POST') {
        posted = true
        return jsonResponse({ project: project(), job_id: 'job-1', webhook_secret: 'whsec_once_only' }, 201)
      }
      if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: posted ? [project()] : [] })
      if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [{ name: 'default' }] })
      return jsonResponse({})
    })
    renderWithProviders(<ProjectsPage />, { route: '/projects' })
    const registerButtons = await screen.findAllByRole('button', { name: '注册项目' })
    await user.click(registerButtons[0])
    await user.type(screen.getByLabelText('项目路径或 ID'), 'group/backend')
    await user.type(screen.getByLabelText('Project Access Token'), 'glpat-secret')
    await user.click(screen.getByLabelText(/自动创建 GitLab webhook/))
    await user.click(screen.getByRole('button', { name: '校验并注册' }))
    const dialog = await screen.findByRole('dialog', { name: '项目已注册' })
    expect(dialog).toHaveTextContent('job-1')
    const secret = screen.getByLabelText('Webhook secret')
    expect(secret).toHaveValue('whsec_once_only')
    expect(secret).toHaveAttribute('readonly')
    await user.click(screen.getByRole('button', { name: '完成' }))
    await waitFor(() => expect(screen.queryByDisplayValue('whsec_once_only')).not.toBeInTheDocument())
  })
})
