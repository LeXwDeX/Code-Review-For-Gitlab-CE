import { useEffect, useId, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  listProfiles, recreateWebhook, rotateProjectToken, unregisterProject, updateProject,
} from '../../api/api'
import { ApiError, errorMessage } from '../../api/client'
import type { Project } from '../../api/types'
import { useCanWrite } from '../../components/RoleContext'
import {
  Badge, Button, ConfirmDialog, Dialog, ErrorState, Field, LoadingState, Panel, Switch, useToast,
} from '../../components/ui'
import { formatDate } from '../../lib/format'
import ui from '../../styles/ui.module.css'
import styles from '../../pages/pages.module.css'

type SettingsDraft = Record<string, unknown>

function splitComma(value: unknown): string {
  return Array.isArray(value) ? value.join(', ') : ''
}

function splitLines(value: unknown): string {
  return Array.isArray(value) ? value.join('\n') : ''
}

function parseComma(value: string): string[] {
  return value.split(',').map((item) => item.trim()).filter(Boolean)
}

function parseLines(value: string): string[] {
  return value.split('\n').map((item) => item.trim()).filter(Boolean)
}

function BoolSetting({
  label, value, onChange, disabled,
}: { label: string; value: unknown; onChange: (next: unknown) => void; disabled?: boolean }) {
  const id = useId()
  const current = value === true ? 'true' : value === false ? 'false' : ''
  return (
    <Field label={label} htmlFor={id} hint="选择“继承平台默认”时不会写入项目覆盖值。">
      <select
        id={id}
        className="select"
        value={current}
        disabled={disabled}
        onChange={(event) => {
          const next = event.target.value
          onChange(next === '' ? undefined : next === 'true')
        }}
      >
        <option value="">继承平台默认</option>
        <option value="true">启用</option>
        <option value="false">禁用</option>
      </select>
    </Field>
  )
}

