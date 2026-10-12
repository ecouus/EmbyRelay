<script setup lang="ts">
import {requestAPI} from './api'
import {computed,onMounted,onUnmounted,ref,watch} from 'vue'
import {NAlert,NButton,NInput,NTag} from 'naive-ui'
interface Address {route_id:string;route_alias:string;path:string;origin:string;node_id:string;entry_label:string;available:boolean;unavailable_reason?:string}
interface Source {id:string;name:string;base_url:string;enabled:boolean;owner_account_id:string;owner_username:string;owner_enabled:boolean;has_credentials:boolean;source_username_masked:string;source_password_masked:string;addresses:Address[]}
const props=defineProps<{accountId?:string;compact?:boolean}>()
const sources=ref<Source[]>([]),search=ref(''),busy=ref(false),error=ref(''),notice=ref('')
let timer:ReturnType<typeof setInterval>|undefined
let sequence=0
const filtered=computed(()=>{const q=search.value.trim().toLocaleLowerCase();return sources.value.filter(s=>!q||[s.name,s.base_url,s.owner_username].some(v=>v.toLocaleLowerCase().includes(q)))})
const addressURL=(a:Address)=>a.path?(a.origin||window.location.origin).replace(/\/$/,'')+a.path:''
async function refresh(){
 const request=++sequence;busy.value=true;error.value=''
 try{
  const {response,data}=await requestAPI('/api/v1/user-sources'+(props.accountId?'?account_id='+encodeURIComponent(props.accountId):''))
  if(!response.ok)throw Error(data.error||'源站读取失败')
  if(request===sequence)sources.value=data
 }catch(e){if(request===sequence)error.value=e instanceof Error?e.message:'源站读取失败'}finally{if(request===sequence)busy.value=false}
}
async function copyAddress(address:Address){
 try{await navigator.clipboard.writeText(addressURL(address));notice.value='中转地址已复制'}catch{error.value='复制失败，请选中中转地址手动复制'}
}
watch(()=>props.accountId,()=>{sources.value=[];notice.value='';search.value='';refresh()})
onMounted(()=>{refresh();timer=setInterval(()=>{if(!busy.value&&!document.hidden)refresh()},15000)})
onUnmounted(()=>{clearInterval(timer);sequence++;sources.value=[]})
</script>
<template>
<section class="panel user-sources" :class="{'compact':compact}">
 <div class="panel-heading"><div><h3>{{compact?'用户添加的 Emby':'用户源站'}}</h3><span>{{filtered.length}} / {{sources.length}} 个源站 · 每 15 秒刷新</span></div><n-button size="small" :loading="busy" @click="refresh">刷新源站</n-button></div>
 <p class="source-note">查看用户保存的源站与中转地址。源站用户名、密码均脱敏展示，凭据加密存储。</p>
 <div v-if="!compact" class="source-search"><n-input v-model:value="search" clearable placeholder="搜索用户、源站名称或地址" /></div>
 <n-alert v-if="error" type="error" closable @close="error=''">{{error}}</n-alert><n-alert v-if="notice" type="success" closable @close="notice=''">{{notice}}</n-alert>
 <div class="source-grid"><article v-for="source in filtered" :key="source.id" class="source-card">
  <div class="source-title"><h3>{{source.name}}</h3><n-tag size="small" :type="source.enabled?'success':'default'">{{source.enabled?'已启用':'已停用'}}</n-tag></div>
  <dl class="source-fields"><dt>所属用户</dt><dd>{{source.owner_username}}<small v-if="!source.owner_enabled">用户账号不可用</small></dd><dt>源站地址</dt><dd>{{source.base_url}}</dd><dt>源站用户名</dt><dd class="credential-mask">{{source.source_username_masked||'未保存'}}</dd><dt>源站密码</dt><dd class="credential-mask">{{source.source_password_masked||'未保存'}}<small>{{source.has_credentials?'已加密存储':'未保存凭据'}}</small></dd></dl>
  <div class="source-addresses"><h4>中转地址</h4><div v-for="address in source.addresses" :key="address.route_id+address.path" class="source-address">
   <span class="entry-label">{{address.entry_label}} · {{address.route_alias}}</span>
   <code v-if="address.path">{{addressURL(address)}}</code><span v-else>地址不可用</span>
   <div class="address-actions"><span v-if="!address.available" class="unavailable">{{address.unavailable_reason}}</span><n-button size="small" :disabled="!address.available" @click="copyAddress(address)">复制中转地址</n-button></div>
  </div><p v-if="!source.addresses.length" class="source-note">没有绑定到该用户的中转地址。</p></div>
 </article></div>
 <p v-if="!busy&&!filtered.length" class="source-note">{{search?'没有匹配的用户源站。':'用户尚未添加 Emby 源站。'}}</p>
</section>
</template>
<style scoped>
.source-note{padding:0 20px;color:var(--muted);font-size:13px;line-height:1.7}.source-search{padding:0 20px 16px;max-width:520px}.source-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,380px),1fr));gap:16px;padding:20px}.source-card{border:1px solid var(--line);border-radius:12px;padding:20px;min-width:0}.source-title{display:flex;align-items:center;justify-content:space-between;gap:12px}.source-title h3{margin:0;overflow-wrap:anywhere}.source-fields{display:grid;grid-template-columns:95px minmax(0,1fr);gap:14px;font-size:13px;line-height:1.6;margin:20px 0}.source-fields dt,.entry-label{color:var(--muted)}.source-fields dd{margin:0;overflow-wrap:anywhere}.source-fields small{display:block;font-size:12px;color:var(--muted);margin-top:3px}.credential-mask{letter-spacing:2px}.credential-mask small{letter-spacing:normal}.source-addresses{border-top:1px solid var(--line);padding-top:16px}.source-addresses h4{margin:0 0 12px;font-size:14px}.source-address{padding:12px;border-radius:8px;background:var(--soft);margin-top:8px;min-width:0}.entry-label{display:block;font-size:12px;margin-bottom:8px}.source-address code{display:block;font-size:12px;line-height:1.7;overflow-wrap:anywhere}.address-actions{display:flex;justify-content:space-between;align-items:center;gap:10px;margin-top:12px;flex-wrap:wrap}.unavailable{font-size:12px;color:var(--muted)}.compact .source-grid{grid-template-columns:1fr}.user-sources>.n-alert{margin:12px 20px}
</style>
