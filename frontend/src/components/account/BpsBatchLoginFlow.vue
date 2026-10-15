<template>
  <section class="space-y-4 rounded-lg border border-blue-300 p-4 dark:border-blue-600" data-testid="bps-batch-flow">
    <p class="text-sm text-blue-700 dark:text-blue-300">{{ t(`${key}bpsBatchDescription`) }}</p>
    <form v-if="!rows.length" class="space-y-3" @submit.prevent="startBatch">
      <label class="block text-sm">{{ t(`${key}bpsBatchInput`) }}
        <textarea v-model="input" class="input mt-1 min-h-36 w-full font-mono text-sm" rows="6" autocomplete="off" autocapitalize="off" :spellcheck="false" :placeholder="t(`${key}bpsBatchPlaceholder`)" data-testid="bps-batch-input" />
      </label>
      <label class="block text-sm">{{ t(`${key}bpsLoginWorkspaceId`) }}
        <input v-model="workspaceId" class="input mt-1 w-full" autocomplete="off" data-testid="bps-batch-workspace-id" />
      </label>
      <label class="block text-sm">{{ t(`${key}bpsLoginWorkspacePreference`) }}
        <select v-model="preference" class="input mt-1 w-full" data-testid="bps-batch-workspace-preference">
          <option value="team_first">{{ t(`${key}bpsLoginTeamFirst`) }}</option>
          <option value="team">{{ t(`${key}bpsLoginTeamOnly`) }}</option>
          <option value="personal">{{ t(`${key}bpsLoginPersonal`) }}</option>
          <option value="first">{{ t(`${key}bpsLoginFirst`) }}</option>
        </select>
      </label>
      <button type="submit" class="btn btn-primary w-full" :disabled="!input.trim()" data-testid="bps-batch-start">{{ t(`${key}bpsBatchStart`) }}</button>
    </form>
    <div v-if="issues.length" class="text-sm text-amber-700 dark:text-amber-300" role="alert" data-testid="bps-batch-issues">
      <p v-for="issue in issues" :key="`${issue.line}-${issue.code}`">{{ t(`${key}bpsBatchLineIssue`, { line: issue.line, reason: t(`${key}bpsBatchIssue${issue.code}`) }) }}</p>
    </div>
    <div v-if="rows.length" class="space-y-3">
      <p role="status" class="text-sm" data-testid="bps-batch-progress">{{ t(`${key}bpsBatchProgress`, { total: rows.length, active: activeCount, ready: readyRows.length, imported: importedCount, failed: failedCount }) }}</p>
      <p class="text-xs text-gray-500">{{ t(`${key}bpsBatchRetention`) }}</p>
      <div class="flex flex-wrap items-center gap-3">
        <button type="button" class="btn btn-primary" :disabled="importing || !readyRows.length || !getImportOptions" data-testid="bps-batch-import" @click="importReady">{{ importing ? t(`${key}bpsLoginImporting`) : t(`${key}bpsBatchImport`, { count: readyRows.length }) }}</button>
        <button type="button" class="btn btn-secondary" :disabled="importing" data-testid="bps-batch-reset" @click="clear">{{ t(`${key}bpsBatchReset`) }}</button>
        <label class="flex items-center gap-2 text-sm"><input v-model="showFailures" type="checkbox" data-testid="bps-batch-show-failures" />{{ t(`${key}bpsBatchShowFailures`) }}</label>
      </div>
      <div class="max-h-96 space-y-2 overflow-y-auto" data-testid="bps-batch-results">
        <article v-for="row in visibleRows" :key="row.line" class="space-y-2 rounded border border-gray-200 p-3 dark:border-gray-600" :data-line="row.line">
          <p class="break-all text-sm font-medium">{{ row.line }}. {{ row.email }}</p>
          <p class="text-xs" :data-testid="`bps-batch-row-status-${row.line}`">{{ rowStatus(row) }}<span v-if="row.code"> · {{ row.code }}</span></p>
          <p v-if="row.job?.preview && row.state === 'ready'" class="break-all text-xs text-gray-500">{{ t(`${key}bpsLoginPreviewWorkspace`) }}: {{ row.job.preview.workspace_id }}</p>
          <form v-if="row.state === 'awaiting_input'" class="flex flex-wrap gap-2" @submit.prevent="submitInput(row)">
            <input v-if="row.job?.challenge_kind === 'email_otp'" v-model="row.challenge" class="input" inputmode="numeric" autocomplete="one-time-code" maxlength="8" :aria-label="t(`${key}bpsLoginEmailOtp`)" :disabled="row.busy" :data-testid="`bps-batch-otp-${row.line}`" />
            <select v-else v-model="row.challenge" class="input max-w-full" :disabled="row.busy" :aria-label="t(`${key}bpsLoginChooseWorkspace`)" :data-testid="`bps-batch-workspace-${row.line}`">
              <option value="" disabled>{{ t(`${key}bpsLoginChooseWorkspace`) }}</option>
              <option v-for="choice in row.job?.workspace_choices || []" :key="choice.id" :value="choice.id">{{ choice.name || choice.id }} ({{ choice.kind }})</option>
            </select>
            <button class="btn btn-secondary" type="submit" :disabled="row.busy || !row.challenge.trim()">{{ t(`${key}bpsLoginSubmitInput`) }}</button>
          </form>
          <p v-if="row.importState === 'unknown' || row.state === 'unknown'" class="text-xs text-amber-700">{{ t(`${key}bpsBatchUnknown`) }}</p>
        </article>
      </div>
    </div>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ t(`${key}bpsLoginFailed`) }}: {{ error }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useBpsBatchLogin, type BpsBatchRow } from '@/composables/useBpsBatchLogin'
