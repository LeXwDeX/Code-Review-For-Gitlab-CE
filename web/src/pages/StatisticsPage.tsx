import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getStats, listProjects } from '../api/api'
import type { Stats } from '../api/types'
import {
  Badge, Button, EmptyState, ErrorState, LoadingState, Metric, PageHeader, Panel,
} from '../components/ui'
import { formatCost } from '../lib/format'
import { formatMs, formatRatio, statNumber } from '../lib/stats'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

type Row = Record<string, unknown>

function asRows(value: unknown): Row[] {
  if (Array.isArray(value)) return value.filter((item): item is Row => Boolean(item) && typeof item === 'object' && !Array.isArray(item))
  if (value && typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>).map(([key, item]) => ({
      key,
      ...(item && typeof item === 'object' && !Array.isArray(item) ? item as Row : { value: item }),
    }))
  }
  return []
}

function decisionRows(value: unknown): Array<{ label: string; count: number }> {
  if (Array.isArray(value)) {
    return value.flatMap((item) => {
      if (!item || typeof item !== 'object') return []
      const row = item as Row
      const label = String(row.decision ?? row.key ?? row.name ?? '未知')
      const count = Number(row.count ?? row.value)
      return Number.isFinite(count) ? [{ label, count }] : []
    })
  }
  if (value && typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>).flatMap(([label, count]) => {
      const numeric = Number(count)
      return Number.isFinite(numeric) ? [{ label, count: numeric }] : []
    })
  }
  return []
}

function cellValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'number') return Number.isInteger(value) ? new Intl.NumberFormat('zh-CN').format(value) : String(value)
  if (typeof value === 'string') return value
  if (typeof value === 'boolean') return value ? 'true' : 'false'
  try { return JSON.stringify(value) } catch { return String(value) }
}

