import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BpsLoginStartRequest } from '@/api/admin/accounts'
const api = vi.hoisted(() => ({ startBpsLogin: vi.fn(), getBpsLogin: vi.fn(), submitBpsLoginInput: vi.fn(), cancelBpsLogin: vi.fn(), importBpsLogin: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: api } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
import BpsBatchLoginFlow from '../BpsBatchLoginFlow.vue'
let wrapper: ReturnType<typeof mount>
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
  api.startBpsLogin.mockReset().mockImplementation((req: BpsLoginStartRequest) => Promise.resolve({ id: req.email, status: req.email.startsWith('bad') ? 'failed' : 'ready', phase: 'validate', expires_at: Date.now() + 600000, preview: { email: req.email, workspace_id: 'workspace-a', plan_type: 'team', expires_at: Date.now() + 3600000 } }))
  api.cancelBpsLogin.mockReset().mockResolvedValue(undefined)
  api.importBpsLogin.mockReset().mockResolvedValue({ created: 1, updated: 0, skipped: 0, failed: 0 })
})
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })
describe('BPS batch UI', () => {
  it('clears the pasted credentials, filters failures, and explicitly imports successes', async () => {
    wrapper = mount(BpsBatchLoginFlow, { props: { getImportOptions: () => ({ group_ids: [2] }) } })
    await wrapper.get('[data-testid="bps-batch-input"]').setValue('good@example.test----private-pass----JBSWY3DPEHPK3PXP\nbad@example.test----other-pass----JBSWY3DPEHPK3PXP')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('private-pass'); expect(wrapper.text()).not.toContain('JBSWY3DPEHPK3PXP')
    expect(wrapper.text()).toContain('good@example.test'); expect(wrapper.text()).not.toContain('bad@example.test')
    expect(api.importBpsLogin).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="bps-batch-show-failures"]').setValue(true); expect(wrapper.text()).toContain('bad@example.test')
    await wrapper.get('[data-testid="bps-batch-import"]').trigger('click'); await flushPromises()
    expect(api.importBpsLogin).toHaveBeenCalledTimes(1); expect(wrapper.emitted('imported')).toHaveLength(1)
  })
})