export function SettingsPanel({ project }: { project: Project }) {
  const canWrite = useCanWrite()
  const toast = useToast()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const profileId = useId()
  const [draft, setDraft] = useState<SettingsDraft>({ ...(project.settings || {}) })
  const [profile, setProfile] = useState(project.profile || 'default')
  const [jsonText, setJsonText] = useState(() => JSON.stringify(project.settings || {}, null, 2))
  const [jsonError, setJsonError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<unknown>(null)
  const [pendingEnabled, setPendingEnabled] = useState(false)
  const [rotateOpen, setRotateOpen] = useState(false)
  const [token, setToken] = useState('')
  const [rotating, setRotating] = useState(false)
  const [rotateError, setRotateError] = useState<unknown>(null)
  const [hookOpen, setHookOpen] = useState(false)
  const [hookPending, setHookPending] = useState(false)
  const [hookError, setHookError] = useState<unknown>(null)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [deletePending, setDeletePending] = useState(false)
  const [deleteError, setDeleteError] = useState<unknown>(null)

  const profiles = useQuery({
    queryKey: ['profiles', 'project-settings'],
    queryFn: () => listProfiles(),
    enabled: canWrite,
  })

  useEffect(() => {
    setDraft({ ...(project.settings || {}) })
    setProfile(project.profile || 'default')
    setJsonText(JSON.stringify(project.settings || {}, null, 2))
    setJsonError(null)
    setSaveError(null)
  }, [project.id])

  const updateDraft = (key: string, value: unknown) => {
    setDraft((current) => {
      const next = { ...current }
      if (value === undefined || value === '' || (Array.isArray(value) && value.length === 0)) delete next[key]
      else next[key] = value
      return next
    })
  }

  const applyJson = () => {
    try {
      const parsed = JSON.parse(jsonText) as unknown
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        setJsonError('JSON 必须是对象。')
        return
      }
      setDraft(parsed as SettingsDraft)
      setJsonError(null)
      setSaveError(null)
    } catch (error) {
      setJsonError(error instanceof Error ? `JSON 解析失败：${error.message}` : 'JSON 解析失败。')
    }
  }

  const save = async () => {
    if (!canWrite) return
    if (!profile.trim()) {
      setSaveError(new Error('档位不能为空。'))
      return
    }
    for (const key of ['max_files', 'max_diff_lines']) {
      const value = draft[key]
      if (value !== undefined && (!Number.isInteger(value) || (value as number) <= 0)) {
        setSaveError(new Error(`${key} 必须是正整数。`))
        return
      }
    }
    setSaving(true)
    setSaveError(null)
    try {
      await updateProject(project.id, { profile: profile.trim(), settings: draft })
      toast.push('success', '项目设置已保存。')
      await queryClient.invalidateQueries({ queryKey: ['project', project.id] })
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
    } catch (error) {
      setSaveError(error)
    } finally {
      setSaving(false)
    }
  }

  const rotate = async () => {
    if (!canWrite) return
    if (!token.trim()) {
      setRotateError(new Error('请输入新的 Project Access Token。'))
      return
    }
    setRotating(true)
    setRotateError(null)
    try {
      await rotateProjectToken(project.id, token.trim())
      setToken('')
      setRotateOpen(false)
      toast.push('success', '项目 token 已轮换。')
      await queryClient.invalidateQueries({ queryKey: ['project', project.id] })
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
    } catch (error) {
      setRotateError(error)
    } finally {
      setRotating(false)
    }
  }

  const rebuildHook = async () => {
    if (!canWrite) return
    setHookPending(true)
    setHookError(null)
    try {
      const response = await recreateWebhook(project.id)
      setHookOpen(false)
      const hookId = response && (typeof response.hook_id === 'number' || typeof response.hook_id === 'string') ? String(response.hook_id) : ''
      toast.push('success', hookId ? `GitLab webhook 已重建（#${hookId}）。` : 'GitLab webhook 已重建。')
      await queryClient.invalidateQueries({ queryKey: ['project', project.id] })
    } catch (error) {
      setHookError(error)
    } finally {
      setHookPending(false)
    }
  }

  const remove = async () => {
    if (!canWrite) return
    setDeletePending(true)
    setDeleteError(null)
    try {
      await unregisterProject(project.id)
      setDeleteOpen(false)
      toast.push('success', '项目已注销。')
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
      navigate('/projects', { replace: true })
    } catch (error) {
      setDeleteError(error)
    } finally {
      setDeletePending(false)
    }
  }

  const toggleEnabled = async (enabled: boolean) => {
    if (!canWrite) return
    setPendingEnabled(true)
    try {
      await updateProject(project.id, { enabled })
      toast.push('success', enabled ? '项目已启用。' : '项目已停用。')
      await queryClient.invalidateQueries({ queryKey: ['project', project.id] })
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
    } catch (error) {
      toast.push('error', `更新启用状态失败：${errorMessage(error)}`)
    } finally {
      setPendingEnabled(false)
    }
  }

  if (!canWrite) {
    return (
      <div className={styles.split}>
        <Panel title="项目设置（只读）" className={styles.mainColumn}>
          <dl className={ui.definitionGrid}>
            <div><dt>档位</dt><dd>{project.profile || '—'}</dd></div>
            <div><dt>启用</dt><dd>{project.enabled ? '启用' : '停用'}</dd></div>
            <div><dt>评审语言</dt><dd>{String(project.settings?.language || '继承平台默认')}</dd></div>
            <div><dt>跳过 Draft</dt><dd>{project.settings?.skip_draft === undefined ? '继承平台默认' : project.settings.skip_draft ? '启用' : '禁用'}</dd></div>
            <div><dt>commit status</dt><dd>{project.settings?.commit_status === undefined ? '继承平台默认' : project.settings.commit_status ? '启用' : '禁用'}</dd></div>
            <div><dt>webhook</dt><dd>{project.hook_id ? '本服务创建' : '手动配置'}</dd></div>
          </dl>
          <div className={styles.notice}>只读账号不能修改项目设置、轮换 token、重建 webhook 或注销项目。</div>
          <details>
            <summary style={{ minHeight: 44, display: 'flex', alignItems: 'center', cursor: 'pointer' }}>查看设置 JSON</summary>
            <pre className="pre" style={jsonStyle}>{JSON.stringify(project.settings || {}, null, 2)}</pre>
          </details>
        </Panel>
      </div>
    )
  }

  return (
    <div className={styles.split}>
      <div className={styles.mainColumn}>
        <Panel
          title="项目设置"
          aside={projectsAside(project)}
        >
          <div className="formGrid">
            <Field label="评审档位" htmlFor={profileId} hint="来自平台配置；此处可改项目默认档位。">
              <input id={profileId} className="input" list="project-profile-options" value={profile} onChange={(event) => setProfile(event.target.value)} />
              <datalist id="project-profile-options">
                {(profiles.data?.profiles || []).map((item) => <option key={item.name} value={item.name} />)}
              </datalist>
            </Field>
            <Field label="评审语言" htmlFor="setting-language" hint="留空时继承平台默认。">
              <select id="setting-language" className="select" value={typeof draft.language === 'string' ? draft.language : ''} onChange={(event) => updateDraft('language', event.target.value)}>
                <option value="">继承平台默认</option>
                <option value="zh-CN">中文</option>
                <option value="en">English</option>
                {typeof draft.language === 'string' && !['zh-CN', 'en'].includes(draft.language) ? <option value={draft.language}>{draft.language}</option> : null}
              </select>
            </Field>
            <BoolSetting label="跳过 Draft MR" value={draft.skip_draft} onChange={(value) => updateDraft('skip_draft', value)} />
            <BoolSetting label="BLOCKING 时写 commit status" value={draft.commit_status} onChange={(value) => updateDraft('commit_status', value)} />
            <BoolSetting label="发布失败时写说明" value={draft.post_failure_note} onChange={(value) => updateDraft('post_failure_note', value)} />
            <BoolSetting label="显示模型署名" value={draft.show_model_attribution} onChange={(value) => updateDraft('show_model_attribution', value)} />
            <BoolSetting label="允许仓库内配置" value={draft.allow_repo_config} onChange={(value) => updateDraft('allow_repo_config', value)} />
            <BoolSetting label="允许仓库内 Skill" value={draft.allow_repo_skills} onChange={(value) => updateDraft('allow_repo_skills', value)} />
            <Field label="目标分支" htmlFor="setting-branches" hint="逗号分隔；留空继承平台默认。">
              <input id="setting-branches" className="input" value={splitComma(draft.target_branches)} onChange={(event) => updateDraft('target_branches', parseComma(event.target.value))} placeholder="main, release/*" />
            </Field>
            <Field label="包含标签" htmlFor="setting-labels-include" hint="逗号分隔。">
              <input id="setting-labels-include" className="input" value={splitComma(draft.labels_include)} onChange={(event) => updateDraft('labels_include', parseComma(event.target.value))} />
            </Field>
            <Field label="排除标签" htmlFor="setting-labels-exclude" hint="带这些标签的 MR 跳过评审。">
              <input id="setting-labels-exclude" className="input" value={splitComma(draft.labels_exclude)} onChange={(event) => updateDraft('labels_exclude', parseComma(event.target.value))} />
            </Field>
            <Field label="max files" htmlFor="setting-max-files" hint="留空使用平台默认。">
              <input id="setting-max-files" className="input" type="number" min={1} value={typeof draft.max_files === 'number' ? draft.max_files : ''} onChange={(event) => updateDraft('max_files', event.target.value ? Number(event.target.value) : undefined)} />
            </Field>
            <Field label="max diff lines" htmlFor="setting-max-lines" hint="留空使用平台默认。">
              <input id="setting-max-lines" className="input" type="number" min={1} value={typeof draft.max_diff_lines === 'number' ? draft.max_diff_lines : ''} onChange={(event) => updateDraft('max_diff_lines', event.target.value ? Number(event.target.value) : undefined)} />
            </Field>
          </div>
          <div className="formGrid">
            <Field label="忽略路径" htmlFor="setting-ignore" hint="每行一个 glob，与平台默认值合并。">
              <textarea id="setting-ignore" className="textarea" rows={5} value={splitLines(draft.ignore)} onChange={(event) => updateDraft('ignore', parseLines(event.target.value))} placeholder="docs/generated/**" />
            </Field>
            <Field label="项目规则" htmlFor="setting-rules" hint="每行一条，作为项目背景注入评审。">
              <textarea id="setting-rules" className="textarea" rows={5} value={splitLines(draft.custom_rules)} onChange={(event) => updateDraft('custom_rules', parseLines(event.target.value))} placeholder="金额计算必须使用 decimal 类型" />
            </Field>
          </div>

          <details>
            <summary style={{ minHeight: 44, display: 'flex', alignItems: 'center', cursor: 'pointer' }}>高级 JSON（profile_by_label 等）</summary>
            <div className={ui.stack} style={{ marginTop: 8 }}>
              <label className="srOnly" htmlFor="settings-json">项目设置 JSON</label>
              <textarea id="settings-json" className="textarea textareaCode" rows={12} value={jsonText} onChange={(event) => setJsonText(event.target.value)} spellCheck={false} />
              {jsonError ? <span className={styles.resultError}>{jsonError}</span> : null}
              <Button variant="secondary" onClick={applyJson}>应用 JSON 到表单</Button>
              <span className="small muted">应用后，保存时提交当前表单值与未在表单中显示的 JSON 字段。</span>
            </div>
          </details>

          {saveError ? <ErrorState error={saveError} title="保存设置失败" /> : null}
          <div className={styles.actionsRight}>
            <Button variant="primary" onClick={() => { void save() }} loading={saving}>保存设置</Button>
          </div>
        </Panel>

        <Panel title="危险操作">
          <div className={ui.stack}>
            <div className={ui.rowBetween}>
              <div className={ui.stack}>
                <strong>注销项目</strong>
                <span className="small muted">删除 webhook（若由本服务创建）并清理工作区；此操作不可撤销。</span>
              </div>
              <Button variant="danger" onClick={() => setDeleteOpen(true)}>注销项目</Button>
            </div>
          </div>
        </Panel>
      </div>

      <div className={styles.sideColumn}>
        <Panel title="凭据与 webhook">
          <div className={ui.stack}>
            <span className="small muted">token 过期时间：{project.token_expires_at ? formatDate(project.token_expires_at) : '未提供'}</span>
            <span className="small muted">token scope：<span className="mono">{project.token_scopes?.join(', ') || '不可用'}</span></span>
            <Button variant="secondary" onClick={() => { setRotateError(null); setToken(''); setRotateOpen(true) }}>轮换 token</Button>
            <Button variant="secondary" onClick={() => { setHookError(null); setHookOpen(true) }}>重建 webhook</Button>
            <span className="small muted">{project.hook_id ? `当前由本服务创建（hook #${project.hook_id}）` : '当前为手动配置或未提供 hook ID'}</span>
          </div>
        </Panel>
        <Panel title="启用状态">
          <div className={ui.rowBetween}>
            <div className={ui.stack}>
              <span>{project.enabled ? '项目已启用' : '项目已停用'}</span>
              <span className="small muted">停用后 webhook 事件被忽略，控制台仍保留历史评审。</span>
            </div>
            <Switch checked={project.enabled} onChange={(next) => { void toggleEnabled(next) }} label="启用项目" disabled={pendingEnabled} />
          </div>
          {project.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="neutral">停用</Badge>}
        </Panel>
      </div>

      <Dialog
        open={rotateOpen}
        onClose={() => { if (!rotating) { setRotateOpen(false); setToken('') } }}
        title="轮换 Project Access Token"
        description="新 token 应具有 api 和 read_repository scope，且属于可访问该项目的 bot 用户。新值不会在界面回显。"
        footer={(
          <>
            <Button variant="secondary" onClick={() => { setRotateOpen(false); setToken('') }} disabled={rotating}>取消</Button>
            <Button variant="primary" onClick={() => { void rotate() }} loading={rotating}>保存新 token</Button>
          </>
        )}
      >
        {rotateError ? <ErrorState error={rotateError} title="轮换失败" /> : null}
        <Field label="新的 Project Access Token" htmlFor="rotate-token">
          <input id="rotate-token" className="input" type="password" value={token} onChange={(event) => setToken(event.target.value)} autoComplete="new-password" />
        </Field>
      </Dialog>

      <ConfirmDialog
        open={hookOpen}
        onClose={() => setHookOpen(false)}
        onConfirm={() => { void rebuildHook() }}
        title="重建 GitLab webhook？"
        description="服务端会重新创建或更新该项目的 GitLab webhook。旧 hook 可能被删除，请确认当前部署地址和权限有效。"
        confirmLabel="重建 webhook"
        danger={false}
        pending={hookPending}
        error={hookError}
      />

      <ConfirmDialog
        open={deleteOpen}
        onClose={() => setDeleteOpen(false)}
        onConfirm={() => { void remove() }}
        title="注销项目？"
        description={`将删除 ${project.path_with_namespace} 的注册记录、webhook（若由本服务创建）和工作区数据。历史评审关联数据也将受影响。此操作不可撤销。`}
        confirmLabel="注销项目"
        pending={deletePending}
        error={deleteError}
      />
    </div>
  )
}

function projectsAside(project: Project) {
  return <span>项目 ID {project.id} · 最后更新 {project.updated_at ? formatDate(project.updated_at) : '—'}</span>
}

const jsonStyle = {
  background: 'var(--surface-2)',
  border: '1px solid var(--border)',
  borderRadius: 8,
  padding: 12,
  maxHeight: 360,
  overflow: 'auto',
  color: 'var(--text)',
} as const
