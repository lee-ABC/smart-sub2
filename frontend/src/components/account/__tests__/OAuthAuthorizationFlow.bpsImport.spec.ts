import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: ref(false), copyToClipboard: vi.fn() }),
}))
vi.mock('@/api/admin', () => ({ adminAPI: {} }))

import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

const wrappers: VueWrapper[] = []
const mountFlow = () => {
  const wrapper = mount(OAuthAuthorizationFlow, {
    props: {
      addMethod: 'oauth',
      platform: 'openai',
      initialInputMethod: 'codex_session',
      showCodexSessionImportOption: true,
      showCookieOption: false,
      showHelp: false,
    },
    global: { stubs: { Icon: true } },
  })
  wrappers.push(wrapper)
  return wrapper
}
const file = (content: string, name = 'credentials.json') => ({
  name,
  size: content.length,
  text: vi.fn().mockResolvedValue(content),
}) as unknown as File

const selectFiles = async (wrapper: VueWrapper, files: File[]) => {
  const picker = wrapper.get('[data-testid="codex-session-files"]')
  Object.defineProperty(picker.element, 'files', { configurable: true, value: files })
  await picker.trigger('change')
  await flushPromises()
}
const submit = (wrapper: VueWrapper) => wrapper.get('[data-testid="import-codex-session"]')

const deferred = () => {
  let resolve!: (value: string) => void
  let reject!: (reason: Error) => void
  const promise = new Promise<string>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

afterEach(() => {
  for (const wrapper of wrappers) wrapper.unmount()
  wrappers.length = 0
})

describe('OAuth credential file import', () => {
  it('combines multiple JSON files and a pasted raw token without changing either profile', async () => {
    const wrapper = mountFlow()
    const native = { access_token: 'native-at', refresh_token: 'native-rt' }
    const bps = { type: 'oai-basispoints', access_token: 'bps-at', refresh_token: 'bps-rt' }
    await wrapper.get('textarea').setValue('raw-access-token')
    await selectFiles(wrapper, [file(JSON.stringify(native, null, 2)), file(JSON.stringify([bps], null, 2))])
    await submit(wrapper).trigger('click')
    expect(wrapper.emitted('import-codex-session')).toEqual([
      [['raw-access-token', JSON.stringify(native), JSON.stringify([bps])].join('\n')],
    ])
    expect(wrapper.find('input[type="file"]').attributes('multiple')).toBeDefined()
    expect(wrapper.get<HTMLInputElement>('input[type="file"]').element.value).toBe('')
    expect(wrapper.text()).toContain('codexSessionDualCredentialsHint')
    expect(wrapper.text()).toContain('codexSessionOutboundHint')
  })

  it('retains manual raw token import without selecting any files', async () => {
    const wrapper = mountFlow()
    await wrapper.get('textarea').setValue('  first-at\nsecond-at  ')
    await submit(wrapper).trigger('click')
    expect(wrapper.emitted('import-codex-session')).toEqual([['first-at\nsecond-at']])
  })

  it('rejects all files atomically and never renders parser text containing secrets', async () => {
    const wrapper = mountFlow()
    await selectFiles(wrapper, [file('{"access_token":"first-at"}'), file('invalid-secret-bps-at')])
    expect(wrapper.get('[role="alert"]').text()).toContain('codexSessionFilesInvalid')
    expect(wrapper.text()).not.toContain('invalid-secret-bps-at')
    expect(submit(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('import-codex-session')).toBeUndefined()
    await wrapper.get('[data-testid="clear-codex-session-files"]').trigger('click')
    await wrapper.get('textarea').setValue('manual-at')
    await submit(wrapper).trigger('click')
    expect(wrapper.emitted('import-codex-session')).toEqual([['manual-at']])
  })

  it('rejects JSON scalars and oversized selections before importing', async () => {
    const wrapper = mountFlow()
    await selectFiles(wrapper, [file('"credential-string"')])
    expect(wrapper.get('[role="alert"]').text()).toContain('codexSessionFilesInvalid')
    const oversized = { size: 10 * 1024 * 1024 + 1, text: vi.fn() } as unknown as File
    await selectFiles(wrapper, [oversized])
    expect(oversized.text).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('codexSessionFilesTooLarge')
    expect(submit(wrapper).attributes('disabled')).toBeDefined()
  })

  it('clears both selected files and pasted credentials when switching mode', async () => {
    const wrapper = mountFlow()
    await wrapper.get('textarea').setValue('pasted-secret')
    await selectFiles(wrapper, [file('{"access_token":"file-secret"}')])
    await wrapper.get('input[value="manual"]').setValue()
    await wrapper.get('input[value="codex_session"]').setValue()
    expect(wrapper.get<HTMLTextAreaElement>('textarea').element.value).toBe('')
    expect(submit(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="clear-codex-session-files"]').exists()).toBe(false)
  })

  it('does not repopulate credentials when a pending read resolves after reset', async () => {
    const wrapper = mountFlow()
    const pending = deferred()
    await selectFiles(wrapper, [{ size: 30, text: () => pending.promise } as File])
    expect(submit(wrapper).attributes('disabled')).toBeDefined()
    ;(wrapper.vm as unknown as { reset: () => void }).reset()
    pending.resolve('{"access_token":"stale-secret"}')
    await flushPromises()
    expect(wrapper.get<HTMLTextAreaElement>('textarea').element.value).toBe('')
    expect(submit(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="clear-codex-session-files"]').exists()).toBe(false)
  })

  it('ignores a stale failed read without clearing a later successful selection', async () => {
    const wrapper = mountFlow()
    const pending = deferred()
    await selectFiles(wrapper, [{ size: 30, text: () => pending.promise } as File])
    await wrapper.get('[data-testid="clear-codex-session-files"]').trigger('click')
    await selectFiles(wrapper, [file('{"access_token":"current-secret"}')])
    pending.reject(new Error('stale-secret'))
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    await submit(wrapper).trigger('click')
    expect(wrapper.emitted('import-codex-session')).toEqual([['{"access_token":"current-secret"}']])
  })

  it('clears retained credentials on unmount and ignores pending read completion', async () => {
    const wrapper = mountFlow()
    const pending = deferred()
    await wrapper.get('textarea').setValue('pasted-secret')
    await selectFiles(wrapper, [{ size: 30, text: () => pending.promise } as File])
    const exposed = wrapper.vm as unknown as { codexSession: string }
    wrapper.unmount()
    pending.resolve('{"access_token":"stale-secret"}')
    await flushPromises()
    expect(exposed.codexSession).toBe('')
    expect(wrapper.emitted('import-codex-session')).toBeUndefined()
  })
})
