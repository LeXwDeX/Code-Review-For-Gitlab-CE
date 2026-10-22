import { useEffect, useId, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { listProfiles, listProjects, registerProject, updateProject } from '../api/api'
import { ApiError, errorMessage } from '../api/client'
import type { Project, ProjectRegistration } from '../api/types'
import { useCanWrite } from '../components/RoleContext'
import {
  Badge, Button, Dialog, EmptyState, ErrorState, Field, LoadingState, PageHeader, Panel, SecretField,
  StatusBadge, Switch, useToast,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import { formatDate, labelForStatus } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

function tokenExpiry(project: Project): { label: string; tone: 'ok' | 'warn' | 'danger' | 'neutral' } {
  if (!project.token_expires_at) return { label: '未提供', tone: 'neutral' }
  const expires = new Date(project.token_expires_at).getTime()
  if (Number.isNaN(expires)) return { label: '日期无效', tone: 'warn' }
  const days = Math.ceil((expires - Date.now()) / 86_400_000)
  if (days <= 0) return { label: '已过期', tone: 'danger' }
  if (days <= 30) return { label: `${days} 天后到期`, tone: 'warn' }
  return { label: formatDate(project.token_expires_at), tone: 'neutral' }
}

function fieldErrorsFrom(error: unknown): Record<string, string> {
  if (!(error instanceof ApiError)) return {}
  const result: Record<string, string> = {}
  for (const detail of error.details || []) {
    if (detail.field) result[detail.field] = detail.message || '输入无效。'
  }
  return result
}

function RegisterProjectDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const projectId = useId()
  const tokenId = useId()
  const profileId = useId()
  const branchesId = useId()
  const [project, setProject] = useState('')
  const [token, setToken] = useState('')
  const [profile, setProfile] = useState('default')
  const [autoWebhook, setAutoWebhook] = useState(true)
  const [skipDraft, setSkipDraft] = useState(true)
  const [branches, setBranches] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [result, setResult] = useState<ProjectRegistration | null>(null)

  const profiles = useQuery({
    queryKey: ['profiles', 'register'],
    queryFn: () => listProfiles(),
    enabled: open,
  })

  useEffect(() => {
    if (!open) {
      setProject('')
      setToken('')
      setProfile('default')
      setAutoWebhook(true)
      setSkipDraft(true)
      setBranches('')
      setPending(false)
      setError(null)
      setErrors({})
      setResult(null)
    }
  }, [open])

  const close = () => {
    if (pending) return
    onClose()
  }

  const submit = async () => {
    const nextErrors: Record<string, string> = {}
    if (!project.trim()) nextErrors.project = '请输入项目路径或 ID。'
    if (!token.trim()) nextErrors.token = '请输入 Project Access Token。'
    setErrors(nextErrors)
    if (Object.keys(nextErrors).length) return
    setPending(true)
    setError(null)
    try {
      const settings: Record<string, unknown> = { skip_draft: skipDraft }
      const targets = branches.split(',').map((value) => value.trim()).filter(Boolean)
      if (targets.length) settings.target_branches = targets
      const registration = await registerProject({
        project: project.trim(),
        token: token.trim(),
        profile: profile.trim() || 'default',
        auto_webhook: autoWebhook,
        settings,
      })
      setResult(registration)
      setToken('')
      setErrors({})
      toast.push('success', registration.job_id ? `项目已注册，onboarding 任务 ${registration.job_id} 已创建。` : '项目已注册。')
      void queryClient.invalidateQueries({ queryKey: ['projects'] })
      void queryClient.invalidateQueries({ queryKey: ['stats'] })
    } catch (caught) {
      setError(caught)
      setErrors(fieldErrorsFrom(caught))
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title={result ? '项目已注册' : '注册项目'}
      description={result
        ? '项目已入库。一次性凭据只在本对话框打开期间可见，关闭后无法再次获取。'
        : '服务端会使用 token 校验项目访问权限和 scope，然后创建 webhook、入库并启动 onboarding。'}
      wide
      footer={result ? (
        <Button variant="primary" onClick={close}>完成</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={close} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>校验并注册</Button>
        </>
      )}
    >
      {result ? (
        <div className={styles.list}>
          <dl className={ui.definitionGrid}>
            <div><dt>项目</dt><dd className="mono">{result.project?.path_with_namespace || project}</dd></div>
            <div><dt>项目 ID</dt><dd className="tabular">{result.project?.id ?? '—'}</dd></div>
            <div><dt>默认分支</dt><dd className="mono">{result.project?.default_branch || '—'}</dd></div>
            <div><dt>档位</dt><dd>{result.project?.profile || '—'}</dd></div>
            <div><dt>token scope</dt><dd className="mono">{result.project?.token_scopes?.join(', ') || '—'}</dd></div>
            <div><dt>token 过期</dt><dd>{result.project?.token_expires_at ? formatDate(result.project.token_expires_at) : '未提供'}</dd></div>
            <div><dt>onboarding 任务</dt><dd className="mono">{result.job_id || '—'}</dd></div>
            <div><dt>webhook</dt><dd>{result.webhook_secret ? '需手工配置' : autoWebhook ? '已自动创建' : '未创建'}</dd></div>
          </dl>
          {result.webhook_secret ? (
            <>
              <div className={styles.warning}>
                服务端只在注册响应中返回 webhook secret。请立即复制并填写到 GitLab webhook 的 Secret token；关闭对话框后不可再查看。
              </div>
              <SecretField label="Webhook secret" value={result.webhook_secret} />
            </>
          ) : null}
        </div>
      ) : (
        <>
          {error ? <ErrorState error={error} title="注册失败" /> : null}
          <div className="formGrid">
            <Field label="项目路径或 ID" htmlFor={projectId} error={errors.project || errors.Project} hint="group/project 或 GitLab 项目 ID">
              <input id={projectId} className="input" value={project} onChange={(event) => setProject(event.target.value)} placeholder="group/project" autoComplete="off" aria-invalid={Boolean(errors.project)} />
            </Field>
            <Field label="Project Access Token" htmlFor={tokenId} error={errors.token || errors.Token} hint="需要 api 和 read_repository scope；只提交给服务端。">
              <input id={tokenId} className="input" type="password" value={token} onChange={(event) => setToken(event.target.value)} autoComplete="new-password" aria-invalid={Boolean(errors.token)} />
            </Field>
            <Field label="评审档位" htmlFor={profileId} hint="从平台配置档位中选择，或输入已有档位名。">
              <input id={profileId} className="input" list="profile-options" value={profile} onChange={(event) => setProfile(event.target.value)} autoComplete="off" />
              <datalist id="profile-options">
                {(profiles.data?.profiles || []).map((item) => <option key={item.name} value={item.name} />)}
              </datalist>
            </Field>
            <Field label="目标分支" htmlFor={branchesId} hint="逗号分隔；留空沿用平台默认分支设置。">
              <input id={branchesId} className="input" value={branches} onChange={(event) => setBranches(event.target.value)} placeholder="main, release/*" autoComplete="off" />
            </Field>
          </div>
          <div className={styles.filterChips} style={{ flexDirection: 'column', alignItems: 'flex-start' }}>
            <label className="checkboxRow">
              <input type="checkbox" checked={autoWebhook} onChange={(event) => setAutoWebhook(event.target.checked)} />
              <span>自动创建 GitLab webhook（需要 Maintainer 权限）</span>
            </label>
            <label className="checkboxRow">
              <input type="checkbox" checked={skipDraft} onChange={(event) => setSkipDraft(event.target.checked)} />
              <span>跳过 Draft MR</span>
            </label>
          </div>
          {profiles.isError ? <span className="small muted">档位列表加载失败，仍可输入档位名。</span> : null}
          <div className={styles.notice}>
            提交后显示服务端校验结果：项目可访问性、token scope 和过期时间、webhook 创建结果。
          </div>
        </>
      )}
    </Dialog>
  )
}

export function ProjectsPage() {
  const canWrite = useCanWrite()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [search, setSearch] = useState('')
  const [enabled, setEnabled] = useState('')
  const [debouncedSearch, setDebouncedSearch] = useState('')
  const [registerOpen, setRegisterOpen] = useState(false)
  const [pendingIds, setPendingIds] = useState<Set<number>>(new Set())

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedSearch(search.trim()), 300)
    return () => window.clearTimeout(timer)
  }, [search])

  const projects = usePaginated<Project>(
    ['projects', 'list', debouncedSearch, enabled],
    (cursor) => listProjects({
      q: debouncedSearch || undefined,
      enabled: enabled === '' ? undefined : enabled === 'true',
    }, cursor),
    'projects',
  )

  const toggleProject = async (project: Project, next: boolean) => {
    if (!canWrite) return
    setPendingIds((current) => new Set(current).add(project.id))
    try {
      await updateProject(project.id, { enabled: next })
      toast.push('success', `${project.path_with_namespace} 已${next ? '启用' : '停用'}。`)
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
    } catch (error) {
      toast.push('error', `更新失败：${errorMessage(error)}`)
    } finally {
      setPendingIds((current) => {
        const nextSet = new Set(current)
        nextSet.delete(project.id)
        return nextSet
      })
    }
  }

  const enabledCount = projects.items.filter((project) => project.enabled).length

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="项目"
          description="每个项目使用自己的 Project Access Token；评审结果以该 token 的 bot 用户身份发布。"
          actions={canWrite ? <Button variant="primary" onClick={() => setRegisterOpen(true)}>注册项目</Button> : undefined}
        />

        <Panel
          title="已注册项目"
          aside={projects.isSuccess ? `已加载 ${projects.items.length}${projects.hasNextPage ? '+' : ''} 个项目 · ${enabledCount} 个启用` : undefined}
        >
          <div className={ui.toolbar} style={{ justifyContent: 'space-between' }}>
            <div className={styles.inlineFilters}>
              <label style={{ ...fieldStyle, flex: '1 1 280px' }}>
                <span className="small">搜索项目路径</span>
                <input className="input" name="q" type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索项目路径" aria-label="搜索项目路径" />
              </label>
              <label style={{ ...fieldStyle, flex: '0 1 160px' }}>
                <span className="small">启用状态</span>
                <select className="select" name="enabled" value={enabled} onChange={(event) => setEnabled(event.target.value)} aria-label="按启用状态筛选">
                  <option value="">全部</option>
                  <option value="true">已启用</option>
                  <option value="false">已停用</option>
                </select>
              </label>
            </div>
            {projects.isFetching ? <span className="small muted" role="status">刷新中…</span> : null}
          </div>

          {projects.isPending ? <LoadingState /> : null}
          {projects.isError ? <ErrorState error={projects.error} onRetry={projects.refetch} /> : null}
          {projects.isSuccess && projects.items.length === 0 ? (
            <EmptyState
              title={debouncedSearch || enabled ? '没有符合条件的项目' : '还没有注册项目'}
              description={debouncedSearch || enabled ? '调整搜索或启用状态筛选。' : '注册第一个 GitLab 项目后，这里会显示工作区、token 和 webhook 状态。'}
              action={canWrite && !debouncedSearch && !enabled ? <Button variant="primary" onClick={() => setRegisterOpen(true)}>注册项目</Button> : undefined}
            />
          ) : null}
          {projects.items.length ? (
            <div className="tableWrap">
              <table className="dataTable" style={{ minWidth: 980 }}>
                <thead>
                  <tr>
                    <th scope="col">项目</th>
                    <th scope="col">档位</th>
                    <th scope="col">工作区</th>
                    <th scope="col">最近评审</th>
                    <th scope="col">token 过期</th>
                    <th scope="col">webhook</th>
                    <th scope="col">启用</th>
                  </tr>
                </thead>
                <tbody>
                  {projects.items.map((project) => {
                    const expiry = tokenExpiry(project)
                    const workspaceState = project.workspace?.state || project.workspace_state
                    const lastReview = project.last_review
                    return (
                      <tr key={project.id}>
                        <td>
                          <div className={styles.tablePrimary}>
                            <Link to={`/projects/${project.id}`} className="mono">{project.path_with_namespace}</Link>
                            <span className={styles.tableMeta}>ID {project.id}{project.default_branch ? ` · 默认分支 ${project.default_branch}` : ''}</span>
                          </div>
                        </td>
                        <td>{project.profile || '—'}</td>
                        <td>{workspaceState ? <StatusBadge status={workspaceState} label={labelForStatus(workspaceState)} /> : <span className="muted">未提供</span>}</td>
                        <td>
                          {lastReview ? (
                            <div className={styles.tablePrimary}>
                              <Link to={`/reviews/${encodeURIComponent(lastReview.id)}`}>!{lastReview.mr_iid} {lastReview.mr_title || ''}</Link>
                              <span className={styles.tableMeta}>{labelForStatus(lastReview.status)}</span>
                            </div>
                          ) : <span className="muted">—</span>}
                        </td>
                        <td><Badge tone={expiry.tone}>{expiry.label}</Badge></td>
                        <td>{project.hook_id ? <Badge tone="ok">本服务创建</Badge> : <Badge tone="neutral">手动配置</Badge>}</td>
                        <td>
                          {canWrite ? (
                            <Switch
                              checked={project.enabled}
                              label={`启用 ${project.path_with_namespace}`}
                              disabled={pendingIds.has(project.id)}
                              onChange={(next) => { void toggleProject(project, next) }}
                            />
                          ) : <Badge tone={project.enabled ? 'ok' : 'neutral'}>{project.enabled ? '启用' : '停用'}</Badge>}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          ) : null}
          {projects.hasNextPage ? (
            <div className={styles.actionsRight}>
              <Button variant="secondary" onClick={projects.fetchNextPage} loading={projects.isFetchingNextPage}>加载更多项目</Button>
            </div>
          ) : null}
        </Panel>
      </div>

      {canWrite ? <RegisterProjectDialog open={registerOpen} onClose={() => setRegisterOpen(false)} /> : null}
    </main>
  )
}

const fieldStyle = { display: 'flex', flexDirection: 'column', gap: 6, minWidth: 0 } as const
