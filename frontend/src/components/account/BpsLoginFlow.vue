<template>
  <section class="space-y-4 rounded-lg border border-blue-300 bg-white/80 p-4 dark:border-blue-600 dark:bg-gray-800/80" data-testid="bps-login-flow">
    <p class="text-sm text-blue-700 dark:text-blue-300">{{ t('admin.accounts.oauth.openai.bpsLoginDescription') }}</p>
    <form v-if="!activeJob" class="space-y-3" @submit.prevent="start">
      <label class="block text-sm">
        {{ t('admin.accounts.oauth.openai.bpsLoginEmail') }}
        <input v-model="email" type="email" autocomplete="username" required class="input mt-1 w-full" data-testid="bps-email" :disabled="busy || importing" />
      </label>
      <label class="block text-sm">
        {{ t('admin.accounts.oauth.openai.bpsLoginPassword') }}
        <input v-model="password" type="password" autocomplete="current-password" required class="input mt-1 w-full" data-testid="bps-password" :disabled="busy || importing" />
      </label>
      <label class="block text-sm">
        {{ t('admin.accounts.oauth.openai.bpsLoginTotpSecret') }}
        <input v-model="totpSecret" type="password" autocomplete="off" required class="input mt-1 w-full font-mono" data-testid="bps-totp-secret" :disabled="busy || importing" />
        <span class="mt-1 block text-xs text-amber-700 dark:text-amber-300">{{ t('admin.accounts.oauth.openai.bpsLoginTotpHint') }}</span>
      </label>
      <label class="block text-sm">
        {{ t('admin.accounts.oauth.openai.bpsLoginWorkspaceId') }}
        <input v-model="workspaceId" type="text" autocomplete="off" class="input mt-1 w-full" data-testid="bps-workspace-id" :disabled="busy || importing" />
      </label>
      <label class="block text-sm">
        {{ t('admin.accounts.oauth.openai.bpsLoginWorkspacePreference') }}
        <select v-model="workspacePreference" class="input mt-1 w-full" data-testid="bps-workspace-preference" :disabled="busy || importing">
          <option value="team_first">{{ t('admin.accounts.oauth.openai.bpsLoginTeamFirst') }}</option>
          <option value="team">{{ t('admin.accounts.oauth.openai.bpsLoginTeamOnly') }}</option>
          <option value="personal">{{ t('admin.accounts.oauth.openai.bpsLoginPersonal') }}</option>
          <option value="first">{{ t('admin.accounts.oauth.openai.bpsLoginFirst') }}</option>
        </select>
      </label>
      <button type="submit" class="btn btn-primary w-full" :disabled="busy || importing" data-testid="bps-start">{{ t('admin.accounts.oauth.openai.bpsLoginStart') }}</button>
    </form>

    <div v-if="job" class="space-y-3">
      <p role="status" class="text-sm" data-testid="bps-job-status">{{ statusText }}<span v-if="job.status === 'running'"> · {{ phaseText }}</span></p>
      <p v-if="job.expires_at" class="text-xs text-gray-500">{{ t('admin.accounts.oauth.openai.bpsLoginExpires') }}: {{ displayTime(job.expires_at) }}</p>
      <form v-if="job.status === 'awaiting_input' && job.challenge_kind === 'email_otp'" class="space-y-2" @submit.prevent="sendInput('email_otp')">
        <label class="block text-sm">
          {{ t('admin.accounts.oauth.openai.bpsLoginEmailOtp') }}
          <input v-model="challengeValue" type="text" inputmode="numeric" autocomplete="one-time-code" class="input mt-1 w-full" maxlength="8" data-testid="bps-email-otp" :disabled="busy" />
        </label>
        <button type="submit" class="btn btn-primary" :disabled="busy || !challengeValue.trim()" data-testid="bps-submit-otp">{{ t('admin.accounts.oauth.openai.bpsLoginSubmitInput') }}</button>
      </form>
      <form v-if="job.status === 'awaiting_input' && job.challenge_kind === 'workspace'" class="space-y-2" @submit.prevent="sendInput('workspace')">
        <label class="block text-sm">
          {{ t('admin.accounts.oauth.openai.bpsLoginChooseWorkspace') }}
          <select v-model="challengeValue" class="input mt-1 w-full" data-testid="bps-workspace-choice" :disabled="busy">
            <option disabled value="">{{ t('admin.accounts.oauth.openai.bpsLoginChooseWorkspace') }}</option>
            <option v-for="choice in job.workspace_choices || []" :key="choice.id" :value="choice.id">{{ choice.name || choice.id }} ({{ choice.kind }})</option>
          </select>
        </label>
        <button type="submit" class="btn btn-primary" :disabled="busy || !challengeValue" data-testid="bps-submit-workspace">{{ t('admin.accounts.oauth.openai.bpsLoginSubmitInput') }}</button>
      </form>
      <div v-if="job.status === 'ready' && job.preview" class="space-y-2 rounded border border-green-300 p-3 dark:border-green-700" data-testid="bps-preview">
        <p class="text-sm">{{ t('admin.accounts.oauth.openai.bpsLoginEmail') }}: {{ job.preview.email }}</p>
        <p class="break-all text-sm">{{ t('admin.accounts.oauth.openai.bpsLoginPreviewWorkspace') }}: {{ job.preview.workspace_id }}</p>
        <p class="text-sm">{{ t('admin.accounts.oauth.openai.bpsLoginPlan') }}: {{ job.preview.plan_type }}</p>
        <p class="text-sm">{{ t('admin.accounts.oauth.openai.bpsLoginTokenExpires') }}: {{ displayTime(job.preview.expires_at) }}</p>
        <button type="button" class="btn btn-primary w-full" :disabled="importing || confirming" data-testid="bps-confirm-import" @click="confirmImport">{{ importing || confirming ? t('admin.accounts.oauth.openai.bpsLoginImporting') : t('admin.accounts.oauth.openai.bpsLoginConfirm') }}</button>
      </div>
      <button v-if="activeJob" type="button" class="btn btn-secondary" :disabled="importing || confirming" data-testid="bps-cancel" @click="reset">{{ t('admin.accounts.oauth.openai.bpsLoginCancel') }}</button>
    </div>
    <p v-if="error || importError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error || importError }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { safeBpsLoginCode } from '@/utils/bpsLogin'
