import { useEffect, useId, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createApiToken, getSettings, listApiTokens, revokeApiToken } from '../api/api'
import type { ApiToken, CreatedApiToken } from '../api/types'
import { useCanWrite } from '../components/RoleContext'
import {
  Button, ConfirmDialog, Dialog, EmptyState, ErrorState, Field, LoadingState, PageHeader, Panel, SecretField, Tabs, useToast,
} from '../components/ui'
import { usePaginated } from '../lib/hooks'
import { formatDate } from '../lib/format'
import ui from '../styles/ui.module.css'
import styles from './pages.module.css'

function tokenLabel(token: ApiToken): string {
  return token.name || `token ${token.id}`
}

function TokenCreateDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const nameId = useId()
  const expiresId = useId()
  const [name, setName] = useState('')
  const [days, setDays] = useState('90')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [created, setCreated] = useState<CreatedApiToken | null>(null)
  const [fieldError, setFieldError] = useState('')

  useEffect(() => {
    if (!open) {
      setName('')
      setDays('90')
      setPending(false)
      setError(null)
      setCreated(null)
      setFieldError('')
    }
  }, [open])

  const submit = async () => {
    const parsedDays = Number(days)
    if (!name.trim()) { setFieldError('请输入 token 名称。'); return }
    if (!Number.isInteger(parsedDays) || parsedDays <= 0) { setFieldError('有效天数必须是正整数。'); return }
    setFieldError('')
    setPending(true)
    setError(null)
    try {
      const result = await createApiToken({ name: name.trim(), expires_in_days: parsedDays })
      setCreated(result)
      toast.push('success', 'API token 已创建，请立即复制。')
      await queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
    } catch (caught) {
      setError(caught)
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={created ? '复制新 API token' : '创建 API token'}
      description={created
        ? '明文 token 只显示这一次。关闭对话框后将从界面状态中清除。'
        : 'API token 用于 reviewctl 或脚本访问管理 API。请设置名称和有效期。'}
      footer={created ? (
        <Button variant="primary" onClick={onClose}>我已复制，关闭</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={onClose} disabled={pending}>取消</Button>
          <Button variant="primary" onClick={() => { void submit() }} loading={pending}>创建</Button>
        </>
      )}
    >
      {created ? (
        <>
          <SecretField label="API token" value={created.token} />
          <div className={styles.notice}>关闭后不会再次显示明文；如遗失，请撤销并创建新 token。</div>
        </>
      ) : (
        <>
          {error ? <ErrorState error={error} title="创建失败" /> : null}
          <div className="formGrid">
            <Field label="名称" htmlFor={nameId} error={fieldError}>
              <input id={nameId} className="input" value={name} onChange={(event) => setName(event.target.value)} placeholder="例如 ci-reviewctl" aria-invalid={Boolean(fieldError)} />
            </Field>
            <Field label="有效天数" htmlFor={expiresId} hint="正整数。">
              <input id={expiresId} className="input" type="number" min={1} value={days} onChange={(event) => setDays(event.target.value)} />
            </Field>
          </div>
        </>
      )}
    </Dialog>
  )
}

