import { describe, expect, it } from 'vitest'
import { COLUMN_GAP, NODE_WIDTH, PADDING, layoutDag } from '../src/lib/dag'

describe('layoutDag', () => {
  it('places a dependency chain in successive layers', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed' },
        { id: 'b', kind: 'x', status: 'running', depends_on: ['a'] },
        { id: 'c', kind: 'x', status: 'pending' },
      ],
      [
        { from: 'a', to: 'b' },
        { from: 'b', to: 'c' },
      ],
    )
    const byId = new Map(layout.nodes.map((node) => [node.id, node]))
    expect(byId.get('a')?.layer).toBe(0)
    expect(byId.get('b')?.layer).toBe(1)
    expect(byId.get('c')?.layer).toBe(2)
    expect(byId.get('b')?.x).toBe(PADDING + NODE_WIDTH + COLUMN_GAP)
    expect(layout.edges).toHaveLength(2)
    expect(layout.edges[0].path).toContain('M ')
  })

  it('puts disconnected components in the first layer and does not crash', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed' },
        { id: 'alone', kind: 'x', status: 'completed' },
      ],
      [],
    )
    expect(layout.nodes.find((node) => node.id === 'alone')?.layer).toBe(0)
    expect(layout.layerCount).toBe(1)
    expect(layout.cycleNodes).toEqual([])
  })

  it('ignores unknown dependencies and reports them', () => {
    const layout = layoutDag([{ id: 'a', kind: 'x', status: 'completed', depends_on: ['missing'] }], [])
    expect(layout.nodes[0].layer).toBe(0)
    expect(layout.unknownEdges).toBe(1)
  })

  it('terminates on cycles and keeps every node', () => {
    const layout = layoutDag(
      [
        { id: 'a', kind: 'x', status: 'completed', depends_on: ['c'] },
        { id: 'b', kind: 'x', status: 'completed', depends_on: ['a'] },
        { id: 'c', kind: 'x', status: 'completed', depends_on: ['b'] },
        { id: 'root', kind: 'x', status: 'completed' },
      ],
      [],
    )
    expect(layout.nodes).toHaveLength(4)
    expect(layout.cycleNodes.sort()).toEqual(['a', 'b', 'c'])
    for (const node of layout.nodes) {
      if (layout.cycleNodes.includes(node.id)) expect(node.layer).toBe(1)
    }
  })

  it('preserves node ids containing dots and de-duplicates edges', () => {
    const layout = layoutDag(
      [
        { id: 'review.std.p1', kind: 'x', status: 'completed' },
        { id: 'verify.f7', kind: 'x', status: 'completed', depends_on: ['review.std.p1'] },
      ],
      [{ from: 'review.std.p1', to: 'verify.f7' }, { from: 'review.std.p1', to: 'verify.f7' }],
    )
    expect(layout.nodes.map((node) => node.id)).toEqual(['review.std.p1', 'verify.f7'])
    expect(layout.edges).toHaveLength(1)
    expect(layout.nodes.find((node) => node.id === 'verify.f7')?.layer).toBe(1)
  })

  it('handles an empty graph', () => {
    const layout = layoutDag([], [])
    expect(layout.nodes).toEqual([])
    expect(layout.edges).toEqual([])
    expect(layout.cycleNodes).toEqual([])
  })
})
