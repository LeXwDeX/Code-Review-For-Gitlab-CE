import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/react'
import { ReviewsPage } from '../src/pages/ReviewsPage'
import { installFetch, jsonResponse, renderWithProviders } from './test-utils'

function routes(reviewsResponse: () => Response | Promise<Response>) {
  return installFetch((url) => {
    if (url.startsWith('/api/v1/reviews')) return reviewsResponse()
    if (url.startsWith('/api/v1/projects')) return jsonResponse({ projects: [] })
    if (url.startsWith('/api/v1/profiles')) return jsonResponse({ profiles: [] })
    return jsonResponse({})
  })
}

describe('review list state handling', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the real API error and no demo rows when the reviews endpoint fails', async () => {
    routes(() => jsonResponse({ error: { code: 'internal', message: 'review store unavailable' } }, 500))
    renderWithProviders(<ReviewsPage />, { route: '/reviews' })
    const alert = await screen.findByRole('alert')
    expect(within(alert).getByText(/review store unavailable/)).toBeInTheDocument()
    expect(screen.queryByText('!318')).not.toBeInTheDocument()
  })

  it('shows an explicit empty state for an empty reviews collection', async () => {
    routes(() => jsonResponse({ reviews: [] }))
    renderWithProviders(<ReviewsPage />, { route: '/reviews' })
    expect(await screen.findByText('还没有评审记录')).toBeInTheDocument()
  })
})
