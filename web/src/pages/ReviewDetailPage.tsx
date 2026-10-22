import { useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  cancelReview, getProject, getReview, getReviewGraph, listFindings, listProjects, republishReview,
} from '../api/api'
import type { Finding, ReviewDetail, ReviewSummary } from '../api/types'
import { useCanWrite } from '../components/RoleContext'
import { TriggerReviewDialog } from '../components/TriggerReviewDialog'
import {
  Badge, Button, ConfirmDialog, EmptyState, ErrorState, LoadingState, Panel, RelativeTime, SeverityBadge, StatusBadge, VerdictBadge, useToast,
} from '../components/ui'
import { ExecutionGraph } from '../features/review/ExecutionGraph'
import { NodeDrawer } from '../features/review/NodeDrawer'
import { useReviewStream } from '../features/review/useReviewStream'
import { usePaginated } from '../lib/hooks'
import {
  decisionTone, formatCost, formatDuration, formatJson, formatTokens, labelForChallenge, labelForDecision,
  labelForTrigger, safeExternalUrl, shortSha, stringifyUnknown,
} from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'
import type { GraphNode } from '../api/types'

const decisionOptions = [
  ['', '全部处置'],
  ['publish', '发布'],
  ['mark', '存疑'],
  ['drop', '丢弃'],
  ['downgrade', '降级'],
]
const severityOptions = [['', '全部级别'], ['P0', 'P0'], ['P1', 'P1'], ['P2', 'P2'], ['info', 'info']]

export function FindingRow({ finding, index, expanded, onToggle }: { finding: Finding; index: number; expanded: boolean; onToggle: () => void }) {
  const bodyId = `finding-body-${index}`
  return (
    <>
      <tr>
        <td><SeverityBadge severity={finding.final_severity || finding.severity} /></td>
        <td>
          <div className={styles.tablePrimary}>
            <button
              type="button"
              className={styles.expandButton}
              aria-expanded={expanded}
              aria-controls={bodyId}
              onClick={onToggle}
              style={{ alignSelf: 'flex-start', paddingLeft: 0 }}
            >
              {expanded ? '收起' : '展开'}：{finding.title}
            </button>
            <span className={styles.tableMeta}>{finding.category || '未分类'}</span>
          </div>
        </td>
        <td><span className="mono small">{finding.path ? `${finding.path}${finding.line ? `:${finding.line}` : ''}` : '—'}</span></td>
        <td>
          <div className={styles.tablePrimary}>
            <span className="mono small">{finding.origin_node}</span>
            <span className={styles.tableMeta}>{finding.origin_provider || '—'}</span>
          </div>
        </td>
        <td><Badge tone={challengeTone(finding.challenge_status)}>{labelForChallenge(finding.challenge_status)}</Badge></td>
        <td><Badge tone={decisionTone(finding.decision)}>{labelForDecision(finding.decision)}</Badge></td>
        <td><span className={styles.tableMeta}>{finding.decision_reason || '—'}</span></td>
      </tr>
      {expanded ? (
        <tr>
          <td colSpan={7} className={styles.bodyCell} id={bodyId}>
            <div className={styles.findingBody}>
              <section>
                <h4>问题正文</h4>
                {finding.body ? <p>{finding.body}</p> : <span className="muted small">没有正文记录。</span>}
              </section>
              <section>
                <h4>证据</h4>
                {finding.evidence ? <pre className="pre" style={evidenceStyle}>{stringifyUnknown(finding.evidence)}</pre> : <span className="muted small">没有证据记录。</span>}
              </section>
              {finding.challenge_evidence ? (
                <section>
                  <h4>挑战证据</h4>
                  <pre className="pre" style={evidenceStyle}>{stringifyUnknown(finding.challenge_evidence)}</pre>
                </section>
              ) : null}
              <section>
                <h4>其他字段</h4>
                <dl className={styles.docMeta} style={{ display: 'grid', gap: 4 }}>
                  <div><dt>confidence</dt><dd>{finding.confidence || '—'}</dd></div>
                  <div><dt>votes</dt><dd>{typeof finding.votes === 'number' ? finding.votes : '—'}</dd></div>
                  <div><dt>position_ok</dt><dd>{typeof finding.position_ok === 'boolean' ? String(finding.position_ok) : '—'}</dd></div>
                  <div><dt>challenge_node</dt><dd>{finding.challenge_node || '—'}</dd></div>
                </dl>
              </section>
            </div>
          </td>
        </tr>
      ) : null}
    </>
  )
}

