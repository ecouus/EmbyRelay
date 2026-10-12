<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NButton, NAlert } from 'naive-ui'
const props = defineProps<{ text: string }>()
const copied = ref(''), error = ref('')
watch(() => props.text, () => { copied.value = ''; error.value = '' })
const fields = computed(() => {
  const result: { label: string; value: string; copyable: boolean }[] = []
  for (const line of props.text.split('\n').filter(Boolean)) {
    const split = line.indexOf('：')
    if (split < 0 && /^https?:\/\//.test(line) && result.length) {
      result.push({ label: '备用中转地址', value: line, copyable: true })
      continue
    }
    const label = split < 0 ? '使用说明' : line.slice(0, split)
    const value = split < 0 ? line : line.slice(split + 1)
    const instruction = /密码/.test(label) && /^(使用|（不保存|不保存)/.test(value)
    result.push({ label, value, copyable: !instruction && /地址|用户名|密码|用户中心/.test(label) && !!value })
  }
  return result
})
async function copy(value: string, label: string) {
  error.value = ''; copied.value = ''
  try { await navigator.clipboard.writeText(value); copied.value = label }
  catch { error.value = '复制失败，请选中下方内容手动复制。' }
}
</script>
<template>
  <div class="import-details">
    <div class="import-intro"><span class="import-badge">播放器配置</span><p>在播放器中添加服务器，按下方字段填写。点击复制即可直接粘贴。</p></div>
    <div v-for="(field,index) in fields" :key="index" class="import-field" :class="{'import-field-primary':field.label.includes('中转')}">
      <div class="import-field-heading"><span>{{field.label}}</span><n-button v-if="field.copyable" size="small" secondary @click="copy(field.value,String(index))">{{copied===String(index)?'已复制 ✓':'复制'}}</n-button></div>
      <div class="import-value">{{field.value}}</div>
    </div>
    <n-alert v-if="error" type="error">{{error}}</n-alert>
    <div class="import-footer"><n-button type="primary" @click="copy(text,'all')">{{copied==='all'?'全部已复制 ✓':'一键复制全部信息'}}</n-button><span role="status" aria-live="polite">{{copied ? '已复制到剪贴板' : '仅分享给账号使用者'}}</span></div>
  </div>
</template>
<style scoped>
.import-intro p{color:var(--muted);font-size:13px;line-height:1.8;margin:12px 0 20px}.import-badge{display:inline-block;padding:4px 10px;border-radius:6px;background:var(--accent-soft);color:var(--accent);font-size:12px;font-weight:600}.import-field{padding:14px 16px;margin:10px 0;border:1px solid var(--line);border-radius:10px;background:var(--surface-soft)}.import-field-primary{border-color:var(--accent);background:var(--accent-soft)}.import-field-heading{display:flex;align-items:center;justify-content:space-between;gap:12px;color:var(--muted);font-size:12px;font-weight:600;margin-bottom:8px}.import-value{color:var(--text);font-size:14px;line-height:1.7;white-space:pre-wrap;overflow-wrap:anywhere;user-select:text}.import-field-primary .import-value{font-family:ui-monospace,monospace;font-weight:600}.import-footer{display:flex;gap:12px;align-items:center;flex-wrap:wrap;padding-top:16px}.import-footer span{font-size:12px;color:var(--muted)}
</style>
