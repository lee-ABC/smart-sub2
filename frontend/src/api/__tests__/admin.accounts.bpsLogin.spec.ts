import { beforeEach, describe, expect, it, vi } from 'vitest'
const { get, post, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), del: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, post, delete: del } }))
import { startBpsLogin, getBpsLogin, submitBpsLoginInput, cancelBpsLogin, importBpsLogin } from '@/api/admin/accounts'
const job = { id: 'job-1', status: 'running', phase: 'session', expires_at: '2026-10-15T15:30:00Z' }
beforeEach(() => { get.mockReset().mockResolvedValue({ data: job }); post.mockReset().mockResolvedValue({ data: job }); del.mockReset().mockResolvedValue({}) })
describe('BPS login admin API contract', () => {
  it('uses the login-only endpoint for password/TOTP and returns the sanitized job', async () => {
    const request = { email: 'operator@example.test', password: 'test-password', totp_secret: 'JBSWY3DPEHPK3PXP', proxy_id: 9, workspace_preference: 'team_first' as const }
    await expect(startBpsLogin(request)).resolves.toEqual(job)
    expect(post).toHaveBeenCalledWith('/admin/accounts/bps-login', request)
  })
  it('encodes job IDs and carries cancellation signals through polling and challenge input', async () => {
    const controller = new AbortController()
    await getBpsLogin('job/1', controller.signal)
    await submitBpsLoginInput('job/1', { kind: 'email_otp', value: '123456' }, controller.signal)
    await cancelBpsLogin('job/1')
    expect(get).toHaveBeenCalledWith('/admin/accounts/bps-login/job%2F1', { signal: controller.signal })
    expect(post).toHaveBeenCalledWith('/admin/accounts/bps-login/job%2F1/input', { kind: 'email_otp', value: '123456' }, { signal: controller.signal })
    expect(del).toHaveBeenCalledWith('/admin/accounts/bps-login/job%2F1')
  })
  it('confirms import using job ID plus first-step options, not credential contents', async () => {
    const result = { created: 0, updated: 1, skipped: 0, failed: 0, errors: [], warnings: [] }
    post.mockResolvedValue({ data: result })
    const options = { name: 'BPS profile', group_ids: [2], proxy_id: 9, update_existing: true }
    await expect(importBpsLogin('job-1', options)).resolves.toEqual(result)
    expect(post).toHaveBeenCalledWith('/admin/accounts/bps-login/job-1/import', options, { timeout: 120000 })
    expect(post.mock.calls[0][1]).not.toHaveProperty('content')
  })
})
