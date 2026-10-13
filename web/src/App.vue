<script setup lang="ts">
import { requestAPI, AUTH_EXPIRED_EVENT, loginPath } from './api'

import BrandLogo from './BrandLogo.vue'
import UiIcon from './UiIcon.vue'
import DeviceManagement from './DeviceManagement.vue'
import ImportDetails from './ImportDetails.vue'
import UserPortal from './UserPortal.vue'
import UserSources from './UserSources.vue'
import ResponsiveTable from './ResponsiveTable.vue'
const portalMode = ref(false)
const loginIdentity = ref(new URLSearchParams(window.location.search).get('portal') === '1' ? 'subscriber' : 'auto')
watch(loginIdentity, () => { resetLogin() })
const mobileNav = ref(false)
watch(portalMode, () => { mobileNav.value = false })
import AccessSubscriptions from './AccessSubscriptions.vue'
import type { DataTableInst } from 'naive-ui'
import { computed, h, onMounted, onUnmounted, ref, watch } from 'vue'
import { darkTheme, zhCN, dateZhCN, NDrawer, NDrawerContent, NQrCode, NConfigProvider, NButton, NInput, NInputNumber, NSelect, NSwitch, NModal, NForm, NFormItem, NDataTable, NTag, NAlert, NSpin } from 'naive-ui'
interface Upstream { id: string; name: string; base_url: string; enabled: boolean; owner_account_id?: string; mode: string; allowed_cidrs: string[] | null; redirect_hosts: string[] | null }
interface Route { id: string; alias: string; upstream_id: string; relay_id: string; enabled: boolean; rate_bps: number; daily_quota: number; max_concurrent: number }
interface Node { id: string; name: string; endpoint: string; enabled: boolean; last_seen: number; active_sessions: number }
interface Traffic { date: string; route_id: string; ingress_bytes: number; egress_bytes: number }
const dark = ref((()=>{try{const saved=localStorage.getItem('embyrelay-theme');if(saved)return saved==='dark'}catch{}return window.matchMedia('(prefers-color-scheme: dark)').matches})())
watch(dark,value=>{document.documentElement.dataset.theme=value?'dark':'light';try{localStorage.setItem('embyrelay-theme',value?'dark':'light')}catch{}},{immediate:true})
const origin = window.location.origin
const me = ref<{username:string;role:string;totp_enabled:boolean}|null>(null)
const page = ref('overview'), busy = ref(false), error = ref(''), notice = ref(''), ready = ref(false)
const username = ref(''), password = ref(''), code = ref(''), totpRequired = ref(false)
const upstreams = ref<Upstream[]>([]), routes = ref<Route[]>([]), nodes = ref<Node[]>([]), traffic = ref<Traffic[]>([])
const adminUpstreams = computed(()=>upstreams.value.filter(source=>!source.owner_account_id))
const userUpstreams = computed(()=>upstreams.value.filter(source=>!!source.owner_account_id))
const sessions = ref<any[]>([]), requests = ref<any[]>([]), diagnostics = ref<any[]>([]), audit = ref<any[]>([]), users = ref<any[]>([]), overview = ref<any>({})
const modal = ref(false), kind = ref(''), edit = ref<any>({}), keys = ref<any[]>([]), keyRoute = ref<Route|null>(null), keyExpiry = ref(30)
const importInfo = ref<any>(null)
watch(modal,value=>{if(!value){edit.value.emby_username='';edit.value.emby_password=''}})
const secret = ref(''), enrollment = ref<any>(null), totpURI = ref(''), totpSecret = ref(''), totpPassword = ref(''), totpCode = ref('')
const currentPassword = ref(''), nextPassword = ref('')
let timer: ReturnType<typeof setInterval> | undefined
const nav = [{id:'overview',label:'运行概览'},{id:'upstreams',label:'管理员源站'},{id:'user-sources',label:'用户源站'},{id:'routes',label:'代理路由'},{id:'relays',label:'中转节点'},{id:'subscriptions',label:'访问账号与套餐'},{id:'devices',label:'设备身份管理'},{id:'sessions',label:'播放会话'},{id:'traffic',label:'流量统计'},{id:'diagnostics',label:'请求诊断'},{id:'audit',label:'审计日志'},{id:'settings',label:'账户与安全'}]
const navGroups = [
 {id:'overview',label:'运行概览',pages:['overview']},
 {id:'upstreams',label:'源站管理',pages:['upstreams','user-sources']},
 {id:'routes',label:'中转配置',pages:['routes','relays']},
 {id:'subscriptions',label:'用户管理',pages:['subscriptions','devices']},
 {id:'sessions',label:'运行监控',pages:['sessions','traffic']},
 {id:'diagnostics',label:'问题排查',pages:['diagnostics','audit']},
 {id:'settings',label:'账户与安全',pages:['settings']}
]
const visibleNavGroups = computed(()=>navGroups.filter(group=>admin.value || group.id!=='subscriptions'))
const currentGroup = computed(()=>navGroups.find(group=>group.pages.includes(page.value)))
const currentTabs = computed(()=>currentGroup.value?.pages.filter(id=>admin.value || id!=='user-sources') || [])
const title = computed(()=>currentGroup.value?.label)
try { const saved = sessionStorage.getItem('embyrelay-page'); if (saved && nav.some(item=>item.id===saved)) page.value=saved } catch {}
watch(page,value=>{ try { sessionStorage.setItem('embyrelay-page',value) } catch {} })
function switchTab(event: KeyboardEvent, id: string) {
 if (!['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) return
 event.preventDefault()
 const index = currentTabs.value.indexOf(id)
 const next = event.key==='Home' ? 0 : event.key==='End' ? currentTabs.value.length-1 : (index+(event.key==='ArrowRight'?1:-1)+currentTabs.value.length)%currentTabs.value.length
 page.value = currentTabs.value[next]
 const buttons = (event.currentTarget as HTMLElement).parentElement?.querySelectorAll<HTMLButtonElement>('[role="tab"]')
 buttons?.[next]?.focus()
}
const admin = computed(()=>me.value?.role==='admin')
const bytes = (n:number=0)=>{ if(n<1024)return `${n} B`; const i=Math.min(Math.floor(Math.log(n)/Math.log(1024)),4); return `${(n/1024**i).toFixed(1)} ${['B','KiB','MiB','GiB','TiB'][i]}` }
const date = (v:number)=>v?new Date(v*1000).toLocaleString('zh-CN'):'—'
const routeName = (id:string)=>routes.value.find(r=>r.id===id)?.alias||id.slice(0,10)
const online = (n:Node)=>n.enabled && Date.now()/1000-n.last_seen<60
const totalEgress = computed(()=>traffic.value.reduce((n,t)=>n+t.egress_bytes,0))
const todayEgress = computed(()=>traffic.value.filter(t=>t.date===new Date().toISOString().slice(0,10)).reduce((n,t)=>n+t.egress_bytes,0))
const barData = computed(()=>{const groups:Record<string,number>={};traffic.value.forEach(t=>groups[t.date]=(groups[t.date]||0)+t.egress_bytes);return Object.entries(groups).sort().slice(-14)})
const theme = computed(()=>({common:{primaryColor:dark.value?'#2dd4bf':'#0d9488',primaryColorHover:dark.value?'#5eead4':'#0f766e',primaryColorPressed:'#115e59',borderRadius:'12px',fontSize:'14px',fontFamily:'system-ui, -apple-system, "Segoe UI", "PingFang SC", sans-serif',bodyColor:dark.value?'#020617':'#f8fafc',cardColor:dark.value?'#0f172a':'#ffffff',modalColor:dark.value?'#0f172a':'#ffffff',tableColor:dark.value?'#0f172a':'#ffffff',inputColor:dark.value?'#1e293b':'#ffffff',borderColor:dark.value?'#253247':'#e2e8f0',textColorBase:dark.value?'#f1f5f9':'#0f172a'},DataTable:{thColor:dark.value?'#172033':'#f8fafc',tdColorHover:dark.value?'#172033':'#f8fafc',thFontWeight:'600',tdPaddingMedium:'16px 20px',thPaddingMedium:'13px 20px'},Button:{fontWeight:'500'},Card:{borderRadius:'16px'}}))
async function api(path:string,method='GET',body?:unknown){const {response:r,data:data}=await requestAPI('/api/v1/'+path,method,body);if(!r.ok){throw new Error(data.error||'请求失败')};return data}
async function run(fn:()=>Promise<void>){error.value='';busy.value=true;try{await fn()}catch(e){error.value=e instanceof Error?e.message:'请求失败'}finally{busy.value=false}}
async function refresh(){if(!me.value)return; const result=await Promise.all(['overview','upstreams','routes','relays','sessions','traffic','audit','diagnostics'].map(p=>api(p==='diagnostics'&&diagnosticNode.value?p+'?node_id='+encodeURIComponent(diagnosticNode.value):p)));[overview.value,upstreams.value,routes.value,nodes.value,sessions.value,traffic.value,audit.value,diagnostics.value]=result;if(admin.value)users.value=await api('users')}
async function refreshCurrent(){
 if(!me.value)return
 const needed:Record<string,string[]>={overview:['overview','upstreams','routes','relays','sessions','traffic','audit','diagnostics'],upstreams:['upstreams'],routes:['routes','upstreams','relays'],relays:['relays'],sessions:['sessions','routes','relays'],traffic:['traffic','sessions','routes'],diagnostics:['diagnostics','relays'],audit:['audit'],devices:['upstreams','routes']}
 const selected=needed[page.value]||[]
 const result=await Promise.all(selected.map(name=>api(name==='diagnostics'&&diagnosticNode.value?name+'?node_id='+encodeURIComponent(diagnosticNode.value):name)))
 selected.forEach((name,index)=>{switch(name){case 'overview':overview.value=result[index];break;case 'upstreams':upstreams.value=result[index];break;case 'routes':routes.value=result[index];break;case 'relays':nodes.value=result[index];break;case 'sessions':sessions.value=result[index];break;case 'traffic':traffic.value=result[index];break;case 'audit':audit.value=result[index];break;case 'diagnostics':diagnostics.value=result[index]}})
 if(admin.value&&['audit','settings'].includes(page.value))users.value=await api('users')
}
async function login(){await run(async()=>{const result=await api(loginPath(loginIdentity.value),'POST',{username:username.value,password:password.value,code:totpRequired.value?code.value:''});if(result.totp_required){totpRequired.value=true;return};resetLogin();if(result.role==='subscriber'){portalMode.value=true;return};me.value=await api('auth/me');await refresh()})}
function resetLogin(){username.value='';password.value='';code.value='';totpRequired.value=false;error.value='';notice.value=''}
async function logout(){await run(async()=>{await api('auth/logout','POST');me.value=null;resetLogin();secret.value='';importInfo.value=null;keyRoute.value=null;enrollment.value=null})}
function open(type:string,row?:any){kind.value=type;edit.value=type==='upstream'?{name:'',base_url:'',enabled:true,mode:'proxy',allowed_cidrs:[],redirect_hosts:[],...row}:type==='route'?{alias:'',upstream_id:upstreams.value[0]?.id,relay_id:'',enabled:true,rate_bps:0,daily_quota:0,max_concurrent:100,...row}:{username:'',password:'',role:'operator',...row};if(type==='upstream'){edit.value.emby_username='';edit.value.emby_password='';edit.value.clear_credentials=false;edit.value.cidrs=(edit.value.allowed_cidrs??[]).join(', ');edit.value.hosts=(edit.value.redirect_hosts??[]).join(', ')};modal.value=true}
async function save(){await run(async()=>{const data={...edit.value};if(kind.value==='upstream'){data.allowed_cidrs=data.cidrs.split(',').map((s:string)=>s.trim()).filter(Boolean);data.redirect_hosts=data.hosts.split(',').map((s:string)=>s.trim()).filter(Boolean);delete data.cidrs;delete data.hosts;delete data.has_credentials;if(data.id&&!data.emby_username)delete data.emby_username;if(data.id&&!data.emby_password)delete data.emby_password};const resource=kind.value==='upstream'?'upstreams':kind.value==='route'?'routes':'users';const id=data.id;delete data.id;delete data.totp_enabled;await api(resource+(id?'/'+id:''),id?'PATCH':'POST',data);modal.value=false;edit.value={};notice.value='配置已保存并生效';await refresh()})}
async function probe(u:Upstream){await run(async()=>{const result=await api('upstreams/'+u.id+'/health');notice.value=result.healthy?`${u.name} 连接正常 · ${result.latency_ms} ms`:`${u.name} 检查失败 · HTTP ${result.status_code||'不可达'}`})}
async function remove(resource:string,id:string){if(!confirm('确认删除？关联路由请先解除。'))return;await run(async()=>{await api(resource+'/'+id,'DELETE');await refresh()})}
async function toggleNode(n:Node){await run(async()=>{await api('relays/'+n.id,'PATCH',{enabled:!n.enabled});await refresh()})}
async function manageKeys(r:Route){importInfo.value=null;keyRoute.value=r;secret.value='';await run(async()=>{keys.value=await api(`routes/${r.id}/keys`)})}
async function newKey(){await run(async()=>{const result=await api(`routes/${keyRoute.value!.id}/keys`,'POST',{expires_at:keyExpiry.value>0?Math.floor(Date.now()/1000)+keyExpiry.value*86400:0});secret.value=relayAddress(result.access_key);keys.value=await api(`routes/${keyRoute.value!.id}/keys`)})}
async function revokeKey(id:string){importInfo.value=null;if(!confirm('撤销后使用该密钥的播放地址将失效，确认撤销？'))return;await run(async()=>{await api('access-keys/'+id,'DELETE');keys.value=await api(`routes/${keyRoute.value!.id}/keys`)})}
function relayAddress(k:string){const route=keyRoute.value!;const node=nodes.value.find(n=>n.id===route.relay_id);return `${(node?.endpoint||origin).replace(/\/$/,'')}/r/${route.alias}/${k}/`}
async function showImport(k:string){const route=keyRoute.value!;importInfo.value=null;await run(async()=>{const data=await api('upstreams/'+route.upstream_id+'/credentials','POST',{});if(keyRoute.value?.id===route.id)importInfo.value={...data,relay_url:relayAddress(k)}})}
const importText=computed(()=>importInfo.value?`中转服务器地址：${importInfo.value.relay_url}\n源站地址：${importInfo.value.source_url}\n用户名：${importInfo.value.username}\n密码：${importInfo.value.password}`:'')
async function copy(v:string){await run(async()=>{await navigator.clipboard.writeText(v);notice.value='已复制'})}
const shellQuote = (value:string) => "'" + value.replace(/'/g, "'\"'\"'") + "'"
const agentInstallCommand = computed(()=>{
 if(!enrollment.value)return ''
 const args=['--control-url',origin,'--server-key',enrollment.value.server_key,'--token',enrollment.value.token,'--mode','nginx']
 return `( installer=$(mktemp) && trap 'rm -f "$installer"' EXIT && curl -fsSL https://raw.githubusercontent.com/ecouus/EmbyRelay/main/scripts/install-agent.sh -o "$installer" && sudo bash "$installer" ${args.map(shellQuote).join(' ')} )`
})
async function enroll(){await run(async()=>{enrollment.value=await api('relays/enroll','POST')})}
async function setupTOTP(){if(!totpPassword.value){error.value='请先输入当前密码';return};await run(async()=>{const result=await api('auth/totp','POST',{action:'setup',password:totpPassword.value});totpSecret.value=result.secret;totpURI.value=result.uri})}
async function changeTOTP(action:string){await run(async()=>{await api('auth/totp','POST',{action,password:totpPassword.value,secret:totpSecret.value,code:totpCode.value});totpPassword.value='';totpSecret.value='';totpURI.value='';totpCode.value='';me.value=null;notice.value='安全设置已更新，请重新登录'})}
async function changeOwnPassword(){await run(async()=>{await api('auth/password','POST',{current:currentPassword.value,next:nextPassword.value});currentPassword.value='';nextPassword.value='';me.value=null;notice.value='密码已修改，请重新登录'})}
function diagnosticDetails(r:any){try{return JSON.parse(r.details||'{}')}catch{return {}}}
const diagnosticNode=ref('')
const diagnosticNodeOptions=computed(()=>[{label:'全部节点',value:''},{label:'控制服务器',value:'local'},...nodes.value.map(n=>({label:n.name,value:n.id}))])
const diagnosticNodeName=(r:any)=>r.node_id?(nodes.value.find(n=>n.id===r.node_id)?.name||r.node_name||r.node_id):'控制服务器'
async function refreshDiagnostics(){diagnostics.value=await api('diagnostics'+(diagnosticNode.value?'?node_id='+encodeURIComponent(diagnosticNode.value):''))}
watch(diagnosticNode,()=>{run(refreshDiagnostics)})
const selectedDiagnostic=ref<any>(null)
const selectedDetails=computed(()=>selectedDiagnostic.value?diagnosticDetails(selectedDiagnostic.value):{})
const diagnosticColumns=[
 {title:'时间',key:'created_at',width:160,render:(r:any)=>date(r.created_at)},
 {title:'节点',key:'node_id',width:150,ellipsis:{tooltip:true},render:(r:any)=>diagnosticNodeName(r)},
 {title:'状态',key:'status',width:75,render:(r:any)=>h(NTag,{type:r.status>=500?'error':'warning',size:'small',bordered:false},()=>String(r.status))},
 {title:'原因',key:'reason',ellipsis:{tooltip:true}},
 {title:'详情',key:'actions',width:90,align:'center' as const,titleAlign:'center' as const,render:(r:any)=>h(NButton,{size:'small',type:'primary',secondary:true,onClick:()=>{error.value='';selectedDiagnostic.value=r}},()=> '查看详情')}
]
const actions=(items:{label:string;fn:()=>void;danger?:boolean;primary?:boolean}[])=>h('div',{class:'actions'},items.map(a=>h(NButton,{size:'small',quaternary:!a.primary,type:a.danger?'error':a.primary?'primary':'default',onClick:a.fn},()=>a.label)))
const status=(enabled:boolean)=>h(NTag,{size:'small',bordered:false,type:enabled?'success':'default'},()=>enabled?'已启用':'已停用')
const upstreamColumns=computed(()=>[{title:'源站',key:'name'},{title:'地址',key:'base_url'},{title:'状态',key:'enabled',render:(r:Upstream)=>status(r.enabled)},...(admin.value?[{title:'操作',key:'actions',render:(r:Upstream)=>actions([{label:'检测',fn:()=>probe(r)},{label:'编辑',fn:()=>open('upstream',r)},{label:'删除',danger:true,fn:()=>remove('upstreams',r.id)}])}]:[])])
const routeColumns=computed(()=>[{title:'路由别名',key:'alias'},{title:'源站',key:'upstream_id',render:(r:Route)=>upstreams.value.find(u=>u.id===r.upstream_id)?.name||'—'},{title:'每日配额',key:'daily_quota',render:(r:Route)=>r.daily_quota?bytes(r.daily_quota):'不限'},{title:'状态',key:'enabled',render:(r:Route)=>status(r.enabled)},...(admin.value?[{title:'操作',key:'actions',render:(r:Route)=>actions([{label:'访问地址',fn:()=>manageKeys(r)},{label:'编辑',fn:()=>open('route',r)},{label:'删除',danger:true,fn:()=>remove('routes',r.id)}])}]:[])])
const clientTraffic=computed(()=>{const groups=new Map<string,any>();for(const s of sessions.value){const key=JSON.stringify([s.client_ip,s.client_name]);const g=groups.get(key)||{ip:s.client_ip||'未知',app:s.client_name||'未提供',bytes:0,count:0};g.bytes+=s.bytes_sent||0;g.count++;groups.set(key,g)}return [...groups.values()].sort((a,b)=>b.bytes-a.bytes)})
const selectedSession=ref<any>(null)
const sessionStatus=(value:string)=>({active:'进行中',completed:'已结束',closed:'已结束',interrupted:'已中断',ended:'已结束',failed:'失败',error:'失败'}[value]||value||'未知')
const sessionNode=(id:string)=>id==='local'?'本机':nodes.value.find(n=>n.id===id)?.name||id||'未知'
const sessionColumns=[
 {title:'开始时间',key:'started_at',width:160,render:(r:any)=>date(r.started_at)},
 {title:'路由',key:'route_id',ellipsis:{tooltip:true},render:(r:any)=>routeName(r.route_id)},
 {title:'状态',key:'status',width:90,align:'center' as const,titleAlign:'center' as const,render:(r:any)=>h(NTag,{size:'small',bordered:false,type:r.status==='active'?'success':'default'},()=>sessionStatus(r.status))},
 {title:'详情',key:'detail',width:100,align:'center' as const,titleAlign:'center' as const,render:(r:any)=>h(NButton,{size:'small',type:'primary',secondary:true,onClick:()=>selectedSession.value=r},()=> '查看详情')}
]
const trafficColumns=[{title:'日期（UTC）',key:'date'},{title:'路由',key:'route_id',render:(r:Traffic)=>routeName(r.route_id)},{title:'源站接收',key:'ingress_bytes',render:(r:Traffic)=>bytes(r.ingress_bytes)},{title:'客户端发送',key:'egress_bytes',render:(r:Traffic)=>bytes(r.egress_bytes)}]
const auditColumns=[{title:'时间',key:'created_at',render:(r:any)=>date(r.created_at)},{title:'操作',key:'action'},{title:'操作者',key:'actor_id',render:(r:any)=>users.value.find(u=>u.id===r.actor_id)?.username||r.actor_id.slice(0,12)},{title:'对象',key:'target',ellipsis:{tooltip:true}}]
const userColumns=[{title:'用户名',key:'username'},{title:'角色',key:'role'},{title:'TOTP',key:'totp_enabled',render:(r:any)=>r.totp_enabled?'已开启':'未开启'},{title:'操作',key:'actions',render:(r:any)=>actions([{label:'编辑 / 重置密码',fn:()=>open('user',r)},{label:'删除',danger:true,fn:()=>remove('users',r.id)}])}]
function sessionExpired(event: Event) {
 const scope=(event as CustomEvent).detail?.scope
 if(scope==='portal') { if(portalMode.value){portalMode.value=false;resetLogin()} ;return }
 if(!me.value)return
 me.value=null;modal.value=false;edit.value={};secret.value='';importInfo.value=null;keyRoute.value=null;enrollment.value=null
 totpSecret.value='';totpURI.value='';totpPassword.value='';totpCode.value='';currentPassword.value='';nextPassword.value=''
 upstreams.value=[];routes.value=[];nodes.value=[];users.value=[];sessions.value=[];traffic.value=[];audit.value=[];diagnostics.value=[];overview.value={}
 error.value='登录已过期，请重新登录'
}
onMounted(async()=>{window.addEventListener(AUTH_EXPIRED_EVENT,sessionExpired);try{if(loginIdentity.value==='subscriber'){try{await api('portal/me');portalMode.value=true}catch{};return};me.value=await api('auth/me');await refresh()}catch(e){if(me.value)error.value=String(e);else{try{await api('portal/me');portalMode.value=true}catch{}}}finally{ready.value=true};timer=setInterval(()=>{if(me.value&&!busy.value&&!document.hidden)refreshCurrent().catch(e=>error.value=e.message)},15000)})
onUnmounted(()=>{clearInterval(timer);window.removeEventListener(AUTH_EXPIRED_EVENT,sessionExpired)})
</script>

<template>
<n-config-provider :theme="dark?darkTheme:null" :theme-overrides="theme" :locale="zhCN" :date-locale="dateZhCN">
  <UserPortal v-if="portalMode" @theme="dark=!dark" @logout="portalMode=false;resetLogin()" />
  <div v-else-if="!ready" class="loading"><n-spin /></div>
  <main v-else-if="!me" class="login-screen"><n-button class="login-theme" @click="dark=!dark" :aria-label="dark?'切换明亮模式':'切换暗色模式'">{{dark?'☀ 明亮':'☾ 暗色'}}</n-button>
    <div class="login-story"><div class="brand"><BrandLogo /> EmbyRelay</div><div><div class="eyebrow">YOUR MEDIA. YOUR NETWORK.</div><h1>每一段播放，<br>都有可靠的连接。</h1><p>管理 Emby 源站、连接中转节点，<br>让你的媒体服务始终井然有序。</p><div class="network-art"><i></i><span>EMBY</span><b>⟷</b><span class="art-center">RELAY</span><b>⟷</b><span>YOU</span></div></div><small>自托管 · 私有网络 · 安全传输</small></div>
    <form class="login-form" @submit.prevent="login"><div class="eyebrow">WELCOME TO EMBYRELAY</div><h2>欢迎回来</h2><p>选择登录身份，进入对应的工作空间。</p><n-alert v-if="error" type="error" :show-icon="false">{{error}}</n-alert><n-alert v-if="notice" type="success" :show-icon="false">{{notice}}</n-alert><template v-if="!totpRequired"><label>登录身份<n-select v-model:value="loginIdentity" :disabled="busy" :options="[{label:'自动识别 / 管理账号',value:'auto'},{label:'订阅用户中心',value:'subscriber'}]" /></label><label>用户名<n-input v-model:value="username" autocomplete="username" placeholder="输入用户名" /></label><label>密码<n-input v-model:value="password" type="password" show-password-on="click" autocomplete="current-password" placeholder="输入账户密码" /></label></template><label v-else>动态验证码 <small>账户已开启两步验证，请输入认证器中的验证码</small><n-input v-model:value="code" autocomplete="one-time-code" placeholder="6 位验证码" maxlength="6" /></label><n-button attr-type="submit" type="primary" size="large" block :loading="busy">{{totpRequired?'验证并登录 →':'登录 →'}}</n-button><n-button v-if="totpRequired" :disabled="busy" @click="resetLogin" block quaternary>返回账号密码登录</n-button><small class="login-foot">管理员管理服务，订阅用户查看额度与播放器配置。</small></form>
  </main>
  <div v-else class="shell">
    <aside class="sidebar"><div class="brand"><BrandLogo /> EmbyRelay</div><div class="nav-caption">工作空间</div><nav><button v-for="n in visibleNavGroups" :key="n.id" :class="{selected:currentGroup?.id===n.id}" @click="page=n.id;mobileNav=false" :aria-current="currentGroup?.id===n.id?'page':undefined"><UiIcon :name="n.id" />{{n.label}}<i v-if="currentGroup?.id===n.id">•</i></button></nav><div class="sidebar-bottom"><span class="status-dot"></span> 控制平面在线 <small>配置 v{{overview.version}}</small></div><div class="profile"><div class="avatar">{{me.username.slice(0,1).toUpperCase()}}</div><div class="profile-info"><b>{{me.username}}</b><small>{{admin?'管理员':'只读操作员'}}</small></div><button class="logout-button" title="退出登录" aria-label="退出登录" @click="logout"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 5H5v14h4M14 8l4 4-4 4M9 12h9" /></svg></button></div></aside>
    <n-drawer v-model:show="mobileNav" :width="300" placement="left" :auto-focus="true" :trap-focus="true"><div class="mobile-navigation"><div class="brand"><BrandLogo /> EmbyRelay<button class="mobile-close" aria-label="关闭导航" @click="mobileNav=false"><UiIcon name="close" /></button></div><nav><button v-for="n in visibleNavGroups" :key="n.id" :class="{selected:currentGroup?.id===n.id}" @click="page=n.id;mobileNav=false"><UiIcon :name="n.id" />{{n.label}}</button></nav><div class="mobile-account"><div class="avatar">{{me.username.slice(0,1).toUpperCase()}}</div><div class="profile-info"><b>{{me.username}}</b><small>{{admin?'管理员':'只读操作员'}}</small></div><n-button size="small" @click="mobileNav=false;logout()">退出</n-button></div></div></n-drawer><main class="workspace"><header><button class="mobile-menu" aria-label="打开导航" :aria-expanded="mobileNav" @click="mobileNav=true"><UiIcon name="menu" /></button><div class="breadcrumb">工作空间 <span>/</span> {{title}}</div><div class="header-meta"><n-button size="small" :aria-label="dark?'切换明亮模式':'切换暗色模式'" @click="dark=!dark">{{dark?'☀ 明亮':'☾ 暗色'}}</n-button><span v-if="page==='overview'" :title="origin" class="header-running"><span class="live-indicator"></span>实时运行中</span><span v-if="page==='overview'" class="header-origin">{{origin}}</span><span v-if="page!=='overview'">● LIVE</span><n-button size="small" :loading="busy" @click="run(refresh)">刷新数据</n-button></div></header>
      <div class="content"><h1 class="visually-hidden">{{title}}</h1><n-alert v-if="error" type="error" closable @close="error=''">{{error}}</n-alert><n-alert v-if="notice" type="success" closable @close="notice=''">{{notice}}</n-alert>
      <div v-if="currentTabs.length>1" class="section-tabs" role="tablist" :aria-label="title"><button v-for="id in currentTabs" :key="id" type="button" role="tab" :aria-selected="page===id" :tabindex="page===id?0:-1" :class="{active:page===id}" @keydown="switchTab($event,id)" @click="page=id">{{nav.find(item=>item.id===id)?.label}}</button></div>
      <div v-if="admin && ['upstreams','routes','relays'].includes(page)" class="page-actions"><n-button v-if="['upstreams','routes'].includes(page)" type="primary" @click="open(page==='upstreams'?'upstream':'route')">＋ {{page==='upstreams'?'添加源站':'新建路由'}}</n-button><n-button v-else type="primary" @click="enroll">＋ 注册中转节点</n-button></div>
      <template v-if="page==='overview'">

        <div class="stats dashboard-stats"><article class="metric-card metric-blue"><div class="metric-top"><span>已配置源站</span><i aria-hidden="true"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="7" rx="2" /><rect x="3" y="14" width="18" height="7" rx="2" /><path d="M7 6.5h.01M7 17.5h.01M11 6.5h6M11 17.5h6M7 10v4M17 10v4" /></svg></i></div><strong>{{upstreams.length}}<small>个</small></strong><p>管理员 {{adminUpstreams.length}} · 用户 {{userUpstreams.length}} · 已启用 {{upstreams.filter(u=>u.enabled).length}}</p></article><article class="metric-card metric-green"><div class="metric-top"><span>在线中转节点</span><i><UiIcon name="relays" /></i></div><strong>{{nodes.filter(online).length}}<small>/ {{nodes.length}}</small></strong><p>最近 60 秒内收到心跳</p></article><article class="metric-card metric-cyan"><div class="metric-top"><span>今日发送流量</span><i><UiIcon name="traffic" /></i></div><strong class="stat-bytes">{{bytes(todayEgress)}}</strong><p>UTC 自然日 · 下行统计</p></article><article class="metric-card metric-amber"><div class="metric-top"><span>当前代理请求</span><i><UiIcon name="sessions" /></i></div><strong>{{overview.active_requests||0}}<small>路</small></strong><p>控制服务器实时连接</p></article><article class="metric-card metric-purple"><div class="metric-top"><span>累计发送量</span><i><UiIcon name="overview" /></i></div><strong class="stat-bytes">{{bytes(totalEgress)}}</strong><p>记录内双向流量</p></article></div>
        <div class="trend-heading"><div><h2>数据趋势</h2><p>按自然日统计；时间范围以服务端时区为准</p></div><div class="trend-filters"><span>站点</span><b>全部路由⌄</b><span>时间</span><b>14 天⌄</b></div></div>
        <div class="trend-grid"><section class="panel trend-card"><div class="panel-heading"><div><h3>流量</h3><span>最近 14 个有数据的日期</span></div><strong>{{bytes(totalEgress)}}</strong></div><div v-if="barData.length" class="chart chart-bars"><div v-for="[d,n] in barData" :key="d" class="bar-column"><div :style="{height:Math.max(5,150*n/Math.max(...barData.map(x=>x[1]),1))+'px'}"></div><small>{{d.slice(5)}}</small></div></div><div v-else class="empty"><div><UiIcon name="traffic" /></div><h4>等待第一段播放</h4><p>通过代理路由访问媒体后，这里将显示真实流量。</p></div></section><section class="panel trend-card"><div class="panel-heading"><div><h3>请求</h3><span>控制服务器累计请求</span></div><strong>{{overview.requests||0}} 次</strong></div><div class="request-summary"><UiIcon name="traffic" /><strong>{{overview.requests||0}}<small>累计请求</small></strong></div><small class="chart-caption">累计计数 · 暂无历史趋势数据</small></section><section class="panel trend-card"><div class="panel-heading"><div><h3>连接概况</h3><span>实时节点与路由状态</span></div><strong>{{routes.filter(r=>r.enabled).length}} 路由</strong></div><div class="health-list"><div><span class="health-dot green"></span><span>在线中转节点</span><b>{{nodes.filter(online).length}} / {{nodes.length}}</b></div><div><span class="health-dot blue"></span><span>启用源站</span><b>{{upstreams.filter(u=>u.enabled).length}} / {{upstreams.length}}</b></div><div><span class="health-dot amber"></span><span>当前代理请求</span><b>{{overview.active_requests||0}}</b></div></div></section></div>
        <div class="dashboard-bottom"><section class="panel status-summary"><div class="panel-heading"><h3>最近会话</h3><n-button text @click="page='sessions'">查看全部 →</n-button></div><n-data-table :columns="sessionColumns" :data="sessions.slice(0,4)" :bordered="false" class="session-summary" :table-layout="'fixed'" /></section><section class="panel status-summary"><div class="panel-heading"><h3>系统状态</h3><span class="pill">正常</span></div><div class="system-status-row"><span>诊断记录</span><b>{{diagnostics.length}} 条</b></div><div class="system-status-row"><span>配置版本</span><b>v{{overview.version||'—'}}</b></div><div class="system-status-row"><span>控制平面</span><b class="text-success">在线</b></div></section></div>
        <div v-if="!upstreams.length" class="onboarding"><div><h3>从添加第一个 Emby 源站开始</h3><p>添加源站 → 创建代理路由 → 生成访问地址 → 使用播放器连接。</p></div><n-button v-if="admin" @click="page='upstreams';open('upstream')">添加源站 →</n-button></div>
      </template>
      <section v-if="page==='upstreams'" class="panel"><responsive-table :columns="upstreamColumns" :data="adminUpstreams" :bordered="false" /></section>
      <section v-if="page==='routes'" class="panel"><responsive-table :columns="routeColumns" :data="routes" :bordered="false" /></section>
      <template v-if="page==='user-sources' && admin"><UserSources /></template>
      <template v-if="page==='relays'"><div v-if="!nodes.length" class="panel empty"><div>⬡</div><h4>还没有中转节点</h4><p>生成一次性注册令牌，在你的另一台服务器上启动 Relay Agent。</p></div><div class="node-grid"><article v-for="n in nodes" :key="n.id" class="panel node-card"><div class="panel-heading"><h3>{{n.name}}</h3><n-tag :type="online(n)?'success':'default'" :bordered="false">{{!n.enabled?'已停用':online(n)?'在线':'离线'}}</n-tag></div><p>{{n.endpoint}}</p><div class="connection-row"><span>活跃连接</span><b>{{n.active_sessions}}</b></div><small>最近心跳：{{date(n.last_seen)}}</small><div v-if="admin" class="node-actions"><n-button size="small" @click="toggleNode(n)">{{n.enabled?'停用':'启用'}}</n-button><n-button size="small" type="error" quaternary @click="remove('relays',n.id)">删除</n-button></div></article></div></template>
      <section v-if="page==='sessions'" class="panel"><div class="panel-heading"><h3>最近 100 个请求会话</h3><span>点击查看 IP、客户端与流量 · 全部节点</span></div><n-data-table :columns="sessionColumns" :data="sessions" :bordered="false" :pagination="{pageSize:15}" class="session-summary" :table-layout="'fixed'" /></section>
      <DeviceManagement v-if="page==='devices'&&admin" :upstreams="upstreams" :routes="routes" @changed="run(refresh)" />
      <AccessSubscriptions v-if="page==='subscriptions'&&admin" :routes="routes" :nodes="nodes" />
<section v-if="page==='traffic'" class="panel"><div class="panel-heading"><h3>客户端流量（最近 100 个请求会话）</h3><span>仅当前会话样本，不是完整历史流量</span></div><responsive-table :data="clientTraffic" :columns="[{title:'客户端 IP',key:'ip'},{title:'客户端',key:'app'},{title:'请求数',key:'count',width:100,align:'right'},{title:'发送流量',key:'bytes',width:140,align:'right',render:(r:any)=>bytes(r.bytes)}]" :pagination="{pageSize:10}" /></section>
<section v-if="page==='traffic'" class="panel"><div class="panel-heading"><h3>每日流量记录</h3><span>UTC · 不含网络协议开销</span></div><responsive-table :columns="trafficColumns" :data="traffic" :bordered="false" :pagination="{pageSize:20}" /></section>
      <section v-if="page==='audit'" class="panel"><responsive-table :columns="auditColumns" :data="audit" :bordered="false" :pagination="{pageSize:20}" /></section>
      <template v-if="page==='diagnostics'"><section class="panel diagnostics-panel"><div class="panel-heading"><h3>请求错误诊断</h3><span>点击查看完整诊断 · 最近 100 条 / 每节点保留 1000 条</span></div><div class="diagnostics-controls"><n-select v-model:value="diagnosticNode" :options="diagnosticNodeOptions" placeholder="选择节点" style="width:240px;max-width:100%" /><n-button size="small" @click="run(refreshDiagnostics)">刷新</n-button></div><p class="diagnostics-note">中转诊断每 15 秒分批同步；断线后恢复补传。仅记录失败请求及 Agent 同步错误。</p><n-data-table :data="diagnostics" :columns="diagnosticColumns" :pagination="{pageSize:15}" class="diagnostic-summary" /></section></template><template v-if="page==='settings'"><section class="panel security-panel"><h3>双因素认证</h3><p>使用认证器应用保护管理账户。更新设置后需要重新登录。</p><n-tag :type="me.totp_enabled?'success':'default'">{{me.totp_enabled?'TOTP 已开启':'TOTP 未开启'}}</n-tag><div class="security-form"><n-input v-model:value="totpPassword" type="password" placeholder="当前密码" /><n-button v-if="!me.totp_enabled && !totpSecret" :disabled="!totpPassword" @click="setupTOTP">配置认证器</n-button><small v-if="!totpPassword && !me.totp_enabled">请先输入当前账户密码</small><template v-if="totpSecret"><n-alert type="info">使用 Google Authenticator（谷歌身份验证器）扫描下方二维码，或手动添加密钥（基于时间）：<code>{{totpSecret}}</code></n-alert><n-qr-code :value="totpURI" :size="200" /></template><n-input v-if="totpSecret||me.totp_enabled" v-model:value="totpCode" placeholder="6 位动态验证码" /><n-button v-if="totpSecret||me.totp_enabled" :type="me.totp_enabled?'error':'primary'" @click="changeTOTP(me!.totp_enabled?'disable':'enable')">{{me.totp_enabled?'关闭 TOTP':'验证并启用'}}</n-button></div></section><section class="panel security-panel"><h3>修改自己的密码</h3><p>修改后所有登录会话将失效，需要重新登录。</p><div class="security-form"><n-input v-model:value="currentPassword" type="password" placeholder="当前密码" /><n-input v-model:value="nextPassword" type="password" placeholder="新密码（8–72 字节）" /><n-button type="primary" :disabled="!currentPassword||!nextPassword" @click="changeOwnPassword">修改密码</n-button></div></section><section v-if="admin" class="panel"><div class="panel-heading"><h3>管理账户</h3><n-button size="small" @click="open('user')">＋ 添加账户</n-button></div><responsive-table :columns="userColumns" :data="users" :bordered="false" /></section></template>
      <footer>EmbyRelay <span>自托管媒体中转平台</span><span>数据每 15 秒刷新</span></footer>
      </div>
    </main>
  </div>
  <n-modal v-model:show="modal" preset="card" :title="(edit.id?'编辑':'添加')+(kind==='upstream'?'源站':kind==='route'?'路由':'账户')" class="edit-modal" :style="{width:'560px',maxWidth:'calc(100vw - 32px)'}"><n-form label-placement="top" @submit.prevent="save">
    <template v-if="kind==='upstream'"><n-form-item label="源站名称"><n-input v-model:value="edit.name" placeholder="例如：家庭 Emby" /></n-form-item><n-form-item label="Emby 地址"><n-input v-model:value="edit.base_url" placeholder="emby.example.com 或 https://emby.example.com:8920" /></n-form-item><p>支持粘贴域名、IP 或完整地址；保留已填协议和端口。未填协议默认 HTTPS，未填端口默认 HTTPS 443 / HTTP 80，保存时自动补全。</p><n-form-item label="Emby 用户名（可选）"><n-input v-model:value="edit.emby_username" autocomplete="off" placeholder="编辑时留空保留已保存用户名" /></n-form-item><n-form-item label="Emby 密码（可选，加密保存）"><n-input v-model:value="edit.emby_password" type="password" autocomplete="new-password" show-password-on="click" placeholder="编辑时留空保留已保存密码" /></n-form-item><p v-if="edit.has_credentials">已保存源站账号。订阅访问账号可使用该账号登录源站。</p><n-form-item v-if="edit.has_credentials" label="清除已保存的 Emby 账号密码"><n-switch v-model:value="edit.clear_credentials" /></n-form-item><n-form-item label="回源私网白名单（CIDR，逗号分隔）"><n-input v-model:value="edit.cidrs" placeholder="192.168.1.20/32" /></n-form-item></template>
    <template v-if="kind==='route'"><n-form-item label="路由别名"><n-input v-model:value="edit.alias" placeholder="home-media" /></n-form-item><n-form-item label="Emby 源站"><n-select v-model:value="edit.upstream_id" :options="upstreams.map(u=>({label:u.name,value:u.id}))" /></n-form-item><n-form-item label="服务节点"><n-select v-model:value="edit.relay_id" :options="[{label:'本机中转',value:''},{label:'自动选择在线节点',value:'auto'},...nodes.map(n=>({label:n.name,value:n.id}))]" /></n-form-item><n-form-item label="路由共享速率上限（字节/秒，0 不限）"><n-input-number v-model:value="edit.rate_bps" :min="0" /></n-form-item><n-form-item label="每日发送配额（字节，0 不限）"><n-input-number v-model:value="edit.daily_quota" :min="0" /></n-form-item><n-form-item label="最大并发请求"><n-input-number v-model:value="edit.max_concurrent" :min="1" :max="10000" /></n-form-item></template>
    <template v-if="kind==='user'"><n-form-item label="用户名"><n-input v-model:value="edit.username" /></n-form-item><n-form-item label="密码（8–72 字节）"><n-input v-model:value="edit.password" type="password" /></n-form-item><n-form-item label="角色"><n-select v-model:value="edit.role" :options="[{label:'管理员',value:'admin'},{label:'只读操作员',value:'operator'}]" /></n-form-item></template>
    <n-form-item v-if="kind!=='user'" label="启用"><n-switch v-model:value="edit.enabled" /></n-form-item><n-alert v-if="error" type="error">{{error}}</n-alert><div class="modal-footer"><n-button @click="modal=false">取消</n-button><n-button type="primary" :loading="busy" @click="save">保存配置</n-button></div>
  </n-form></n-modal>
  <n-modal :show="!!keyRoute" @update:show="v=>{if(!v){keyRoute=null;secret='';importInfo=null}}" preset="card" title="代理访问地址" :style="{width:'620px',maxWidth:'calc(100vw - 32px)'}"><p>路由：{{keyRoute?.alias}}。新地址加密保存，管理员可再次查看。旧版密钥只存摘要，无法恢复地址。轮换时先生成新密钥，再撤销旧密钥。</p><div class="key-create"><n-input-number v-model:value="keyExpiry" :min="0" :max="3650"><template #suffix>天有效 · 0 永久</template></n-input-number><n-button type="primary" @click="newKey">生成地址</n-button></div><div v-if="secret" class="secret-box"><code>{{secret}}</code><n-button size="small" @click="copy(secret)">复制地址</n-button></div><section v-if="importInfo" class="panel"><h3>播放器导入信息</h3><p>包含明文密码和访问密钥，请只分享给账号使用者。</p><ImportDetails :text="importText" /><n-button @click="importInfo=null">隐藏</n-button></section><div v-for="k in keys" :key="k.id" class="key-row"><div><code>{{k.id.slice(0,12)}}</code><div v-if="k.access_key"><code style="overflow-wrap:anywhere">{{relayAddress(k.access_key)}}</code><n-button @click="copy(relayAddress(k.access_key))">复制地址</n-button><n-button type="primary" secondary @click="showImport(k.access_key)" :disabled="!!k.account_id||!k.enabled||(k.expires_at>0&&k.expires_at<Date.now()/1000)">一键展示导入信息</n-button><small v-if="k.account_id">订阅地址：请到“访问账号与套餐”查看用户导入信息，不要分享真实源站账号。</small></div><small v-else>旧版密钥无法恢复完整地址，请重新生成</small><small>到期：{{k.expires_at?date(k.expires_at):'永不过期'}}</small></div><n-button size="small" type="error" quaternary @click="revokeKey(k.id)">撤销</n-button></div><n-alert v-if="error" type="error">{{error}}</n-alert></n-modal>
  <n-modal :show="!!enrollment" @update:show="v=>{if(!v)enrollment=null}" preset="card" title="注册中转节点" :style="{width:'720px',maxWidth:'calc(100vw - 32px)'}">
    <p>在 B 的 Debian / Ubuntu 服务器执行命令，按提示输入节点域名与名称。需已安装 Docker Compose v2；默认部署到 /opt/embyrelay-agent。</p>
    <n-alert type="info">宿主机 Nginx + Certbot：自动检查反代、申请并续签证书；Agent 默认使用 host 网络。</n-alert>
    <p style="margin-top:16px">请先将节点域名解析到 B。令牌十分钟内有效且只能使用一次；命令含注册令牌，请勿分享。下载镜像耗时过长时，可重新生成令牌后重试。</p>
    <div class="agent-install-command"><pre>{{agentInstallCommand}}</pre><n-button type="primary" @click="copy(agentInstallCommand)">复制部署命令</n-button></div>
    <p style="margin-top:16px">注册成功后，在代理路由中选择 B。已有部署重复执行时保留节点配置与数据，检查 HTTPS 反代并更新镜像。</p>
    <details><summary>手动部署配置</summary><div class="secret-box"><code>RELAY_ENROLL_TOKEN={{enrollment?.token}}<br>RELAY_SERVER_KEY={{enrollment?.server_key}}<br>RELAY_CONTROL_URL={{origin}}</code><n-button size="small" @click="copy(`RELAY_ENROLL_TOKEN=${enrollment.token}\nRELAY_SERVER_KEY=${enrollment.server_key}\nRELAY_CONTROL_URL=${origin}`)">复制配置</n-button></div></details>
    <n-alert v-if="!origin.startsWith('https://')" type="warning" style="margin-top:16px">当前面板为 HTTP，请在部署命令中将控制端地址改为 A 的公网 HTTPS 地址。</n-alert>
    <n-alert v-if="error" type="error" style="margin-top:16px">{{error}}</n-alert>
  </n-modal>
<n-drawer :show="!!selectedDiagnostic" @update:show="v=>{if(!v)selectedDiagnostic=null}" placement="right" class="diagnostic-drawer" :width="640">
 <n-drawer-content title="请求诊断详情" closable>
 <template v-if="selectedDiagnostic">
 <n-alert v-if="error" type="error" style="margin-bottom:16px">{{error}}</n-alert>
 <n-tag :type="selectedDiagnostic.status>=500?'error':'warning'">HTTP {{selectedDiagnostic.status}}</n-tag>
 <h3 class="diagnostic-reason">{{selectedDiagnostic.reason||'旧记录未保存具体原因'}}</h3>
 <dl class="diagnostic-fields">
 <dt>服务器节点</dt><dd>{{diagnosticNodeName(selectedDiagnostic)}}<small v-if="selectedDiagnostic.node_id">{{selectedDiagnostic.node_id}}</small></dd>
 <dt>时间 / 来源</dt><dd>{{date(selectedDiagnostic.created_at)}} · {{selectedDiagnostic.source==='upstream'?'回源阶段':selectedDiagnostic.source==='agent'?'Agent 同步':'本机处理'}}</dd>
 <dt>请求方法 / 路由 / 耗时</dt><dd>{{selectedDiagnostic.method||'—'}} · {{selectedDetails.route||'—'}} · {{selectedDetails.duration_ms??'—'}} ms</dd>
 <dt>入站路径（脱敏）</dt><dd>{{selectedDiagnostic.path||'—'}}</dd>
 <dt>回源相对路径</dt><dd>{{selectedDetails.upstream_path||'—'}}</dd>
 <template v-if="selectedDetails.auth">
 <dt>请求用途</dt><dd>{{selectedDetails.auth.operation==='playback_progress'?'播放进度上报':'其他请求'}}</dd>
 <dt>入站认证相关字段</dt><dd>{{selectedDetails.auth.inbound_fields?.join('、')||'未发现常见认证字段'}}</dd>
 <dt>实际回源认证相关字段</dt><dd>{{selectedDetails.auth.outbound_fields?.join('、')||'未发现常见认证字段'}}<small>仅表示字段存在，不代表 Token 有效；不记录凭据内容。</small></dd>
 <dt>本次请求观察到的跳转</dt><dd>{{selectedDetails.auth.redirect_origins?.join(' → ')||'未观察到跳转'}}<small>不包含播放器此前请求的跳转。</small></dd>
 </template>
 <template v-if="selectedDetails.device_identity"><dt>设备身份模式 / 来源</dt><dd>{{selectedDetails.device_identity.mode}} / {{selectedDetails.device_identity.scope}}</dd><dt>原始 / 上游设备 ID</dt><dd>{{selectedDetails.device_identity.original.id}}<br />{{selectedDetails.device_identity.applied.id}}</dd></template>
 <dt>跳转目标</dt><dd>{{selectedDetails.target||'无记录'}}</dd>
 <dt>处理建议</dt><dd>{{selectedDetails.advice||'检查源站连通性、账号授权和路由配置；复制诊断供排查。'}}</dd>
 <dt>请求 ID</dt><dd>{{selectedDiagnostic.id}}</dd>
 </dl></template>
 <template #footer><div v-if="selectedDiagnostic" class="diagnostic-detail-actions">

 <n-button @click="copy(JSON.stringify({...selectedDiagnostic,details:selectedDetails},null,2))">复制诊断</n-button>
 </div></template>
 </n-drawer-content></n-drawer>
<n-drawer :show="!!selectedSession" @update:show="v=>{if(!v)selectedSession=null}" class="diagnostic-drawer" :width="640"><n-drawer-content title="播放会话详情" closable><dl v-if="selectedSession" class="diagnostic-fields"><dt>会话 ID</dt><dd>{{selectedSession.id}}</dd><dt>客户端 IP</dt><dd>{{selectedSession.client_ip||'旧记录无此信息'}}</dd><dt>客户端 / 版本</dt><dd>{{selectedSession.client_name||'未提供'}} {{selectedSession.client_version}}</dd><dt>User-Agent（客户端自报）</dt><dd>{{selectedSession.user_agent||'未提供'}}</dd><dt>开始 / 结束</dt><dd>{{date(selectedSession.started_at)}} / {{date(selectedSession.ended_at)}}</dd><dt>路由 / 节点</dt><dd>{{routeName(selectedSession.route_id)}} / {{sessionNode(selectedSession.node_id)}}</dd><dt>节点 ID</dt><dd>{{selectedSession.node_id}}</dd><dt>发送流量 / 状态</dt><dd>{{bytes(selectedSession.bytes_sent)}} / {{sessionStatus(selectedSession.status)}}</dd></dl><template #footer><n-button @click="copy(JSON.stringify(selectedSession,null,2))">复制会话信息</n-button></template></n-drawer-content></n-drawer>
</n-config-provider>
</template>