function DimensionTable({ rows, emptyLabel }: { rows: Row[]; emptyLabel: string }) {
  const columns = useMemo(() => {
    const set = new Set<string>()
    for (const row of rows.slice(0, 12)) for (const key of Object.keys(row)) set.add(key)
    return Array.from(set)
  }, [rows])
  if (!rows.length) return <EmptyState title={emptyLabel} description="stats API 没有返回该维度的数据。" />
  return (
    <div className="tableWrap">
      <table className="dataTable" style={{ minWidth: Math.max(560, columns.length * 130) }}>
        <thead><tr>{columns.map((column) => <th key={column} scope="col">{column}</th>)}</tr></thead>
        <tbody>
          {rows.map((row, index) => (
            <tr key={String(row.id ?? row.key ?? index)}>
              {columns.map((column) => <td key={column} className={typeof row[column] === 'number' ? 'tabular' : undefined}>{cellValue(row[column])}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function StatsContent({ stats }: { stats: Stats }) {
  const decisions = decisionRows(stats.decisions)
  const byProject = asRows(stats.by_project)
  const byModel = asRows(stats.by_model)
  const runs = statNumber(stats, 'runs')
  const blocking = statNumber(stats, 'blocking')
  const published = statNumber(stats, 'published')

  return (
    <>
      <div className={styles.statGrid}>
        <Metric label="评审次数" value={runs === null ? '不可用' : runs} note={blocking === null ? 'BLOCKING 次数不可用' : `BLOCKING ${blocking} 次`} />
        <Metric label="BLOCKING 比例" value={formatRatio(statNumber(stats, 'blocking_ratio'))} note="来自 stats.blocking_ratio" />
        <Metric label="已发布问题" value={published === null ? '不可用' : published} note="来自 stats.published" />
        <Metric label="被挑战推翻" value={formatRatio(statNumber(stats, 'overturned_ratio'))} note="来自 stats.overturned_ratio" />
        <Metric label="平均用时" value={formatMs(statNumber(stats, 'avg_duration_ms'))} note={`P95 ${formatMs(statNumber(stats, 'p95_duration_ms'))}`} />
        <Metric label="成本" value={(() => { const cost = statNumber(stats, 'cost_micros'); return cost === null ? '不可用' : formatCost(cost) })()} note="按 API 的 micros 原值换算" />
      </div>

      <Panel title="问题处置分布" aside="来自 stats.decisions">
        {decisions.length ? (
          <div className={styles.budgetRow}>
            {decisions.map((row) => <Badge key={row.label} tone={row.label === 'publish' ? 'ok' : row.label === 'mark' ? 'warn' : 'neutral'}>{row.label}: {row.count}</Badge>)}
          </div>
        ) : <EmptyState title="没有处置分布数据" description="stats.decisions 缺失或为空；显示不可用，而不是零值。" />}
        <div className={styles.notice}>
          当前 API 契约未定义反馈写入或抽样标注接口，因此这里不显示标注入口；只呈现 stats 已计算的反证指标。
        </div>
      </Panel>

      <Panel title="按项目" aside="from stats.by_project">
        <DimensionTable rows={byProject} emptyLabel="没有按项目统计" />
      </Panel>
      <Panel title="按模型" aside="from stats.by_model">
        <DimensionTable rows={byModel} emptyLabel="没有按模型统计" />
      </Panel>

      <details>
        <summary style={{ minHeight: 44, display: 'flex', alignItems: 'center', cursor: 'pointer' }}>查看原始 stats 响应</summary>
        <pre className="pre statsJson" style={jsonStyle}>{JSON.stringify(stats, null, 2)}</pre>
      </details>
    </>
  )
}

export function StatisticsPage() {
  const initial = useMemo(() => {
    const to = new Date()
    const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000)
    const localDate = (date: Date) => `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
    return {
      fromDate: localDate(from),
      toDate: localDate(to),
      projectId: '',
    }
  }, [])
  const [fromDate, setFromDate] = useState(initial.fromDate)
  const [toDate, setToDate] = useState(initial.toDate)
  const [projectId, setProjectId] = useState(initial.projectId)
  const [applied, setApplied] = useState(initial)
  const [validation, setValidation] = useState('')

  const projects = useQuery({ queryKey: ['projects', 'statistics'], queryFn: () => listProjects({ limit: 100 }) })
  const from = new Date(`${applied.fromDate}T00:00:00`).toISOString()
  const to = new Date(`${applied.toDate}T23:59:59.999`).toISOString()
  const stats = useQuery({
    queryKey: ['stats', 'page', from, to, applied.projectId],
    queryFn: () => getStats({ from, to, project_id: applied.projectId || undefined }),
  })

  const apply = () => {
    if (!fromDate || !toDate) { setValidation('请选择开始和结束日期。'); return }
    if (fromDate > toDate) { setValidation('开始日期不能晚于结束日期。'); return }
    setValidation('')
    setApplied({ fromDate, toDate, projectId })
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="统计与反馈"
          description="按时间与项目查看实际运行统计、问题处置和模型维度数据；不可用的值不会显示为零。"
        />
        <Panel title="筛选">
          <div className={styles.inlineFilters}>
            <label style={fieldStyle}>
              <span className="small">开始日期</span>
              <input className="input" name="from" type="date" value={fromDate} onChange={(event) => setFromDate(event.target.value)} />
            </label>
            <label style={fieldStyle}>
              <span className="small">结束日期</span>
              <input className="input" name="to" type="date" value={toDate} onChange={(event) => setToDate(event.target.value)} />
            </label>
            <label style={fieldStyle}>
              <span className="small">项目</span>
              <select className="select" name="project_id" value={projectId} onChange={(event) => setProjectId(event.target.value)}>
                <option value="">全部项目</option>
                {(projects.data?.projects || []).map((project) => <option key={project.id} value={project.id}>{project.path_with_namespace}</option>)}
              </select>
            </label>
            <Button variant="primary" onClick={apply}>应用</Button>
          </div>
          {validation ? <span className={styles.resultError} role="alert">{validation}</span> : null}
          {projects.isError ? <span className="small muted">项目筛选列表加载失败，仍可查看全部项目统计。</span> : null}
        </Panel>

        {stats.isPending ? <LoadingState label="正在读取统计数据…" /> : null}
        {stats.isError ? <ErrorState error={stats.error} onRetry={() => { void stats.refetch() }} title="统计数据加载失败" /> : null}
        {stats.isSuccess && !stats.data ? <EmptyState title="stats 返回了空响应" description="页面不会用示例数据补全；请检查服务端统计实现。" /> : null}
        {stats.data ? <StatsContent stats={stats.data} /> : null}
      </div>
    </main>
  )
}

const fieldStyle = { display: 'flex', flexDirection: 'column', gap: 6, minWidth: 0 } as const
const jsonStyle = { background: 'var(--surface-2)', border: '1px solid var(--border)', borderRadius: 8, padding: 12, color: 'var(--text)' } as const