export function SystemPage() {
  const canWrite = useCanWrite()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [tab, setTab] = useState('tokens')
  const [createOpen, setCreateOpen] = useState(false)
  const [revokeTarget, setRevokeTarget] = useState<ApiToken | null>(null)
  const [revokePending, setRevokePending] = useState(false)
  const [revokeError, setRevokeError] = useState<unknown>(null)

  const tokens = usePaginated<ApiToken>(
    ['api-tokens'],
    (cursor) => listApiTokens(cursor),
    'tokens',
  )
  const settings = useQuery({
    queryKey: ['settings'],
    queryFn: getSettings,
    enabled: tab === 'settings',
  })

  const revoke = async () => {
    if (!revokeTarget) return
    setRevokePending(true)
    setRevokeError(null)
    try {
      await revokeApiToken(revokeTarget.id)
      setRevokeTarget(null)
      toast.push('success', 'API token 已撤销。')
      await queryClient.invalidateQueries({ queryKey: ['api-tokens'] })
    } catch (error) {
      setRevokeError(error)
    } finally {
      setRevokePending(false)
    }
  }

  return (
    <main className={ui.page}>
      <div className={ui.pageStack}>
        <PageHeader
          title="系统"
          description="管理当前用户的 API token，查看平台配置的脱敏只读视图。"
        />
        <Tabs
          items={[{ key: 'tokens', label: 'API Token' }, { key: 'settings', label: '平台配置（只读）' }]}
          value={tab}
          onChange={setTab}
          label="系统设置"
        />

        {tab === 'tokens' ? (
          <Panel
            title="我的 API token"
            aside={canWrite ? 'cookie 写操作由 CSRF 保护' : '只读账号可查看 token 列表，但不能创建或撤销'}
          >
            {canWrite ? (
              <div className={styles.actionsRight}>
                <Button variant="primary" onClick={() => setCreateOpen(true)}>创建 token</Button>
              </div>
            ) : null}
            {tokens.isPending ? <LoadingState /> : null}
            {tokens.isError ? <ErrorState error={tokens.error} onRetry={tokens.refetch} /> : null}
            {tokens.isSuccess && tokens.items.length === 0 ? <EmptyState title="还没有 API token" description={canWrite ? '创建一个 token 供 reviewctl 或脚本使用。' : '当前账号没有 API token。'} /> : null}
            {tokens.items.length ? (
              <div className="tableWrap">
                <table className="dataTable" style={{ minWidth: 760 }}>
                  <thead>
                    <tr>
                      <th scope="col">名称</th>
                      <th scope="col">创建时间</th>
                      <th scope="col">过期时间</th>
                      <th scope="col">最近使用</th>
                      {canWrite ? <th scope="col">操作</th> : null}
                    </tr>
                  </thead>
                  <tbody>
                    {tokens.items.map((token) => (
                      <tr key={token.id}>
                        <td>{tokenLabel(token)}</td>
                        <td>{token.created_at ? formatDate(token.created_at) : '—'}</td>
                        <td>{token.expires_at ? formatDate(token.expires_at) : '—'}</td>
                        <td>{token.last_used_at ? formatDate(token.last_used_at) : '从未使用或未提供'}</td>
                        {canWrite ? <td><Button variant="danger" onClick={() => setRevokeTarget(token)}>撤销</Button></td> : null}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}
            {tokens.hasNextPage ? <Button variant="secondary" onClick={tokens.fetchNextPage} loading={tokens.isFetchingNextPage}>加载更多</Button> : null}
          </Panel>
        ) : (
          <Panel
            title="平台配置"
            aside="服务端脱敏只读视图"
          >
            {settings.isPending ? <LoadingState label="正在读取平台配置…" /> : null}
            {settings.isError ? <ErrorState error={settings.error} onRetry={() => { void settings.refetch() }} /> : null}
            {settings.isSuccess ? (
              <>
                <div className={styles.notice}>此页面不提供编辑；密钥和凭据由服务端脱敏，界面不显示其值。</div>
                {Object.keys(settings.data || {}).length ? (
                  <pre className="pre" style={settingsStyle}>{JSON.stringify(settings.data, null, 2)}</pre>
                ) : <EmptyState title="平台配置为空" description="服务端返回了空对象；这不代表配置缺失或加载成功。" />}
              </>
            ) : null}
          </Panel>
        )}
      </div>

      {canWrite ? <TokenCreateDialog open={createOpen} onClose={() => setCreateOpen(false)} /> : null}
      <ConfirmDialog
        open={Boolean(revokeTarget)}
        onClose={() => setRevokeTarget(null)}
        onConfirm={() => { void revoke() }}
        title="撤销 API token？"
        description={`撤销后，使用 ${revokeTarget ? tokenLabel(revokeTarget) : '该 token'} 的脚本将立即失去访问权限。`}
        confirmLabel="撤销"
        pending={revokePending}
        error={revokeError}
      />
    </main>
  )
}

const settingsStyle = {
  background: 'var(--surface-2)',
  border: '1px solid var(--border)',
  borderRadius: 8,
  padding: 12,
  color: 'var(--text)',
  maxHeight: 520,
  overflow: 'auto',
} as const
