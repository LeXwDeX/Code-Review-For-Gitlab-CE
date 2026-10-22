import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render } from '@testing-library/react'
import { QueryClientProvider } from '@tanstack/react-query'
import { useReviewStream } from '../src/features/review/useReviewStream'
import { createTestQueryClient } from './test-utils'
import { review } from './fixtures'

class FakeEventSource {
  static instances: FakeEventSource[] = []
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 2
  url: string
  closed = false
  readyState = FakeEventSource.CONNECTING
  listeners = new Map<string, Set<EventListenerOrEventListenerObject>>()
  onerror: ((this: EventSource, ev: Event) => unknown) | null = null

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: EventListenerOrEventListenerObject | null) {
    if (!listener) return
    const set = this.listeners.get(type) || new Set()
    set.add(listener)
    this.listeners.set(type, set)
  }

  removeEventListener(type: string, listener: EventListenerOrEventListenerObject | null) {
    if (!listener) return
    this.listeners.get(type)?.delete(listener)
  }

  dispatchEvent(event: Event) {
    for (const listener of this.listeners.get(event.type) || []) {
      if (typeof listener === 'function') listener.call(this as unknown as EventSource, event)
      else listener.handleEvent(event)
    }
    return true
  }

  emit(type: string, data: string) {
    this.dispatchEvent(new MessageEvent(type, { data }))
  }

  close() {
    this.closed = true
    this.readyState = FakeEventSource.CLOSED
  }
}

function Harness({ id, status }: { id: string; status: string }) {
  const stream = useReviewStream(id, review({ id, status }))
  return <span data-testid="stream-state">{stream.state}</span>
}

describe('review SSE lifecycle', () => {
  beforeEach(() => {
    FakeEventSource.instances = []
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('closes the EventSource on a terminal workflow event and on unmount', async () => {
    const client = createTestQueryClient()
    const { unmount, getByTestId } = render(
      <QueryClientProvider client={client}><Harness id="review.1" status="running" /></QueryClientProvider>,
    )
    await vi.waitFor(() => expect(FakeEventSource.instances).toHaveLength(1))
    const source = FakeEventSource.instances[0]
    expect(source.url).toBe('/api/v1/reviews/review.1/stream')
    await act(async () => {
      source.emit('workflow', JSON.stringify({ status: 'completed', verdict: 'PASS' }))
    })
    await vi.waitFor(() => expect(source.closed).toBe(true))
    expect(getByTestId('stream-state')).toHaveTextContent('closed')
    unmount()
    expect(source.closed).toBe(true)
  })

  it('closes when the review id changes', async () => {
    const client = createTestQueryClient()
    const { rerender } = render(
      <QueryClientProvider client={client}><Harness id="first.review" status="running" /></QueryClientProvider>,
    )
    await vi.waitFor(() => expect(FakeEventSource.instances).toHaveLength(1))
    const first = FakeEventSource.instances[0]
    rerender(
      <QueryClientProvider client={client}><Harness id="second.review" status="running" /></QueryClientProvider>,
    )
    await vi.waitFor(() => expect(FakeEventSource.instances).toHaveLength(2))
    expect(first.closed).toBe(true)
    expect(FakeEventSource.instances[1].url).toBe('/api/v1/reviews/second.review/stream')
  })

  it('does not connect for an already-terminal review', () => {
    const client = createTestQueryClient()
    render(
      <QueryClientProvider client={client}><Harness id="done.review" status="completed" /></QueryClientProvider>,
    )
    expect(FakeEventSource.instances).toHaveLength(0)
  })
})
