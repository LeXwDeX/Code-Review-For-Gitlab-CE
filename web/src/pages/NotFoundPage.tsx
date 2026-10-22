import { Link } from 'react-router-dom'
import { ErrorState } from '../components/ui'
import { ApiError } from '../api/client'

export function NotFoundPage() {
  return (
    <main style={{ maxWidth: 720, margin: '0 auto', padding: 32 }}>
      <ErrorState error={new ApiError(404, 'not_found', '这个页面不存在。')} title="页面不存在" />
      <div style={{ marginTop: 16 }}>
        <Link to="/" style={linkStyle}>返回总览</Link>
      </div>
    </main>
  )
}

const linkStyle = { display: 'inline-flex', alignItems: 'center', minHeight: 44, padding: '0 16px', borderRadius: 8, border: '1px solid var(--control-border)', background: 'var(--surface)', textDecoration: 'none' } as const
