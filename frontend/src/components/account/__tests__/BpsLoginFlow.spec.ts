import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BpsLoginJob, BpsLoginStartRequest } from '@/api/admin/accounts'

const api = vi.hoisted(() => ({
  startBpsLogin: vi.fn(), getBpsLogin: vi.fn(), submitBpsLoginInput: vi.fn(), cancelBpsLogin: vi.fn(),
}))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
import BpsLoginFlow from '../BpsLoginFlow.vue'

const wrappers: VueWrapper[] = []
const task = (overrides: Partial<BpsLoginJob> = {}): BpsLoginJob => ({
  id: 'test-job', status: 'running', phase: 'session', expires_at: Date.now() + 600_000, ...overrides,
})
const readyTask = () => task({ status: 'ready', phase: 'validate', preview: {
  email: 'operator@example.test', workspace_id: 'workspace-1', plan_type: 'team', expires_at: Date.now() + 60_000,
} })
const mountFlow = (props: Record<string, unknown> = {}) => {
  const wrapper = mount(BpsLoginFlow, { props })
  wrappers.push(wrapper)
  return wrapper
}
const fill = async (wrapper: VueWrapper) => {
  await wrapper.get('[data-testid="bps-email"]').setValue('operator@example.test')
  await wrapper.get('[data-testid="bps-password"]').setValue('private-password')
  await wrapper.get('[data-testid="bps-totp-secret"]').setValue('JBSWY3DPEHPK3PXP')
}
const start = async (wrapper: VueWrapper) => {
  await fill(wrapper)
  await wrapper.get('form').trigger('submit')
  await flushPromises()
}
const pending = <T,>() => {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
  api.startBpsLogin.mockReset().mockResolvedValue(task())
  api.getBpsLogin.mockReset().mockResolvedValue(task())
  api.submitBpsLoginInput.mockReset().mockResolvedValue(task())
  api.cancelBpsLogin.mockReset().mockResolvedValue(undefined)
})
afterEach(() => {
  wrappers.forEach((wrapper) => wrapper.unmount())
  wrappers.length = 0
  vi.useRealTimers()
})

