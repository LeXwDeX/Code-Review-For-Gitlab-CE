import { useRef, useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Button, Dialog, ErrorState, Field } from '../src/components/ui'
import { ApiError } from '../src/api/client'

function DialogHarness({ onClose }: { onClose: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button ref={undefined} onClick={() => setOpen(true)}>打开详情</Button>
      <Dialog
        open={open}
        onClose={() => { setOpen(false); onClose() }}
        title="节点详情"
        footer={(
          <>
            <Button variant="secondary">取消</Button>
            <Button variant="primary">确认</Button>
          </>
        )}
      >
        <p>正文</p>
      </Dialog>
    </>
  )
}

describe('dialogs and form states', () => {
  it('traps focus, closes on Escape and restores focus to the trigger', async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    render(<DialogHarness onClose={onClose} />)
    const trigger = screen.getByRole('button', { name: '打开详情' })
    await user.click(trigger)
    const dialog = screen.getByRole('dialog', { name: '节点详情' })
    expect(dialog).toBeInTheDocument()
    const close = screen.getByRole('button', { name: '关闭对话框' })
    expect(close).toHaveFocus()

    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(screen.getByRole('button', { name: '确认' })).toHaveFocus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(close).toHaveFocus()

    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(onClose).toHaveBeenCalled()
    await waitFor(() => expect(trigger).toHaveFocus())
  })

  it('renders labelled fields with an error alert', () => {
    render(
      <Field label="项目路径" htmlFor="project" error="必须填写项目路径">
        <input id="project" aria-invalid="true" />
      </Field>,
    )
    expect(screen.getByLabelText('项目路径')).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('必须填写项目路径')
  })

  it('renders API errors without substituting a success state', () => {
    render(<ErrorState error={new ApiError(500, 'internal', '存储不可用')} />)
    expect(screen.getByRole('alert')).toHaveTextContent('存储不可用')
    expect(screen.queryByText(/成功/)).not.toBeInTheDocument()
  })
})
