<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NButton, NFormItem, NInput, NSelect } from 'naive-ui'
const props=defineProps<{accountScope?:boolean;devices?:any[]}>()
const model=defineModel<any>()
const mode=computed({get:()=>model.value?.mode||'inherit',set:(value:string)=>{model.value=value==='inherit'?null:{id:'',name:'',client:'',version:'',groups:[],exceptions:[],...model.value,mode:value}}})
const modes=computed(()=>[{label:props.accountScope?'沿用源站默认':'兼容默认（保持已有登录方式）',value:'inherit'},{label:'原始标识 · 保留播放器身份',value:'original'},{label:'稳定标识 · 每个设备独立生成',value:'stable'},{label:'自定义标识 · 此账号与源站共用一个身份',value:'custom'},{label:'身份分组 · 每个组使用独立身份',value:'grouped'}])
const groupOptions=computed(()=>model.value?.groups?.map((g:any)=>({label:g.name||g.key,value:g.key}))||[])
const deviceOptions=computed(()=>Array.from(new Map((props.devices||[]).map((d:any)=>[d.identity.original.id,{label:(d.identity.original.name||d.identity.original.client||'未命名设备')+' · '+d.identity.original.id,value:d.identity.original.id}])).values()))
function addGroup(){(model.value.groups||=[]).push({key:'group-'+Date.now().toString(36),id:'',name:'',devices:[]})}
function addException(){(model.value.exceptions||=[]).push({device:'',mode:'original',id:'',name:'',group:''})}
</script>
<template>
 <div class="device-policy-editor">
  <n-form-item label="设备身份模式"><n-select v-model:value="mode" :options="modes" /></n-form-item>
  <n-alert type="info" :show-icon="false">设置仅用于通过访问账号登录的播放器。更改后重新登录生效，已有会话保持登录时的身份；不同账号和源站的生成标识彼此独立。</n-alert>
  <template v-if="model && mode!=='original'">
   <div class="device-field-grid">
    <n-form-item v-if="mode==='custom'" label="自定义标识种子（留空自动生成固定身份）"><n-input v-model:value="model.id" maxlength="128" placeholder="例如 living-room" /></n-form-item>
    <n-form-item label="设备名称（留空保留播放器名称）"><n-input v-model:value="model.name" maxlength="128" placeholder="我的媒体终端" /></n-form-item>
   </div>
   <p class="device-hint">标识种子不会原样发给源站，会结合账号与源站生成实际设备 ID。稳定标识以播放器提供的设备 ID 为依据；重装后 ID 变化时，可手动分配到原身份组。</p>
   <details class="device-advanced"><summary>高级客户端字段</summary><div class="device-field-grid"><n-form-item label="客户端名称（留空保留真实名称）"><n-input v-model:value="model.client" maxlength="128" /></n-form-item><n-form-item label="客户端版本（留空保留真实版本）"><n-input v-model:value="model.version" maxlength="128" /></n-form-item></div><p class="device-hint">只调整身份字段，不修改播放器的编码、转码或媒体能力。</p></details>
  </template>
  <template v-if="model">
   <div class="device-section-heading"><h3>身份分组</h3><n-button size="small" :disabled="(model.groups?.length||0)>=32" @click="addGroup">添加组</n-button></div>
   <p class="device-hint">分组模式下，未分组设备使用独立稳定标识。使用原始标识或稳定标识的设备也可通过例外指定身份组。</p>
   <article v-for="(group,index) in model.groups" :key="index" class="device-rule">
    <div class="device-section-heading"><b>{{group.name||'身份组 '+(Number(index)+1)}}</b><n-button size="small" quaternary type="error" @click="model.groups.splice(index,1)">移除</n-button></div>
    <div class="device-field-grid"><n-form-item label="组标识（保存后保持不变）"><n-input v-model:value="group.key" maxlength="64" /></n-form-item><n-form-item label="组设备名称"><n-input v-model:value="group.name" maxlength="128" placeholder="客厅设备" /></n-form-item><n-form-item label="组标识种子（可选）"><n-input v-model:value="group.id" maxlength="128" /></n-form-item></div>
    <n-form-item label="组内原始设备 ID（可选择已登录设备，或输入新 ID）"><n-select :value="group.devices" multiple filterable tag :options="deviceOptions" @update:value="group.devices=$event" /></n-form-item>
   </article>
   <div class="device-section-heading"><h3>特定设备例外</h3><n-button size="small" :disabled="(model.exceptions?.length||0)>=128" @click="addException">添加例外</n-button></div>
   <article v-for="(rule,index) in model.exceptions" :key="index" class="device-rule">
    <div class="device-section-heading"><b>设备例外 {{Number(index)+1}}</b><n-button size="small" quaternary type="error" @click="model.exceptions.splice(index,1)">移除</n-button></div>
    <n-form-item label="原始设备 ID（精确匹配）"><n-input v-model:value="rule.device" maxlength="512" /></n-form-item>
    <n-form-item label="此设备的模式"><n-select v-model:value="rule.mode" :options="modes.filter(m=>m.value!=='inherit')" /></n-form-item>
    <n-form-item v-if="rule.mode==='grouped'" label="身份组"><n-select v-model:value="rule.group" :options="groupOptions" /></n-form-item>
    <div v-if="rule.mode!=='original'" class="device-field-grid"><n-form-item v-if="rule.mode==='custom'" label="设备标识种子"><n-input v-model:value="rule.id" maxlength="128" /></n-form-item><n-form-item label="设备名称（可选）"><n-input v-model:value="rule.name" maxlength="128" /></n-form-item></div>
   </article>
  </template>
  <n-alert v-if="mode==='custom'||mode==='grouped'||model?.exceptions?.some((e:any)=>e.mode==='custom'||e.mode==='grouped')" type="warning" :show-icon="false">多个播放器共用身份时，源站可能将其识别为同一设备，影响播放进度、会话识别和远程控制。先用少量设备验证兼容性。</n-alert>
 </div>
</template>
<style scoped>
.device-field-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:0 18px}.device-policy-editor>.n-alert{margin-bottom:20px}.device-hint{font-size:12px;line-height:1.8;color:var(--muted)}.device-section-heading{display:flex;justify-content:space-between;align-items:center;gap:12px;margin:24px 0 12px}.device-section-heading h3{font-size:15px;margin:0}.device-section-heading b{font-size:13px}.device-rule{padding:18px;border:1px solid var(--line);border-radius:12px;margin:12px 0;background:var(--surface-soft)}.device-rule .device-section-heading{margin:0 0 16px}.device-advanced{border:1px solid var(--line);border-radius:10px;padding:14px;margin:18px 0}.device-advanced summary{cursor:pointer;font-size:13px;color:var(--muted)}.device-advanced .device-field-grid{margin-top:16px}@media(max-width:650px){.device-field-grid{grid-template-columns:1fr}.device-rule{padding:14px}}
</style>
