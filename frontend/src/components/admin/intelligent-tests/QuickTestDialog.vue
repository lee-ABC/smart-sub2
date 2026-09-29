<template>
  <BaseDialog :show="accountId !== null" :title="`测试账号 #${accountId}`" width="narrow" @close="!busy && emit('close')">
    <p class="mb-4 text-sm leading-relaxed text-gray-500">测试会发送真实模型请求并产生上游用量。提交后在后台排队执行，可到智能测试页面查看进度。</p>
    <p v-if="error" class="mb-4 text-sm text-red-600" role="alert">{{ error }}</p>
    <label class="mb-4 block text-sm text-gray-600 dark:text-gray-300">测试模型
      <select v-model="selectedModel" class="input mt-1.5" :disabled="busy || loadingModels">
        <option value="">使用测试设置默认模型</option>
        <option v-for="model in models" :key="model.id" :value="model.id">{{ model.display_name || model.id }}</option>
      </select>
      <span v-if="loadingModels" class="mt-1 block text-xs text-gray-400">正在读取账号可用模型…</span>
    </label>
    <label v-if="testGroups.length" class="mb-4 block text-sm text-gray-600 dark:text-gray-300">测试分组（跟随 Excel / 原生设置）
      <select v-model.number="selectedGroup" data-testid="quick-test-group" class="input mt-1.5" :disabled="busy" @change="reloadModels">
        <option :value="0">自动（多分组含 Excel 时需明确选择）</option>
        <option v-for="group in testGroups" :key="group.id" :value="group.id">{{ group.name }} (#{{ group.id }})</option>
      </select>
    </label>
    <div class="grid gap-3">
      <button v-for="setting in settings" :key="setting.test_type" class="btn btn-secondary justify-between" :disabled="busy || !accountReady || loadingModels || !setting.enabled" @click="run([setting.test_type])">运行{{ setting.name || testName(setting.test_type) }} <span v-if="!setting.enabled" class="text-xs">未启用</span></button>
      <button class="btn btn-primary" :disabled="busy || !accountReady || loadingModels || !enabledTypes.length" @click="run(enabledTypes)">{{ busy ? '正在提交…' : '运行全部测试' }}</button>
    </div>
  </BaseDialog>
</template>
<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { intelligentTestsAPI, newTestRequestKey, type TestSetting } from '@/api/intelligentTests'
import { getAvailableModels, getById } from '@/api/admin/accounts'
import { extractApiErrorMessage } from '@/utils/apiError'
import { useAppStore } from '@/stores/app'
import { testName, isTextTestModel, testSubmissionMessage } from './display'
const props = defineProps<{ accountId: number | null }>()
const emit = defineEmits<{ close: []; queued: [] }>()
const settings = ref<TestSetting[]>([]), busy = ref(false), error = ref('')
const models = ref<{ id: string; display_name?: string }[]>([]), selectedModel = ref(''), loadingModels = ref(false)
const testGroups = ref<{ id: number; name: string }[]>([]), selectedGroup = ref(0), accountReady = ref(false)
let modelVersion = 0
const app = useAppStore()
const enabledTypes = computed(() => settings.value.filter(item => item.enabled).map(item => item.test_type))
let key = '', signature = '', generation = 0
watch(() => props.accountId, async id => {
  const current = ++generation
  key = ''; signature = ''; error.value = ''; settings.value = []; models.value = []; selectedModel.value = ''
  modelVersion++; testGroups.value = []; selectedGroup.value = 0; accountReady.value = false
  if (id === null) return
  try {
    loadingModels.value = true
    // Settings are required to render the test actions; model discovery is
    // only a convenience and must never block the dialog when an upstream
    // catalog is slow or unavailable.
    const settingsPromise = intelligentTestsAPI.settings().then(result => {
      if (current === generation) settings.value = result
    })
    const modelsPromise = getById(id).then(async account => {
      if (current !== generation) return
      if (account.platform === 'openai') {
        const ids = [...new Set([...(account.group_ids ?? []), ...(account.groups ?? []).map(group => group.id)])]
        testGroups.value = ids.map(groupId => ({ id: groupId, name: account.groups?.find(group => group.id === groupId)?.name || '分组' }))
        selectedGroup.value = ids.length === 1 ? ids[0] : 0
      }
      accountReady.value = true
      await reloadModels()
    })
    await Promise.all([settingsPromise, modelsPromise])
  }
  catch (err) { if (current === generation) error.value = extractApiErrorMessage(err, '无法读取测试设置') }
  finally { if (current === generation && !accountReady.value) loadingModels.value = false }
}, { immediate: true })
onUnmounted(() => { generation++; modelVersion++ })
async function reloadModels() {
  const id = props.accountId
  if (id === null) return
  const current = ++modelVersion
  const accountGeneration = generation
  loadingModels.value = true; models.value = []; selectedModel.value = ''; error.value = ''
  try {
    const result = await getAvailableModels(id, selectedGroup.value || undefined)
    if (current === modelVersion && accountGeneration === generation) models.value = result.filter(model => isTextTestModel(model.id))
  } catch (err) {
    if (current === modelVersion && accountGeneration === generation) error.value = extractApiErrorMessage(err, '模型列表读取失败；多分组账号请先选择测试分组')
  } finally { if (current === modelVersion && accountGeneration === generation) loadingModels.value = false }
}
async function run(types: string[]) {
  if (props.accountId === null || busy.value || !accountReady.value || loadingModels.value) return
  const current = generation
  types = [...types].sort()
  const nextSignature = JSON.stringify([props.accountId, types, selectedModel.value, selectedGroup.value])
  if (signature !== nextSignature) { key = newTestRequestKey(); signature = nextSignature }
  busy.value = true; error.value = ''
  try {
    const overrides = selectedModel.value ? Object.fromEntries(types.map(type => [type, selectedModel.value])) : undefined
    const data = selectedGroup.value > 0
      ? await intelligentTestsAPI.run([props.accountId], types, key, overrides, selectedGroup.value)
      : overrides ? await intelligentTestsAPI.run([props.accountId], types, key, overrides) : await intelligentTestsAPI.run([props.accountId], types, key)
    app.showSuccess(testSubmissionMessage(data)); emit('queued')
    if (current === generation) { signature = ''; emit('close') }
  }
  catch (err) { if (current === generation) error.value = extractApiErrorMessage(err, '任务提交失败，可重试') }
  finally { busy.value = false }
}
</script>
