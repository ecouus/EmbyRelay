<script setup lang="ts">
import {computed,onMounted,onUnmounted,ref,watch} from 'vue'
import {NAlert,NButton,NSelect,NSpin} from 'naive-ui'
import DevicePolicyEditor from './DevicePolicyEditor.vue'
import DeviceRecords from './DeviceRecords.vue'
const props=defineProps<{upstreams:any[];routes:any[]}>()
const emit=defineEmits(['changed'])
const source=ref(props.upstreams[0]?.id||''),account=ref(''),accounts=ref<any[]>([]),policy=ref<any>(null),defaultPolicy=ref<any>(null),records=ref<any[]>([]),busy=ref(false),ready=ref(false),error=ref(''),notice=ref('')
const sourceOptions=computed(()=>props.upstreams.map(u=>({label:u.name,value:u.id})))
const accountOptions=computed(()=>[{label:'源站默认 · 所有关联账号',value:''},...accounts.value.filter(a=>props.upstreams.find(u=>u.id===source.value)?.owner_account_id===a.account.id || props.routes.some(r=>r.id===a.account.route_id&&r.upstream_id===source.value)).map(a=>({label:'账号覆盖 · '+a.account.username,value:a.account.id}))])
const defaultModeLabel=computed(()=>({original:'原始标识',stable:'稳定标识',custom:'自定义标识',grouped:'身份分组'} as Record<string,string>)[defaultPolicy.value?.mode]||'兼容默认')
const endpoint=()=>account.value?'access-accounts/'+account.value+'/device-policy?upstream_id='+encodeURIComponent(source.value):'upstreams/'+source.value+'/device-policy'
async function api(path:string,method='GET',body?:any){const r=await fetch('/api/v1/'+path,{method,headers:{'Content-Type':'application/json','X-Requested-With':'EmbyRelay'},body:body===undefined?undefined:JSON.stringify(body)});const data=await r.json();if(!r.ok)throw Error(data.error||'请求失败');return data}
async function run(fn:()=>Promise<void>){busy.value=true;error.value='';try{await fn()}catch(e){error.value=e instanceof Error?e.message:String(e)}finally{busy.value=false}}
async function refreshRecords(){const selected=source.value+':'+account.value;const data=await api('device-records?upstream_id='+encodeURIComponent(source.value)+(account.value?'&account_id='+encodeURIComponent(account.value):''));if(selected===source.value+':'+account.value)records.value=data}
async function load(){ready.value=false;notice.value='';if(!source.value)return;const [data,list]=await Promise.all([api(endpoint()),api('access-accounts')]);accounts.value=list;policy.value=data.policy;defaultPolicy.value=data.default_policy;await refreshRecords();ready.value=true}
async function save(){await run(async()=>{await api(endpoint(),'PATCH',{policy:policy.value});emit('changed');await load();notice.value='已保存。新登录使用当前设置，已有会话保持原身份。'})}
async function restore(){policy.value=null;await save()}
watch(source,()=>{account.value='';run(load)})
watch(account,()=>{if(!busy.value)run(load)})
watch(()=>props.upstreams,()=>{if(!props.upstreams.some(u=>u.id===source.value))source.value=props.upstreams[0]?.id||''})
let timer:ReturnType<typeof setInterval>|undefined
onMounted(()=>{run(load);timer=setInterval(()=>{if(!busy.value&&ready.value)refreshRecords().catch(e=>error.value=e.message)},15000)})
onUnmounted(()=>clearInterval(timer))
</script>
<template>
 <div class="device-management">
  <n-alert v-if="error" type="error" closable @close="error=''">{{error}}</n-alert><n-alert v-if="notice" type="success" closable @close="notice=''">{{notice}}</n-alert>
  <section class="panel device-settings"><div class="panel-heading"><div><h3>设备身份设置</h3><p>为源站设置默认身份规则，或为指定访问账号单独覆盖。</p></div><n-button :loading="busy" @click="run(load)">重新读取</n-button></div>
   <div class="device-settings-body"><div class="device-selectors"><div><label>Emby 源站</label><n-select v-model:value="source" :options="sourceOptions" :disabled="busy" placeholder="请选择源站" /></div><div><label>配置范围</label><n-select v-model:value="account" :options="accountOptions" :disabled="busy||!source" /></div></div>
    <p v-if="!source" class="device-empty">先添加 Emby 源站，再配置设备身份。</p>
    <n-spin v-else :show="busy"><template v-if="ready"><p v-if="account" class="device-hint">源站默认：{{defaultModeLabel}}。选择“沿用源站默认”可移除该账号的覆盖。</p><DevicePolicyEditor v-model="policy" :account-scope="!!account" :devices="records" /><div class="device-save"><n-button type="primary" :loading="busy" @click="save">保存设置</n-button><n-button :disabled="busy" @click="restore">{{account?'恢复源站默认':'恢复默认设置'}}</n-button></div></template></n-spin>
   </div>
  </section>
  <section class="panel"><div class="panel-heading"><div><h3>设备与登录映射</h3><p>最近 200 个设备记录 · 每 15 秒刷新</p></div><n-button :disabled="!source" :loading="busy" @click="run(refreshRecords)">刷新记录</n-button></div><DeviceRecords :records="records" :accounts="accounts" :upstreams="upstreams" /></section>
 </div>
</template>
<style scoped>
.device-management>.n-alert{margin-bottom:18px}.panel-heading p{font-size:12px;color:var(--muted);margin:6px 0 0}.device-settings-body{padding:0 24px 24px}.device-selectors{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:20px;margin-bottom:26px}.device-selectors label{display:block;font-size:12px;color:var(--muted);margin-bottom:8px}.device-save{display:flex;gap:12px;flex-wrap:wrap;margin-top:24px;padding-top:20px;border-top:1px solid var(--line)}.device-hint,.device-empty{font-size:12px;color:var(--muted);line-height:1.8}.device-empty{padding:24px;text-align:center}@media(max-width:650px){.device-selectors{grid-template-columns:1fr;gap:14px}.device-settings-body{padding:0 18px 18px}}
</style>