import type { BpsBatchImportOptions } from '@/utils/bpsBatchLogin'
import type { BpsWorkspacePreference } from '@/api/admin/accounts'

const props = defineProps<{ proxyId?: number | null; getImportOptions?: () => BpsBatchImportOptions | null }>()
const emit = defineEmits<{ imported: [] }>()
const { t } = useI18n()
const key = 'admin.accounts.oauth.openai.'
const input = ref('')
const preference = ref<BpsWorkspacePreference>('team_first')
const workspaceId = ref('')
const showFailures = ref(false)
const batch = useBpsBatchLogin({ proxyId: () => props.proxyId, getImportOptions: () => props.getImportOptions?.(), onImported: () => emit('imported') })
const { rows, issues, error, importing, activeCount, readyRows, importReady, submitInput } = batch
const importedCount = computed(() => rows.value.filter(row => row.importState === 'imported').length)
const failedCount = computed(() => rows.value.filter(row => ['failed', 'expired', 'cancelled', 'unknown'].includes(row.state) || ['failed', 'unknown'].includes(row.importState)).length)
const visibleRows = computed(() => rows.value.filter(row => showFailures.value || !['failed', 'expired', 'cancelled'].includes(row.state)))
function startBatch() {
  const text = input.value
  input.value = ''
  if (!batch.start(text, preference.value, workspaceId.value) && !issues.value.length) error.value = 'LOGIN_REQUEST_REJECTED'
}
function clear() { input.value = ''; workspaceId.value = ''; preference.value = 'team_first'; batch.reset() }
const statusKeys: Record<string, string> = { queued: 'bpsBatchQueued', starting: 'bpsStatusRunning', running: 'bpsStatusRunning', awaiting_input: 'bpsStatusAwaitingInput', ready: 'bpsStatusReady', completed: 'bpsStatusCompleted', failed: 'bpsStatusFailed', cancelled: 'bpsStatusCancelled', expired: 'bpsStatusExpired', unknown: 'bpsBatchUnknownStatus' }
const importKeys: Record<string, string> = { importing: 'bpsLoginImporting', imported: 'bpsBatchImported', skipped: 'bpsBatchSkipped', failed: 'bpsBatchImportFailed', unknown: 'bpsBatchUnknownStatus' }
const rowStatus = (row: BpsBatchRow) => t(key + (importKeys[row.importState] || statusKeys[row.state] || 'bpsStatusFailed'))
watch(() => props.proxyId, clear)
onBeforeUnmount(() => { input.value = ''; batch.dispose() })
defineExpose({ reset: clear })
</script>