import type { BpsLoginJob, BpsWorkspacePreference } from '@/api/admin/accounts'

const props = defineProps<{ proxyId?: number | null; importing?: boolean; importError?: string }>()
const emit = defineEmits<{ confirm: [jobId: string] }>()
const { t } = useI18n()
const email = ref('')
const password = ref('')
const totpSecret = ref('')
const workspaceId = ref('')
const workspacePreference = ref<BpsWorkspacePreference>('team_first')
const challengeValue = ref('')
const job = ref<BpsLoginJob | null>(null)
const busy = ref(false)
const confirming = ref(false)
const error = ref('')
let generation = 0
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
let disposed = false

const activeJob = computed(() => !!job.value && ['queued', 'running', 'awaiting_input', 'ready'].includes(job.value.status))
const phaseKeys: Record<string, string> = {
  session: 'bpsPhaseSession', email: 'bpsPhaseEmail', password: 'bpsPhasePassword',
  mfa: 'bpsPhaseMfa', email_otp: 'bpsPhaseEmailOtp', authorization: 'bpsPhaseAuthorization',
  exchange: 'bpsPhaseExchange', validate: 'bpsPhaseValidate',
}
const statusKeys: Record<string, string> = {
  queued: 'bpsStatusRunning', completed: 'bpsStatusCompleted', expired: 'bpsStatusExpired',
  running: 'bpsStatusRunning', awaiting_input: 'bpsStatusAwaitingInput', ready: 'bpsStatusReady',
  failed: 'bpsStatusFailed', cancelled: 'bpsStatusCancelled',
}
const phaseText = computed(() => t(`admin.accounts.oauth.openai.${phaseKeys[job.value?.phase || ''] || 'bpsStatusRunning'}`))
const statusText = computed(() => t(`admin.accounts.oauth.openai.${statusKeys[job.value?.status || ''] || 'bpsStatusRunning'}`))

