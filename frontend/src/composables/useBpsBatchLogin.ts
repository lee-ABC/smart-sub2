import { computed, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { BpsLoginJob, BpsLoginStartRequest, BpsWorkspacePreference } from '@/api/admin/accounts'
import { safeBpsLoginCode } from '@/utils/bpsLogin'
import { BPS_BATCH_CONCURRENCY, parseBpsLoginBatch, type BpsBatchCredential, type BpsBatchImportOptions, type BpsBatchIssue } from '@/utils/bpsBatchLogin'

export interface BpsBatchRow {
  line: number
  email: string
  state: BpsLoginJob['status'] | 'starting' | 'unknown'
  job?: BpsLoginJob
  code: string
  challenge: string
  busy: boolean
  version: number
  importState: 'none' | 'importing' | 'imported' | 'failed' | 'unknown' | 'skipped'
}

export function useBpsBatchLogin(options: {
  proxyId: () => number | null | undefined
  getImportOptions: () => BpsBatchImportOptions | null | undefined
  onImported?: () => void
}) {
  const rows = ref<BpsBatchRow[]>([])
  const issues = ref<BpsBatchIssue[]>([])
  const importing = ref(false)
  const error = ref('')
  const secrets = new Map<number, BpsBatchCredential>()
  const controllers = new Map<number, AbortController>()
  const polling = new Map<number, AbortController>()
  let generation = 0
  let disposed = false
  let timer: ReturnType<typeof setTimeout> | undefined
  let queuedUntil = 0
  let workspace: { workspace_id?: string; workspace_preference: BpsWorkspacePreference } = { workspace_preference: 'team_first' }
  const isActive = (row: BpsBatchRow) => row.state === 'starting' || row.state === 'running' || row.state === 'awaiting_input' || (row.state === 'queued' && !!row.job)
  const pending = computed(() => rows.value.some(row => row.state === 'queued' || isActive(row)))
  const activeCount = computed(() => rows.value.filter(isActive).length)
  const expiry = (job?: BpsLoginJob) => {
    const value = job?.expires_at
    return new Date(typeof value === 'number' && value < 1e12 ? value * 1000 : value || '').getTime()
  }
  const readyRows = computed(() => rows.value.filter(row => row.state === 'ready' && row.importState === 'none' && row.job?.preview && expiry(row.job) > Date.now()))
  const current = (expected: number) => !disposed && generation === expected
  const cancelJob = (id: string) => { void adminAPI.accounts.cancelBpsLogin(id).catch(() => {}) }
  const wipe = (record?: BpsBatchCredential) => { if (record) { record.password = ''; record.totp_secret = '' } }
  const codeFor = (value: unknown) => {
    const err = value as { code?: unknown; response?: { status?: number; data?: { code?: unknown; reason?: unknown } } }
    if (err?.response?.status === 429) return 'LOGIN_RATE_LIMITED'
    return safeBpsLoginCode(err?.response?.data?.reason || err?.response?.data?.code || err?.code)
  }
  function reset() {
    generation++
    if (timer) clearTimeout(timer)
    timer = undefined
    controllers.forEach(controller => controller.abort())
    controllers.clear(); polling.clear()
    secrets.forEach(wipe); secrets.clear()
    for (const row of rows.value) {
      row.challenge = ''
      if (row.job && row.importState !== 'importing' && row.importState !== 'imported') cancelJob(row.job.id)
    }
    rows.value = []; issues.value = []; importing.value = false; error.value = ''
  }
  function apply(row: BpsBatchRow, value: BpsLoginJob) {
    if ((row.job && row.job.id !== value.id) || !Number.isFinite(expiry(value))) throw new Error('INVALID_LOGIN_RESPONSE')
    const previousChallenge = row.job?.challenge_kind
    // Explicit public projection: never retain arbitrary server response fields.
    row.job = { id: value.id, status: value.status, phase: value.phase, phase_elapsed_ms: value.phase_elapsed_ms, total_elapsed_ms: value.total_elapsed_ms, code: value.code, expires_at: value.expires_at, challenge_kind: value.challenge_kind,
      workspace_choices: value.workspace_choices?.map(choice => ({ id: choice.id, name: choice.name, kind: choice.kind })),
      preview: value.preview ? { email: value.preview.email, workspace_id: value.preview.workspace_id, plan_type: value.preview.plan_type, expires_at: value.preview.expires_at } : undefined }
    row.state = value.status
    row.code = value.code ? safeBpsLoginCode(value.code) : ''
    if (previousChallenge !== value.challenge_kind || value.status !== 'awaiting_input') row.challenge = ''
    if (value.status === 'ready' && (!value.preview?.workspace_id || typeof value.preview.email !== 'string' || value.preview.email.toLowerCase() !== row.email.toLowerCase())) { row.state = 'failed'; row.code = 'TOKEN_ACCOUNT_MISMATCH'; cancelJob(value.id) }
    if (['failed', 'expired', 'cancelled'].includes(row.state)) cancelJob(value.id)
  }
  function schedule() {
    if (disposed || timer || !rows.value.some(row => row.state === 'queued' || isActive(row) || (row.state === 'ready' && row.importState === 'none'))) return
    const expected = generation
    timer = setTimeout(() => {
      timer = undefined
      if (!current(expected)) return
      for (const row of rows.value) {
        if (row.state === 'queued' && !row.job && Date.now() >= queuedUntil) { wipe(secrets.get(row.line)); secrets.delete(row.line); row.state = 'expired'; row.code = 'LOGIN_TIMEOUT' }
        if (row.job && (isActive(row) || (row.state === 'ready' && row.importState === 'none')) && Date.now() >= expiry(row.job)) { row.state = 'expired'; row.code = 'LOGIN_TIMEOUT'; row.version++; controllers.get(row.line)?.abort(); cancelJob(row.job.id) }
        if (row.job && isActive(row) && !row.busy && !polling.has(row.line)) void poll(row, expected)
      }
      pump(expected); schedule()
    }, 1500)
  }
  async function poll(row: BpsBatchRow, expected: number) {
    const id = row.job!.id
    const version = ++row.version
    const controller = new AbortController()
    polling.set(row.line, controller); controllers.set(row.line, controller)
    try {
      const job = await adminAPI.accounts.getBpsLogin(id, controller.signal)
      if (current(expected) && version === row.version) apply(row, job)
    } catch (err) {
      if (current(expected) && version === row.version && !controller.signal.aborted) {
        row.code = codeFor(err)
        const status = (err as { response?: { status?: number } })?.response?.status
        if (status === 404 || status === 410) { row.state = 'expired'; row.code = 'LOGIN_TIMEOUT' }
      }
    } finally {
      if (current(expected)) { if (polling.get(row.line) === controller) polling.delete(row.line); if (controllers.get(row.line) === controller) controllers.delete(row.line); pump(expected); schedule() }
    }
  }
  async function launch(row: BpsBatchRow, expected: number) {
    const record = secrets.get(row.line)
    if (!record) return
    secrets.delete(row.line)
    const request: BpsLoginStartRequest = { email: record.email, password: record.password, totp_secret: record.totp_secret, ...workspace, proxy_id: options.proxyId() ?? undefined }
    wipe(record); row.state = 'starting'
    let acknowledgedId: string | undefined
    try {
      // Do not abort create: a late acknowledgement gives us the job to cancel.
      const job = await adminAPI.accounts.startBpsLogin(request)
      acknowledgedId = job.id
      if (!current(expected)) { cancelJob(job.id); return }
      apply(row, job)
    } catch (err) {
      if (acknowledgedId) cancelJob(acknowledgedId)
      if (current(expected)) { row.state = (err as { response?: unknown })?.response ? 'failed' : 'unknown'; row.code = codeFor(err) }
    } finally {
      request.password = ''; request.totp_secret = ''
      if (current(expected)) { pump(expected); schedule() }
    }
  }
  function pump(expected: number) {
    if (!current(expected)) return
    for (const row of rows.value) {
      if (activeCount.value >= BPS_BATCH_CONCURRENCY) break
      if (row.state === 'queued' && !row.job) {
        if (Date.now() >= queuedUntil) { wipe(secrets.get(row.line)); secrets.delete(row.line); row.state = 'expired'; row.code = 'LOGIN_TIMEOUT' }
        else void launch(row, expected)
      }
    }
  }
  function start(text: string, preference: BpsWorkspacePreference, workspaceID: string) {
    if (disposed || pending.value || importing.value || rows.value.length) return false
    const parsed = parseBpsLoginBatch(text)
    issues.value = parsed.issues
    if (!parsed.entries.length) return false
    generation++; queuedUntil = Date.now() + 600_000
    workspace = { workspace_preference: preference, workspace_id: workspaceID.trim() || undefined }
    rows.value = parsed.entries.map(record => { secrets.set(record.line, record); return { line: record.line, email: record.email, state: 'queued', code: '', challenge: '', busy: false, version: 0, importState: 'none' } })
    pump(generation); schedule(); return true
  }
  async function submitInput(row: BpsBatchRow) {
    const kind = row.job?.challenge_kind
    const value = row.challenge.trim()
    if (!row.job || row.state !== 'awaiting_input' || row.busy || !kind) return
    if ((kind === 'email_otp' && !/^\d{6,8}$/.test(value)) || (kind === 'workspace' && !row.job.workspace_choices?.some(choice => choice.id === value))) { row.code = 'LOGIN_REQUEST_REJECTED'; return }
    const expected = generation; const version = ++row.version
    controllers.get(row.line)?.abort()
    polling.delete(row.line)
    const controller = new AbortController(); controllers.set(row.line, controller)
    row.busy = true; row.challenge = ''
    try {
      const job = await adminAPI.accounts.submitBpsLoginInput(row.job.id, { kind, value }, controller.signal)
      if (current(expected) && row.version === version) apply(row, job)
    } catch (err) { if (current(expected) && row.version === version && !controller.signal.aborted) row.code = codeFor(err) }
    finally { if (current(expected)) { row.busy = false; if (controllers.get(row.line) === controller) controllers.delete(row.line); pump(expected); schedule() } }
  }
  async function importReady() {
    if (disposed || importing.value || !readyRows.value.length) return
    const settings = options.getImportOptions()
    if (!settings) { error.value = 'BPS_IMPORT_FAILED'; return }
    const frozen: BpsBatchImportOptions = JSON.parse(JSON.stringify(settings))
    const expected = generation
    const selected = [...readyRows.value]
    importing.value = true; error.value = ''
    let changed = false
    try {
      for (const row of selected) {
        if (!current(expected)) break
        if (!row.job || expiry(row.job) <= Date.now()) { row.state = 'expired'; row.code = 'LOGIN_TIMEOUT'; continue }
        row.importState = 'importing'
        const id = row.job.id
        try {
          const result = await adminAPI.accounts.importBpsLogin(id, { ...frozen, name: row.email, update_existing: true })
          if (!current(expected)) { cancelJob(id); break }
          if (result.failed > 0) { row.importState = 'failed'; row.code = 'BPS_IMPORT_FAILED' }
          else if (result.created + result.updated > 0) { row.importState = 'imported'; changed = true }
          else row.importState = 'skipped'
        } catch {
          // Unknown mutation outcomes are never automatically retried or relogged.
          if (current(expected)) { row.importState = 'unknown'; row.code = 'BPS_IMPORT_FAILED' } else cancelJob(id)
        }
      }
    } finally {
      if (current(expected)) { importing.value = false; if (changed) options.onImported?.(); schedule() }
    }
  }
  function dispose() { reset(); disposed = true }
  return { rows, issues, error, importing, pending, activeCount, readyRows, start, reset, dispose, submitInput, importReady }
}