function PublicationsPanel({ review, mrUrl }: { review: ReviewDetail; mrUrl?: string }) {
  const publications = review.publications
  return (
    <Panel title="发布结果">
      {!publications ? (
        <EmptyState title="尚无发布结果记录" description="服务端详情没有 publications 字段；这不表示发布成功。" />
      ) : (
        <div className={styles.publicationGrid}>
          <div className={styles.publicationCard}>
            <span className={styles.publicationLabel}>汇总评论</span>
            <span className={styles.publicationValue}>
              {publications.summary_note_id ? `note #${publications.summary_note_id}` : '尚未发布'}
            </span>
            {publications.summary_note_id ? (
              <span className={styles.tableMeta}>
                {mrUrl ? <a href={`${mrUrl}#note_${publications.summary_note_id}`} target="_blank" rel="noopener noreferrer">在 MR 中查看</a> : 'note ID 来自评审详情'}
              </span>
            ) : null}
          </div>
          <div className={styles.publicationCard}>
            <span className={styles.publicationLabel}>行内讨论</span>
            <span className={styles.publicationValue}>
              {typeof publications.discussions === 'number' ? `${publications.discussions} 条` : '不可用'}
            </span>
          </div>
          <div className={styles.publicationCard}>
            <span className={styles.publicationLabel}>commit status</span>
            <span className={styles.publicationValue}>{publications.commit_status || '不可用'}</span>
            {publications.commit_status === 'failed' ? <span className={styles.tableMeta}>发布状态失败，不能视为 PASS。</span> : null}
          </div>
          {Object.entries(publications)
            .filter(([key]) => !['summary_note_id', 'discussions', 'commit_status'].includes(key))
            .map(([key, value]) => (
              <div className={styles.publicationCard} key={key}>
                <span className={styles.publicationLabel}>{key}</span>
                <span className={styles.publicationValue}>{typeof value === 'object' ? formatJson(value) : String(value ?? '—')}</span>
              </div>
            ))}
        </div>
      )}
    </Panel>
  )
}

function challengeTone(status?: string): 'ok' | 'warn' | 'neutral' {
  if (status === 'confirmed') return 'ok'
  if (status === 'uncertain') return 'warn'
  return 'neutral'
}

