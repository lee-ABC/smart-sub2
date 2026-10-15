import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import type { BpsLoginJob, BpsLoginStartRequest } from '@/api/admin/accounts'

const api = vi.hoisted(() => ({ startBpsLogin: vi.fn(), getBpsLogin: vi.fn(), submitBpsLoginInput: vi.fn(), cancelBpsLogin: vi.fn(), importBpsLogin: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
import { useBpsBatchLogin } from '../useBpsBatchLogin'

const secret = 'JBSWY3DPEHPK3PXP'
const line = (i: number) => `person${i}@example.test----private-password-${i}----${secret}`
const job = (i: number, status: BpsLoginJob['status'] = 'running', extra: Partial<BpsLoginJob> = {}): BpsLoginJob => ({
  id: `job-${i}`, status, phase: 'session', expires_at: Date.now() + 600_000,
  ...(status === 'ready' ? { preview: { email: `person${i}@example.test`, workspace_id: 'workspace-a', plan_type: 'team', expires_at: Date.now() + 3600000 } } : {}), ...extra,
})
const result = { created: 1, updated: 0, skipped: 0, failed: 0, items: [], errors: [], warnings: [] }
const deferred = <T,>() => { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r }); return { resolve, promise } }
const stores: ReturnType<typeof useBpsBatchLogin>[] = []
const make = (overrides: Partial<Parameters<typeof useBpsBatchLogin>[0]> = {}) => {
  const store = useBpsBatchLogin({ proxyId: () => 7, getImportOptions: () => ({ group_ids: [2], concurrency: 10 }), ...overrides }); stores.push(store); return store
}
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
  api.startBpsLogin.mockReset().mockImplementation((r: BpsLoginStartRequest) => Promise.resolve(job(Number(r.email.match(/person(\d+)/)![1]))))
  api.getBpsLogin.mockReset().mockImplementation((id: string) => Promise.resolve(job(Number(id.slice(4)))))
  api.submitBpsLoginInput.mockReset().mockResolvedValue(job(1, 'ready'))
  api.cancelBpsLogin.mockReset().mockResolvedValue(undefined)
  api.importBpsLogin.mockReset().mockResolvedValue(result)
})
afterEach(() => { stores.forEach(store => store.dispose()); stores.length = 0; vi.useRealTimers() })