describe('BPS server-side login lifecycle', () => {
  it('submits email/password/Base32 secret and selected proxy, then clears secret fields before ACK', async () => {
    const ack = pending<BpsLoginJob>()
    let sent: BpsLoginStartRequest | undefined
    api.startBpsLogin.mockImplementation((value: BpsLoginStartRequest) => { sent = { ...value }; return ack.promise })
    const wrapper = mountFlow({ proxyId: 17 })
    await fill(wrapper)
    await wrapper.get('[data-testid="bps-workspace-id"]').setValue('workspace-1')
    await wrapper.get('form').trigger('submit')
    expect(sent).toEqual({ email: 'operator@example.test', password: 'private-password', totp_secret: 'JBSWY3DPEHPK3PXP', proxy_id: 17, workspace_id: 'workspace-1', workspace_preference: 'team_first' })
    expect(wrapper.get<HTMLInputElement>('[data-testid="bps-password"]').element.value).toBe('')
    expect(wrapper.get<HTMLInputElement>('[data-testid="bps-totp-secret"]').element.value).toBe('')
    ack.resolve(task())
    await flushPromises()
    expect(wrapper.text()).not.toContain('private-password')
  })

  it('rejects a six-digit rotating code instead of accepting it as a Base32 secret', async () => {
    const wrapper = mountFlow()
    await fill(wrapper)
    await wrapper.get('[data-testid="bps-totp-secret"]').setValue('222222')
    await wrapper.get('form').trigger('submit')
    expect(api.startBpsLogin).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('bpsLoginInvalidCredentials')
  })

  it('shows a ready preview but never imports until explicit confirmation', async () => {
    api.getBpsLogin.mockResolvedValue(readyTask())
    const wrapper = mountFlow()
    await start(wrapper)
    await vi.advanceTimersByTimeAsync(1500)
    expect(wrapper.get('[data-testid="bps-preview"]').text()).toContain('operator@example.test')
    expect(wrapper.emitted('confirm')).toBeUndefined()
    await wrapper.get('[data-testid="bps-confirm-import"]').trigger('click')
    expect(wrapper.emitted('confirm')).toEqual([['test-job']])
    expect(wrapper.text()).not.toContain('access_token')
  })

  it('continues the same task with email OTP without resending password or secret', async () => {
    api.startBpsLogin.mockResolvedValue(task({ status: 'awaiting_input', challenge_kind: 'email_otp', phase: 'email_otp' }))
    const wrapper = mountFlow()
    await start(wrapper)
    await wrapper.get('[data-testid="bps-email-otp"]').setValue('654321')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api.submitBpsLoginInput).toHaveBeenCalledWith('test-job', { kind: 'email_otp', value: '654321' }, expect.any(AbortSignal))
    expect(api.startBpsLogin).toHaveBeenCalledTimes(1)
  })

  it('submits only a workspace offered by the task', async () => {
    api.startBpsLogin.mockResolvedValue(task({ status: 'awaiting_input', challenge_kind: 'workspace', workspace_choices: [{ id: 'team-1', name: 'Team one', kind: 'team' }] }))
    const wrapper = mountFlow()
    await start(wrapper)
    await wrapper.get('[data-testid="bps-workspace-choice"]').setValue('team-1')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api.submitBpsLoginInput).toHaveBeenCalledWith('test-job', { kind: 'workspace', value: 'team-1' }, expect.any(AbortSignal))
  })

  it('cancels its task and stops future polls on reset', async () => {
    const wrapper = mountFlow()
    await start(wrapper)
    await wrapper.get('[data-testid="bps-cancel"]').trigger('click')
    await vi.advanceTimersByTimeAsync(10000)
    expect(api.cancelBpsLogin).toHaveBeenCalledWith('test-job')
    expect(api.getBpsLogin).not.toHaveBeenCalled()
    expect(wrapper.get<HTMLInputElement>('[data-testid="bps-password"]').element.value).toBe('')
    expect(wrapper.get<HTMLInputElement>('[data-testid="bps-totp-secret"]').element.value).toBe('')
  })

  it('cancels a late create ACK after unmount rather than retaining an orphaned task', async () => {
    const ack = pending<BpsLoginJob>()
    api.startBpsLogin.mockReturnValue(ack.promise)
    const wrapper = mountFlow()
    await start(wrapper)
    wrapper.unmount()
    ack.resolve(task({ id: 'late-job' }))
    await flushPromises()
    expect(api.cancelBpsLogin).toHaveBeenCalledWith('late-job')
    expect(api.getBpsLogin).not.toHaveBeenCalled()
  })

  it('aborts an old poll and ignores its late response after a reset', async () => {
    const result = pending<BpsLoginJob>()
    api.getBpsLogin.mockReturnValue(result.promise)
    const wrapper = mountFlow()
    await start(wrapper)
    await vi.advanceTimersByTimeAsync(1500)
    const signal = api.getBpsLogin.mock.calls[0][1] as AbortSignal
    await wrapper.get('[data-testid="bps-cancel"]').trigger('click')
    expect(signal.aborted).toBe(true)
    result.resolve(readyTask())
    await flushPromises()
    expect(wrapper.find('[data-testid="bps-preview"]').exists()).toBe(false)
    expect(wrapper.emitted('confirm')).toBeUndefined()
  })

  it('does not let an older poll replace the state returned by challenge submission', async () => {
    const oldPoll = pending<BpsLoginJob>()
    const waiting = task({ status: 'awaiting_input', challenge_kind: 'email_otp', phase: 'email_otp' })
    api.startBpsLogin.mockResolvedValue(waiting)
    api.getBpsLogin.mockReturnValue(oldPoll.promise)
    api.submitBpsLoginInput.mockResolvedValue(readyTask())
    const wrapper = mountFlow()
    await start(wrapper)
    await vi.advanceTimersByTimeAsync(1500)
    await wrapper.get('[data-testid="bps-email-otp"]').setValue('654321')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    oldPoll.resolve(waiting)
    await flushPromises()
    expect(wrapper.find('[data-testid="bps-preview"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="bps-email-otp"]').exists()).toBe(false)
  })

  it('does not cancel a task while explicitly confirmed import is in flight', async () => {
    api.startBpsLogin.mockResolvedValue(readyTask())
    const wrapper = mountFlow()
    await start(wrapper)
    await wrapper.setProps({ importing: true })
    wrapper.unmount()
    expect(api.cancelBpsLogin).not.toHaveBeenCalled()
  })

  it('retires the task if the selected proxy changes', async () => {
    const wrapper = mountFlow({ proxyId: 1 })
    await start(wrapper)
    await wrapper.setProps({ proxyId: 2 })
    expect(api.cancelBpsLogin).toHaveBeenCalledWith('test-job')
    expect(wrapper.find('[data-testid="bps-job-status"]').exists()).toBe(false)
  })

  it('shows fixed progress/error codes, never raw upstream errors or unknown phases', async () => {
    api.startBpsLogin.mockResolvedValue(task({ phase: 'private-upstream-detail' }))
    api.getBpsLogin.mockRejectedValue({ response: { data: { code: 'PURE_PASSWORD_INVALID', message: 'private-password', detail: 'private-token' } } })
    const wrapper = mountFlow()
    await start(wrapper)
    expect(wrapper.text()).not.toContain('private-upstream-detail')
    await vi.advanceTimersByTimeAsync(1500)
    expect(wrapper.get('[role="alert"]').text()).toContain('PURE_PASSWORD_INVALID')
    expect(wrapper.text()).not.toContain('private-password')
    expect(wrapper.text()).not.toContain('private-token')
    expect(api.startBpsLogin).toHaveBeenCalledTimes(1)
  })

  it('ends polling at the server-provided expiry and cancels the expired task', async () => {
    api.startBpsLogin.mockResolvedValue(task({ expires_at: Date.now() + 1000 }))
    const wrapper = mountFlow()
    await start(wrapper)
    await vi.advanceTimersByTimeAsync(1500)
    expect(api.getBpsLogin).not.toHaveBeenCalled()
    expect(api.cancelBpsLogin).toHaveBeenCalledWith('test-job')
    expect(wrapper.get('[role="alert"]').text()).toContain('LOGIN_TIMEOUT')
    await vi.advanceTimersByTimeAsync(10000)
    expect(api.getBpsLogin).not.toHaveBeenCalled()
  })

  it('shows sanitized parent import errors without discarding the ready preview', async () => {
    api.startBpsLogin.mockResolvedValue(readyTask())
    const wrapper = mountFlow()
    await start(wrapper)
    await wrapper.setProps({ importError: 'BPS_IMPORT_FAILED' })
    expect(wrapper.get('[role="alert"]').text()).toContain('BPS_IMPORT_FAILED')
    expect(wrapper.find('[data-testid="bps-preview"]').exists()).toBe(true)
  })
})