export function ReviewDetailPage() {
  const { id = '' } = useParams()
  const canWrite = useCanWrite()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [decision, setDecision] = useState('')
  const [severity, setSeverity] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [selectedNode, setSelectedNode] = useState<GraphNode | null>(null)
  const [cancelOpen, setCancelOpen] = useState(false)
  const [cancelPending, setCancelPending] = useState(false)
  const [cancelError, setCancelError] = useState<unknown>(null)
  const [republishing, setRepublishing] = useState(false)
  const [triggerOpen, setTriggerOpen] = useState(false)

  const detail = useQuery({
    queryKey: ['review', id, 'detail'],
    queryFn: () => getReview(id),
    enabled: Boolean(id),
  })
  const graph = useQuery({
    queryKey: ['review', id, 'graph'],
    queryFn: () => getReviewGraph(id),
    enabled: Boolean(id),
  })
  const findings = usePaginated<Finding>(
    ['review', id, 'findings', decision, severity],
    (cursor) => listFindings(id, { decision: decision || undefined, severity: severity || undefined }, cursor),
    'findings',
    { enabled: Boolean(id) },
  )
  const project = useQuery({
    queryKey: ['project', detail.data?.project_id],
    queryFn: () => getProject(detail.data!.project_id),
    enabled: Boolean(detail.data?.project_id),
  })
  const allProjects = useQuery({
    queryKey: ['projects', 'review-trigger'],
    queryFn: () => listProjects({ limit: 100 }),
    enabled: canWrite,
  })

  const stream = useReviewStream(id, detail.data)
  const projectNames = useMemo(
    () => new Map((allProjects.data?.projects || []).map((item) => [item.id, item.path_with_namespace])),
    [allProjects.data],
  )

  useEffect(() => {
    setExpanded(new Set())
  }, [id, decision, severity])

  if (!id) return <main className={ui.page}><ErrorState error={new Error('URL 中没有评审 ID。')} title="无法打开评审" /></main>
  if (detail.isPending) return <main className={ui.page}><LoadingState label="正在读取评审详情…" /></main>
  if (detail.isError) {
    return (
      <main className={ui.page}>
        <div className={ui.pageStack}>
          <Link to="/reviews">← 返回评审列表</Link>
          <ErrorState error={detail.error} onRetry={() => { void detail.refetch() }} title="评审详情加载失败" />
        </div>
      </main>
    )
  }

  const review = detail.data
  const projectPath = project.data?.path_with_namespace || review.project_path || projectNames.get(review.project_id) || `项目 ${review.project_id}`
  const mrUrl = safeExternalUrl(
    project.data?.web_url
      ? `${project.data.web_url.replace(/\/$/, '')}/-/merge_requests/${review.mr_iid}`
      : undefined,
  )
  const canCancel = canWrite && (review.status === 'running' || review.status === 'pending')
  const canRepublish = canWrite && review.status === 'completed'

  const onCancel = async () => {
    if (!canWrite) return
    setCancelPending(true)
    setCancelError(null)
    try {
      await cancelReview(id)
      setCancelOpen(false)
      toast.push('success', '已请求取消评审。')
      await queryClient.invalidateQueries({ queryKey: ['review', id] })
      void queryClient.invalidateQueries({ queryKey: ['reviews'] })
    } catch (error) {
      setCancelError(error)
    } finally {
      setCancelPending(false)
    }
  }

  const onRepublish = async () => {
    if (!canWrite) return
    setRepublishing(true)
    try {
      const result = await republishReview(id)
      toast.push('success', typeof result?.job_id === 'string' ? `已创建重新发布任务 ${result.job_id}。` : '已请求重新发布。')
      await queryClient.invalidateQueries({ queryKey: ['review', id] })
    } catch (error) {
      toast.push('error', error instanceof Error ? error.message : '重新发布失败。')
    } finally {
      setRepublishing(false)
    }
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <div className={styles.docMeta}>
          <Link to="/reviews">评审</Link>
          <span>/</span>
          <span>{projectPath}</span>
          <span>/</span>
          <span>!{review.mr_iid}</span>
        </div>

        <Panel>
          <div className={styles.reviewHeader}>
            <div className={ui.stack} style={{ gap: 10, flex: '1 1 520px' }}>
              <div className={styles.reviewTitleRow}>
                <VerdictBadge verdict={review.verdict} />
                <StatusBadge status={review.status} />
                <h1 className={styles.reviewTitle}>!{review.mr_iid} {review.mr_title || '未命名 MR'}</h1>
              </div>
              <div className={styles.reviewMeta}>
                <span>项目 <span className="mono">{projectPath}</span></span>
                <span>head <code>{shortSha(review.head_sha)}</code></span>
                <span>档位 {review.profile || '—'}</span>
                <span>触发 {labelForTrigger(review.trigger)}</span>
                <span>用时 <span className="tabular">{formatDuration(review.duration_ms)}</span></span>
                <span>tokens <span className="tabular">{formatTokens(review.usage?.tokens_in, review.usage?.tokens_out)}</span></span>
                <span>成本 <span className="tabular">{formatCost(review.usage?.cost_micros)}</span></span>
              </div>
              {review.error ? <div className={styles.warning} role="alert">执行失败：{review.error}</div> : null}
              {review.cancel_reason ? <div className={styles.notice}>取消原因：{review.cancel_reason}</div> : null}
            </div>
            <div className={styles.cardActions} style={{ justifyContent: 'flex-end' }}>
              {mrUrl ? (
                <a href={mrUrl} target="_blank" rel="noopener noreferrer" style={secondaryLinkStyle}>在 GitLab 中打开</a>
              ) : null}
              {canWrite ? <Button variant="secondary" onClick={() => setTriggerOpen(true)}>重新触发评审</Button> : null}
              {canRepublish ? <Button variant="secondary" onClick={() => { void onRepublish() }} loading={republishing}>重新发布</Button> : null}
              {canCancel ? <Button variant="primary" onClick={() => setCancelOpen(true)}>取消评审</Button> : null}
            </div>
          </div>
          <div className={styles.docMeta} role="status">
            <span>创建 <RelativeTime value={review.created_at} /></span>
            {stream.state === 'open' ? <span className={styles.resultOk}>实时更新已连接</span> : null}
            {stream.state === 'connecting' ? <span>正在连接实时更新…</span> : null}
            {stream.state === 'closed' ? <span className="muted">{stream.terminal ? '评审已结束，实时连接已关闭。' : '实时更新未连接。'}</span> : null}
            {stream.state === 'error' ? (
              <span className={styles.resultError}>
                {stream.error || '实时更新中断。'}
                <button type="button" className={styles.expandButton} onClick={stream.retry}>重试连接</button>
              </span>
            ) : null}
          </div>
        </Panel>

        <Panel
          title="执行图"
          aside={<span>节点状态与依赖图 · {graph.isSuccess ? `${graph.data.nodes.length} 个节点` : ''}</span>}
          className={styles.mainColumn}
        >
          {graph.isPending ? <LoadingState label="正在读取执行图…" /> : null}
          {graph.isError ? <ErrorState error={graph.error} onRetry={() => { void graph.refetch() }} title="执行图加载失败" /> : null}
          {graph.isSuccess ? (
            <ExecutionGraph
              nodes={graph.data.nodes}
              edges={graph.data.edges}
              selectedId={selectedNode?.id}
              onSelect={(node) => setSelectedNode(node)}
            />
          ) : null}
        </Panel>

        <Panel
          title="问题"
          aside={`${findings.isSuccess ? `${findings.items.length}${findings.hasNextPage ? '+' : ''} 条当前筛选结果` : '正在读取'} · 正文与证据以纯文本展示`}
        >
          <div className={styles.inlineFilters}>
            <div role="group" aria-label="按处置过滤" className={styles.filterChips}>
              {decisionOptions.map(([value, label]) => (
                <button
                  key={value || 'all'}
                  type="button"
                  className={[styles.chip, decision === value ? styles.chipActive : ''].filter(Boolean).join(' ')}
                  aria-pressed={decision === value}
                  onClick={() => setDecision(value)}
                >
                  {label}
                </button>
              ))}
            </div>
            <label style={{ ...fieldStyle, flex: '0 1 180px' }}>
              <span className="small">级别</span>
              <select className="select" name="severity" value={severity} onChange={(event) => setSeverity(event.target.value)} aria-label="按严重级别过滤">
                {severityOptions.map(([value, label]) => <option key={value || 'all'} value={value}>{label}</option>)}
              </select>
            </label>
          </div>
          {findings.isPending ? <LoadingState label="正在读取问题…" /> : null}
          {findings.isError ? <ErrorState error={findings.error} onRetry={findings.refetch} /> : null}
          {findings.isSuccess && findings.items.length === 0 ? <EmptyState title="没有符合条件的问题" description="调整处置或级别筛选；这不表示评审没有产生过问题。" /> : null}
          {findings.items.length ? (
            <div className="tableWrap">
              <table className={`dataTable ${styles.findingsTable}`}>
                <thead>
                  <tr>
                    <th scope="col">级别</th>
                    <th scope="col">问题</th>
                    <th scope="col">位置</th>
                    <th scope="col">来源</th>
                    <th scope="col">挑战</th>
                    <th scope="col">处置</th>
                    <th scope="col">理由</th>
                  </tr>
                </thead>
                <tbody>
                  {findings.items.map((finding, index) => (
                    <FindingRow
                      key={finding.id}
                      finding={finding}
                      index={index}
                      expanded={expanded.has(finding.id)}
                      onToggle={() => {
                        setExpanded((current) => {
                          const next = new Set(current)
                          if (next.has(finding.id)) next.delete(finding.id)
                          else next.add(finding.id)
                          return next
                        })
                      }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
          {findings.hasNextPage ? (
            <div className={styles.actionsRight}>
              <Button variant="secondary" onClick={findings.fetchNextPage} loading={findings.isFetchingNextPage}>加载更多问题</Button>
            </div>
          ) : null}
        </Panel>

        <PublicationsPanel review={review} mrUrl={mrUrl} />
      </div>

      <NodeDrawer reviewId={id} node={selectedNode} onClose={() => setSelectedNode(null)} />

      <ConfirmDialog
        open={cancelOpen}
        onClose={() => { setCancelOpen(false); setCancelError(null) }}
        onConfirm={() => { void onCancel() }}
        title="取消评审？"
        description={`将请求取消 !${review.mr_iid} 的当前评审。工作流可能已有部分结果，取消后不会被视为 PASS。`}
        confirmLabel="取消评审"
        pending={cancelPending}
        error={cancelError}
      />

      {canWrite ? (
        <TriggerReviewDialog
          open={triggerOpen}
          onClose={() => setTriggerOpen(false)}
          projects={allProjects.data?.projects || []}
          presetProjectId={review.project_id}
          presetIid={review.mr_iid}
          presetProjectLabel={projectPath}
          onTriggered={(jobId) => {
            setTriggerOpen(false)
            toast.push('success', jobId ? `已创建评审任务 ${jobId}。` : '已创建评审任务。')
            void queryClient.invalidateQueries({ queryKey: ['reviews'] })
          }}
        />
      ) : null}
    </main>
  )
}

const evidenceStyle = {
  background: 'var(--surface)',
  border: '1px solid var(--border)',
  borderRadius: 6,
  padding: 10,
  maxHeight: 240,
  overflow: 'auto',
  color: 'var(--text)',
} as const

const secondaryLinkStyle = {
  display: 'inline-flex',
  alignItems: 'center',
  minHeight: 44,
  padding: '0 16px',
  borderRadius: 8,
  border: '1px solid var(--control-border)',
  background: 'var(--surface)',
  color: 'var(--text)',
  fontWeight: 500,
  textDecoration: 'none',
} as const

const fieldStyle = {
  display: 'flex',
  flexDirection: 'column',
  gap: 6,
  minWidth: 0,
} as const
