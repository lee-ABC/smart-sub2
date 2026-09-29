<template>
  <section class="mt-4 rounded-lg border border-gray-200 p-4 dark:border-dark-400">
    <h4 class="text-sm font-medium">{{ zh ? 'OpenAI 出站通道' : 'OpenAI outbound transport' }}</h4>
    <p class="mt-1 text-xs text-gray-500">{{ zh ? '仅切换已选账号的出站方式；原分组调度、优先级、粘性会话与故障切换不变。Excel 仅接管已支持的模型（6-sol / 6-astra / 5.6-sol / terra / luna）；gpt-5.5 等其他模型及 API Key 账号保持原生。支持 Responses / Chat / Messages。此设置独立保存。' : 'Only changes outbound transport. Existing group scheduling, priority, sticky sessions and failover remain unchanged. Excel handles supported models only (6-sol / 6-astra / 5.6-sol / terra / luna). Other models, including gpt-5.5, and API key accounts remain native. Supports Responses / Chat / Messages. Saved independently.' }}</p>
    <div class="mt-3 flex items-center gap-3">
      <select v-model="mode" class="input" :disabled="busy || !loaded" aria-label="OpenAI transport">
        <option value="native">{{ zh ? '原生（默认）' : 'Native (default)' }}</option>
        <option value="excel" :disabled="!available">Excel / Basispoints</option>
      </select>
      <button type="button" class="btn btn-secondary whitespace-nowrap" :disabled="busy || !loaded || mode === saved" @click="save">{{ zh ? '保存通道' : 'Save transport' }}</button>
    </div>
    <p v-if="loaded && !available" class="mt-2 text-xs text-amber-600">{{ zh ? '服务端尚未配置 Excel 私有通道。' : 'Private Excel transport is not configured.' }}</p>
    <p v-if="error" role="alert" class="mt-2 text-xs text-red-600">{{ zh ? '无法读取或保存通道设置。' : 'Unable to load or save transport.' }}</p>
    <p v-if="notice" role="status" class="mt-2 text-xs text-green-600">{{ zh ? '已保存，新请求立即生效。' : 'Saved. Effective for new requests.' }}</p>
  </section>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api/client'
const props = defineProps<{ groupId: number }>()
const { locale } = useI18n()
const zh = computed(() => locale.value.startsWith('zh'))
type Mode = 'native' | 'excel'
const mode = ref<Mode>('native'), saved = ref<Mode>('native')
const busy = ref(false), loaded = ref(false), available = ref(false), notice = ref(false), error = ref(false)
let generation = 0
watch(() => props.groupId, async id => {
  const current = ++generation
  busy.value = true; loaded.value = false; notice.value = false; error.value = false
  try {
    const { data } = await apiClient.get<{ mode: Mode; available: boolean }>(`/admin/groups/${id}/excel-mode`)
    if (generation !== current) return
    mode.value = saved.value = data.mode; available.value = data.available; loaded.value = true
  } catch { if (generation === current) error.value = true }
  finally { if (generation === current) busy.value = false }
}, { immediate: true })
async function save() {
  if (busy.value || !loaded.value || (mode.value === 'excel' && !available.value)) return
  const current = generation, id = props.groupId, wanted = mode.value
  busy.value = true; error.value = false; notice.value = false
  try {
    const { data } = await apiClient.put<{ mode: Mode; available: boolean }>(`/admin/groups/${id}/excel-mode`, { mode: wanted })
    if (generation !== current) return
    mode.value = saved.value = data.mode; available.value = data.available; notice.value = true
  } catch { if (generation === current) error.value = true }
  finally { if (generation === current) busy.value = false }
}
onBeforeUnmount(() => { generation++ })
</script>
