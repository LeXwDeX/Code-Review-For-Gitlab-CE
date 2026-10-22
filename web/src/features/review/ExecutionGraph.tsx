import { useMemo } from 'react'
import type { GraphEdge, GraphNode } from '../../api/types'
import { layoutDag, NODE_HEIGHT, NODE_WIDTH } from '../../lib/dag'
import { labelForStatus, statusTone } from '../../lib/format'
import { Badge, EmptyState } from '../../components/ui'
import styles from '../../pages/pages.module.css'

export function nodeStatusTone(status: string) {
  return statusTone(status)
}

export function ExecutionGraph({
  nodes, edges, selectedId, onSelect,
}: {
  nodes: GraphNode[]
  edges: GraphEdge[]
  selectedId?: string
  onSelect: (node: GraphNode) => void
}) {
  const layout = useMemo(() => layoutDag(nodes, edges), [nodes, edges])

  if (!nodes.length) {
    return <EmptyState title="执行图尚未生成" description="评审开始执行后，节点会按依赖关系显示在这里。" />
  }


  return (
    <div className={styles.graphViewport}>
      <div
        style={{
          position: 'relative',
          width: layout.width,
          height: layout.height,
          minWidth: '100%',
        }}
      >
        <svg
          width={layout.width}
          height={layout.height}
          style={{ position: 'absolute', inset: 0, overflow: 'visible', pointerEvents: 'none' }}
          aria-hidden="true"
        >
          {layout.edges.map((edge) => (
            <path
              key={edge.key}
              d={edge.path}
              fill="none"
              stroke="var(--border-strong)"
              strokeWidth={1.5}
              opacity={0.7}
            />
          ))}
        </svg>
        {layout.nodes.map((node) => {
          const selected = node.id === selectedId
          const status = labelForStatus(node.status)
          return (
            <button
              key={node.id}
              type="button"
              className={[
                styles.nodeCard,
                selected ? styles.nodeCardSelected : '',
                node.status === 'failed' ? styles.nodeFail : '',
              ].filter(Boolean).join(' ')}
              style={{ left: node.x, top: node.y, width: NODE_WIDTH, height: NODE_HEIGHT }}
              onClick={() => onSelect(node)}
              aria-pressed={selected}
              aria-label={`节点 ${node.id}，类型 ${node.kind}，状态 ${status}`}
              title={node.id}
            >
              <span className={styles.nodeId}>{node.id}</span>
              <span className={styles.nodeKind}>{node.kind}</span>
              <span className={styles.nodeFooter}>
                <Badge tone={nodeStatusTone(node.status)}>{status}</Badge>
                <span className={styles.nodeAttempt}>
                  {node.provider || '—'}{typeof node.attempt === 'number' ? ` · #${node.attempt}` : ''}
                </span>
              </span>
            </button>
          )
        })}
        {layout.nodes.map((node) => {
          // Layer labels are rendered above nodes only when they stay inside the canvas.
          const layerX = node.x
          const layerNodes = layout.nodes.filter((item) => item.layer === node.layer)
          if (layerNodes[0]?.id !== node.id) return null
          const label = `第 ${node.layer + 1} 层`
          return (
            <span
              key={`layer-${node.id}`}
              aria-hidden="true"
              style={{
                position: 'absolute', left: layerX, top: 4, fontSize: 11, color: 'var(--muted)',
              }}
            >
              {label}
            </span>
          )
        })}
      </div>
      <p className={styles.graphHint}>
        点击节点查看参数、输出和模型调用。共 {layout.nodes.length} 个节点，
        {layout.edges.length} 条连接；第 1 层为无依赖节点。
        {layout.cycleNodes.length ? ` 检测到 ${layout.cycleNodes.length} 个循环依赖节点，已放置在末层。` : ''}
        {layout.unknownEdges ? ` 忽略了 ${layout.unknownEdges} 条指向未知节点的连接。` : ''}
      </p>
    </div>
  )
}