describe('BPS batch lifecycle', () => {
  it('limits starting/running/waiting jobs to five and advances the queue as one succeeds', async () => {
    const store = make(); store.start(Array.from({ length: 8 }, (_, i) => line(i + 1)).join('\n'), 'team_first', '')
    expect(api.startBpsLogin).toHaveBeenCalledTimes(5)
    await flushPromises(); expect(store.activeCount.value).toBe(5)
    api.getBpsLogin.mockImplementation((id: string) => Promise.resolve(job(Number(id.slice(4)), id === 'job-1' ? 'ready' : 'awaiting_input', id === 'job-1' ? {} : { challenge_kind: 'email_otp' })))
    await vi.advanceTimersByTimeAsync(1500); await flushPromises()
    expect(api.startBpsLogin).toHaveBeenCalledTimes(6); expect(store.activeCount.value).toBe(5); expect(store.readyRows.value).toHaveLength(1)
    expect(api.importBpsLogin).not.toHaveBeenCalled()
    expect(JSON.stringify(store.rows.value)).not.toContain('private-password')
    expect(JSON.stringify(store.rows.value)).not.toContain(secret)
  })
  it('clears queued secrets and cancels all known tasks including a late create ACK after reset', async () => {
    const late = deferred<BpsLoginJob>(); api.startBpsLogin.mockReturnValue(late.promise)
    const store = make(); store.start(Array.from({ length: 7 }, (_, i) => line(i + 1)).join('\n'), 'team_first', '')
    store.reset(); late.resolve(job(1)); await flushPromises(); await vi.advanceTimersByTimeAsync(10000)
    expect(store.rows.value).toHaveLength(0); expect(api.cancelBpsLogin).toHaveBeenCalledWith('job-1')
    expect(api.startBpsLogin).toHaveBeenCalledTimes(5); expect(api.getBpsLogin).not.toHaveBeenCalled()
  })
  it('only imports ready successful rows once and freezes group settings across the batch', async () => {
    api.startBpsLogin.mockImplementation((r: BpsLoginStartRequest) => Promise.resolve(job(Number(r.email.match(/person(\d+)/)![1]), r.email.startsWith('person3') ? 'failed' : 'ready')))
    const settings = { group_ids: [2], concurrency: 10 }
    const imported = vi.fn(); const store = make({ getImportOptions: () => settings, onImported: imported })
    store.start([line(1), line(2), line(3)].join('\n'), 'team_first', ''); await flushPromises()
    const first = deferred<typeof result>(); api.importBpsLogin.mockImplementationOnce(() => first.promise)
    const pending = store.importReady(); await store.importReady(); settings.group_ids = [99]
    first.resolve(result); await pending; await store.importReady()
    expect(api.importBpsLogin).toHaveBeenCalledTimes(2)
    expect(api.importBpsLogin.mock.calls.map(c => c[0])).toEqual(['job-1', 'job-2'])
    expect(api.importBpsLogin.mock.calls.map(c => c[1].group_ids)).toEqual([[2], [2]])
    expect(api.importBpsLogin.mock.calls[0][1].name).toBe('person1@example.test')
    expect(store.rows.value.map(row => row.importState)).toEqual(['imported', 'imported', 'none'])
    expect(imported).toHaveBeenCalledTimes(1)
  })
  it('keeps unknown import outcomes out of subsequent one-click attempts without relogging', async () => {
    api.startBpsLogin.mockResolvedValue(job(1, 'ready')); api.importBpsLogin.mockRejectedValue(new Error('private-upstream-token'))
    const store = make(); store.start(line(1), 'team_first', ''); await flushPromises(); await store.importReady(); await store.importReady()
    expect(store.rows.value[0].importState).toBe('unknown'); expect(api.importBpsLogin).toHaveBeenCalledTimes(1)
    expect(api.startBpsLogin).toHaveBeenCalledTimes(1); expect(JSON.stringify(store.rows.value)).not.toContain('private-upstream-token')
  })
  it('cancels no in-flight import, stops remaining imports on disposal, and ignores late replies', async () => {
    api.startBpsLogin.mockImplementation((r: BpsLoginStartRequest) => Promise.resolve(job(Number(r.email.match(/person(\d+)/)![1]), 'ready')))
    const store = make(); store.start([line(1), line(2)].join('\n'), 'team_first', ''); await flushPromises()
    const imported = deferred<typeof result>(); api.importBpsLogin.mockReturnValue(imported.promise)
    const promise = store.importReady(); store.dispose()
    expect(api.cancelBpsLogin).not.toHaveBeenCalledWith('job-1'); expect(api.cancelBpsLogin).toHaveBeenCalledWith('job-2')
    imported.resolve(result); await promise
    expect(api.importBpsLogin).toHaveBeenCalledTimes(1); expect(store.rows.value).toHaveLength(0)
  })
  it('handles per-row challenge input and retires an older in-flight poll', async () => {
    api.startBpsLogin.mockResolvedValue(job(1, 'awaiting_input', { challenge_kind: 'email_otp' }))
    const old = deferred<BpsLoginJob>(); api.getBpsLogin.mockReturnValue(old.promise)
    const store = make(); store.start(line(1), 'team_first', ''); await flushPromises(); await vi.advanceTimersByTimeAsync(1500)
    const row = store.rows.value[0]; row.challenge = '654321'; await store.submitInput(row)
    old.resolve(job(1, 'awaiting_input', { challenge_kind: 'email_otp' })); await flushPromises()
    expect(row.state).toBe('ready'); expect(row.challenge).toBe('')
    expect(api.submitBpsLoginInput).toHaveBeenCalledWith('job-1', { kind: 'email_otp', value: '654321' }, expect.any(AbortSignal))
    expect(api.startBpsLogin).toHaveBeenCalledTimes(1)
  })
  it('does not import expired results or a different email and never keeps unknown token fields', async () => {
    api.startBpsLogin.mockResolvedValue({ ...job(1, 'ready'), access_token: 'private-token' })
    const store = make(); store.start(line(1), 'team_first', ''); await flushPromises()
    expect(JSON.stringify(store.rows.value)).not.toContain('private-token')
    await vi.advanceTimersByTimeAsync(601000); await store.importReady()
    expect(store.rows.value[0].state).toBe('expired'); expect(api.importBpsLogin).not.toHaveBeenCalled()
    store.reset(); api.startBpsLogin.mockResolvedValue(job(2, 'ready')); store.start(line(1), 'team_first', ''); await flushPromises()
    expect(store.rows.value[0].state).toBe('failed'); expect(store.readyRows.value).toHaveLength(0)
  })
  it('imports ready rows while the queue stays alive and later imports only newly successful rows', async () => {
    api.startBpsLogin.mockImplementation((r: BpsLoginStartRequest) => Promise.resolve(job(Number(r.email.match(/person(\d+)/)![1]), r.email.startsWith('person1@') ? 'ready' : 'running')))
    const imported = vi.fn(); const store = make({ onImported: imported })
    store.start([line(1), line(2)].join('\n'), 'team_first', ''); await flushPromises()
    await store.importReady()
    expect(store.pending.value).toBe(true); expect(api.cancelBpsLogin).not.toHaveBeenCalled()
    api.getBpsLogin.mockResolvedValue(job(2, 'ready'))
    await vi.advanceTimersByTimeAsync(1500); await store.importReady()
    expect(api.importBpsLogin.mock.calls.map(c => c[0])).toEqual(['job-1', 'job-2'])
    expect(imported).toHaveBeenCalledTimes(2)
  })
  it('keeps transient GET failures on the same task, rejects invalid workspace choices, and continues the valid choice', async () => {
    api.startBpsLogin.mockResolvedValue(job(1, 'awaiting_input', { challenge_kind: 'workspace', workspace_choices: [{ id: 'workspace-a', name: 'Test workspace', kind: 'team' }] }))
    api.getBpsLogin.mockRejectedValue({ response: { status: 502, data: { message: 'private-upstream-token' } } })
    const store = make(); store.start(line(1), 'team_first', ''); await flushPromises()
    await vi.advanceTimersByTimeAsync(1500)
    const row = store.rows.value[0]; row.challenge = 'wrong-workspace'; await store.submitInput(row)
    expect(api.submitBpsLoginInput).not.toHaveBeenCalled()
    row.challenge = 'workspace-a'; await store.submitInput(row)
    expect(api.submitBpsLoginInput).toHaveBeenCalledWith('job-1', { kind: 'workspace', value: 'workspace-a' }, expect.any(AbortSignal))
    expect(api.startBpsLogin).toHaveBeenCalledTimes(1)
    expect(JSON.stringify(store.rows.value)).not.toContain('private-upstream-token')
  })
  it('cancels a malformed acknowledged job instead of retaining an orphan and never imports it', async () => {
    api.startBpsLogin.mockResolvedValue(job(1, 'ready', { expires_at: 'invalid-date' }))
    const store = make(); store.start(line(1), 'team_first', ''); await flushPromises(); await store.importReady()
    expect(api.cancelBpsLogin).toHaveBeenCalledWith('job-1')
    expect(api.importBpsLogin).not.toHaveBeenCalled()
  })

})