const displayTime = (value: string | number) => {
  const date = new Date(typeof value === 'number' && value < 1e12 ? value * 1000 : value)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString()
}
const safeCode = safeBpsLoginCode
const errorCode = (value: unknown) => {
  const err = value as { code?: unknown; response?: { data?: { code?: unknown; reason?: unknown } } }
  return safeCode(err?.response?.data?.reason || err?.response?.data?.code || err?.code)
}
const showError = (code: string) => { error.value = `${t('admin.accounts.oauth.openai.bpsLoginFailed')}: ${code}` }
const cancelOwnedJob = (id: string) => {
  // Cancellation never logs credentials or untrusted upstream error bodies.
  void adminAPI.accounts.cancelBpsLogin(id).catch(() => {})
}
const stopRequests = () => {
  if (timer) clearTimeout(timer)
  timer = undefined
  controller?.abort()
  controller = undefined
}
const reset = () => {
  const ownId = job.value?.id
  const importing = confirming.value || props.importing
  generation++
  stopRequests()
  password.value = ''
  totpSecret.value = ''
  email.value = ''
  workspaceId.value = ''
  workspacePreference.value = 'team_first'
  challengeValue.value = ''
  job.value = null
  busy.value = false
  error.value = ''
  if (ownId && !importing) cancelOwnedJob(ownId)
}
const schedulePoll = (id: string, expected: number) => {
  if (disposed || expected !== generation || !['queued', 'running', 'awaiting_input'].includes(job.value?.status || '')) return
  timer = setTimeout(() => { void poll(id, expected) }, 1500)
}
const applyJob = (value: BpsLoginJob) => {
  const challengeChanged = job.value?.challenge_kind !== value.challenge_kind || job.value?.status !== value.status
  // Keep only the public job projection. Tokens are never requested or displayed.
  job.value = {
    id: value.id, status: value.status, phase: value.phase, code: value.code,
    expires_at: value.expires_at, challenge_kind: value.challenge_kind,
    workspace_choices: value.workspace_choices, preview: value.preview,
  }
  if (challengeChanged) challengeValue.value = ''
  if (value.status === 'failed' || value.status === 'expired') showError(safeCode(value.code || 'LOGIN_TIMEOUT'))
}
async function poll(id: string, expected: number) {
  if (disposed || generation !== expected || busy.value) return
  const expiry = job.value?.expires_at
  const expiryTime = new Date(typeof expiry === 'number' && expiry < 1e12 ? expiry * 1000 : expiry || '').getTime()
  if (Number.isFinite(expiryTime) && Date.now() >= expiryTime) {
    if (job.value) job.value = { ...job.value, status: 'failed', code: 'LOGIN_TIMEOUT' }
    showError('LOGIN_TIMEOUT')
    cancelOwnedJob(id)
    return
  }
  controller = new AbortController()
  try {
    const value = await adminAPI.accounts.getBpsLogin(id, controller.signal)
    if (disposed || generation !== expected || job.value?.id !== id) return
    applyJob(value)
    error.value = value.status === 'failed' || value.status === 'expired' ? error.value : ''
    schedulePoll(id, expected)
  } catch (err) {
    if (disposed || generation !== expected || controller?.signal.aborted) return
    showError(errorCode(err))
    // A transient read failure must not create another login task.
    schedulePoll(id, expected)
  }
}
const start = async () => {
  if (busy.value || props.importing || confirming.value || activeJob.value) return
  const secret = totpSecret.value.replace(/\s/g, '').toUpperCase()
  if (!email.value.trim() || !password.value || !/^[A-Z2-7]{16,256}={0,6}$/.test(secret)) {
    error.value = t('admin.accounts.oauth.openai.bpsLoginInvalidCredentials')
    return
  }
  const request = {
    email: email.value.trim(), password: password.value, totp_secret: secret,
    workspace_id: workspaceId.value.trim() || undefined,
    workspace_preference: workspacePreference.value,
    proxy_id: props.proxyId ?? undefined,
  }
  generation++
  const expected = generation
  stopRequests()
  if (job.value?.id) cancelOwnedJob(job.value.id)
  job.value = null
  error.value = ''
  busy.value = true
  password.value = ''
  totpSecret.value = ''
  try {
    // Do not abort task creation: a late ACK supplies the ID needed to cancel it.
    const value = await adminAPI.accounts.startBpsLogin(request)
    if (disposed || expected !== generation) { cancelOwnedJob(value.id); return }
    applyJob(value)
    schedulePoll(value.id, expected)
  } catch (err) {
    if (!disposed && expected === generation) showError(errorCode(err))
  } finally {
    request.password = ''
    request.totp_secret = ''
    if (expected === generation) busy.value = false
  }
}
const sendInput = async (kind: 'email_otp' | 'workspace') => {
  const current = job.value
  if (!current || current.status !== 'awaiting_input' || current.challenge_kind !== kind || busy.value) return
  const value = challengeValue.value.trim()
  if ((kind === 'email_otp' && !/^\d{6,8}$/.test(value)) || (kind === 'workspace' && !current.workspace_choices?.some((choice) => choice.id === value))) {
    error.value = t('admin.accounts.oauth.openai.bpsLoginInvalidInput')
    return
  }
  // Retire any in-flight poll before advancing the task with operator input.
  generation++
  const expected = generation
  stopRequests()
  controller = new AbortController()
  busy.value = true
  challengeValue.value = ''
  error.value = ''
  try {
    const next = await adminAPI.accounts.submitBpsLoginInput(current.id, { kind, value }, controller.signal)
    if (disposed || expected !== generation || job.value?.id !== current.id) return
    applyJob(next)
  } catch (err) {
    if (!disposed && expected === generation && !controller?.signal.aborted) showError(errorCode(err))
  } finally {
    if (expected === generation) { busy.value = false; schedulePoll(current.id, expected) }
  }
}
const confirmImport = async () => {
  if (job.value?.status !== 'ready' || !job.value.preview || props.importing || confirming.value) return
  confirming.value = true
  generation++
  stopRequests()
  emit('confirm', job.value.id)
  await nextTick()
  if (!props.importing && !disposed) confirming.value = false
}
watch(() => props.importing, (value, previous) => {
  if (!value && previous) confirming.value = false
})
watch(() => props.proxyId, reset)
onBeforeUnmount(() => { disposed = true; reset() })
defineExpose({ reset })
</script>
