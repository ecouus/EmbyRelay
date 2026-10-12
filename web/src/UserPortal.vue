<script setup lang="ts">
import BrandLogo from './BrandLogo.vue'
import UiIcon from './UiIcon.vue'
import DeviceRecords from './DeviceRecords.vue'
import ImportDetails from './ImportDetails.vue'
import {ref,onMounted,onUnmounted} from 'vue'
import {NButton,NInput,NModal,NForm,NFormItem,NAlert,NTag} from 'naive-ui'
const deviceModal=ref(false),deviceRecords=ref<any[]>([])
async function showDevices(){deviceModal.value=true;await run(async()=>{deviceRecords.value=await api('devices')})}
const emit=defineEmits(['theme','logout'])
const me=ref<any>(null),sources=ref<any[]>([]),username=ref(''),password=ref(''),error=ref(''),busy=ref(false),modal=ref(false),form=ref<any>({}),delivery=ref(''),notice=ref('')
const date=(n:number)=>n?new Date(n*1000).toLocaleString('zh-CN'):'长期有效'
const bytes=(n:number)=>n>=1073741824?(n/1073741824).toFixed(2)+' GiB':(n/1048576).toFixed(2)+' MiB'
async function api(path:string,method='GET',body?:any){const r=await fetch('/api/v1/portal/'+path,{method,headers:{'Content-Type':'application/json','X-Requested-With':'EmbyRelay'},body:body===undefined?undefined:JSON.stringify(body)});const d=await r.json();if(!r.ok){if(r.status===401&&path!=='login'){me.value=null;sources.value=[];delivery.value='';modal.value=false;deviceModal.value=false;deviceRecords.value=[];emit('logout')}throw Error(d.error||'请求失败')}return d}
async function run(fn:()=>Promise<void>){busy.value=true;error.value='';try{await fn()}catch(e){error.value=String(e instanceof Error?e.message:e)}finally{busy.value=false}}
async function refresh(){me.value=await api('me');sources.value=await api('sources')}
async function login(){await run(async()=>{await api('login','POST',{username:username.value,password:password.value});password.value='';await refresh()})}
async function logout(){await run(async()=>{await api('logout','POST',{});me.value=null;sources.value=[];delivery.value='';form.value={};deviceModal.value=false;deviceRecords.value=[];emit('logout')})}
function edit(s?:any){form.value=s?{id:s.id,name:s.name,base_url:s.base_url,emby_username:'',emby_password:''}:{name:'',base_url:'',emby_username:'',emby_password:''};modal.value=true}
async function save(){await run(async()=>{const {id,...body}=form.value;if(id&&!body.emby_username)delete body.emby_username;if(id&&!body.emby_password)delete body.emby_password;await api('sources'+(id?'/'+id:''),id?'PATCH':'POST',body);modal.value=false;form.value={};await refresh();notice.value='源站已保存，所有地址共用账户额度'})}
async function remove(s:any){if(!window.confirm('删除 '+s.name+'？该源站的中转地址会失效，已用流量保留。'))return;await run(async()=>{await api('sources/'+s.id,'DELETE');await refresh()})}
function show(s:any){const addresses=s.connection_addresses?.length?s.connection_addresses.map((a:any)=>(a.origin||location.origin).replace(/\/$/,'')+a.path):s.addresses.map((p:string)=>location.origin+p);delivery.value=`名称：${s.name}\n源站地址：${s.base_url}\n中转服务器地址：${addresses.join('\n')}\n登录用户名：${me.value.account.username}\n登录密码：使用你的用户中心密码\n所有地址共享账户流量和有效期；下载 1 GiB 按双向折算扣除 2 GiB。`}
let timer:ReturnType<typeof setInterval>|undefined
onMounted(async()=>{try{await refresh()}catch{}timer=setInterval(()=>{if(me.value&&!busy.value&&!modal.value)refresh().catch(e=>error.value=e.message)},15000)})
onUnmounted(()=>clearInterval(timer))
</script>
<template>
<main class="portal"><header><a class="portal-brand" href="/"><BrandLogo /> EmbyRelay <small>用户中心</small></a><div><n-button @click="emit('theme')">明暗切换</n-button><n-button v-if="me" @click="showDevices">我的设备</n-button><n-button v-if="me" @click="logout">退出登录</n-button><a v-else href="/">管理控制台</a></div></header>
<n-alert v-if="error" type="error" closable @close="error=''">{{error}}</n-alert>
<form v-if="!me" class="portal-login" @submit.prevent="login"><div class="eyebrow">YOUR MEDIA SPACE</div><h1>用户中心</h1><p>添加自己的 Emby，随时查看订阅与剩余额度。</p><n-form-item label="用户名"><n-input v-model:value="username" autocomplete="username" placeholder="输入管理员为你开通的用户名" size="large" /></n-form-item><n-form-item label="密码"><n-input v-model:value="password" type="password" show-password-on="click" autocomplete="current-password" placeholder="输入账户密码" size="large" /></n-form-item><n-button attr-type="submit" type="primary" size="large" block :loading="busy">登录用户中心 →</n-button><div class="portal-login-note">账号由管理员开通<br />播放器使用相同的用户名和密码</div></form>
<template v-else><div class="portal-title"><div class="portal-welcome-icon"><UiIcon name="sessions" /></div><div class="eyebrow">YOUR MEDIA SPACE</div><h1>{{me.account.username}} 的媒体空间</h1><p>全部 Emby 共享流量和有效期。流量按双向折算：下载 1 GiB，扣除 2 GiB。</p></div><div class="portal-stats"><article><div class="portal-stat-icon"><UiIcon name="shield" /></div><span>订阅有效期</span><strong>{{me.account.expires_at&&me.account.expires_at<=Date.now()/1000?'已到期':!me.account.started_at?'待首次播放登录':me.account.expires_at?Math.max(0,Math.ceil((me.account.expires_at-Date.now()/1000)/86400))+' 天':'长期'}}</strong><small>{{date(me.account.expires_at)}}</small></article><article><div class="portal-stat-icon blue"><UiIcon name="traffic" /></div><span>本周期已用</span><strong>{{bytes(me.used_bytes)}}</strong><small>总额度 {{me.account.plan.traffic_bytes?bytes(me.account.plan.traffic_bytes):'不限'}}</small></article><article><div class="portal-stat-icon purple"><UiIcon name="overview" /></div><span>剩余流量</span><strong>{{me.account.plan.traffic_bytes?bytes(Math.max(0,me.account.plan.traffic_bytes-me.used_bytes)):'不限'}}</strong><small>{{me.next_reset_at?'重置于 '+date(me.next_reset_at):'累计额度，不自动重置'}}</small></article></div>
<n-alert v-if="notice" type="success" closable @close="notice=''">{{notice}}</n-alert><section class="panel"><div class="portal-section"><div><h2>我的 Emby</h2><p>保存源站账号后，使用中转地址和用户中心账号登录播放器。</p></div><n-button type="primary" @click="edit()">添加 Emby</n-button></div><div class="portal-grid"><article v-for="s in sources" :key="s.id" class="source"><div class="source-title"><span class="source-icon"><UiIcon name="upstreams" /></span><h3>{{s.name}}</h3><n-tag :type="s.enabled?'success':'default'">{{s.enabled?'已启用':'已停用'}}</n-tag></div><p>{{s.base_url}}</p><div class="portal-actions"><n-button type="primary" :disabled="!s.addresses.length" @click="show(s)">播放器配置 / 复制</n-button><n-button @click="edit(s)">编辑</n-button><n-button @click="remove(s)">删除</n-button></div></article><p v-if="!sources.length">还没有添加 Emby 源站。添加第一个源站，即可获取播放器连接信息。</p></div></section></template>
<n-modal v-model:show="deviceModal" preset="card" title="我的设备身份" :style="{width:'900px',maxWidth:'calc(100vw - 32px)'}"><p class="device-record-note">查看播放器登录时使用的设备身份。身份规则由管理员配置。</p><DeviceRecords :records="deviceRecords" :upstreams="sources" /><n-alert v-if="error" type="error">{{error}}</n-alert></n-modal>
<n-modal :show="modal" @update:show="v=>{modal=v;if(!v)form={}}" preset="card" :title="form.id?'编辑 Emby':'添加 Emby'" :style="{width:'540px',maxWidth:'calc(100vw - 32px)'}"><n-form @submit.prevent="save"><n-form-item label="名称"><n-input v-model:value="form.name" placeholder="我的 Emby" /></n-form-item><n-form-item label="Emby 地址"><n-input v-model:value="form.base_url" placeholder="emby.example.com:8920" /></n-form-item><p>未填写协议时补全 HTTPS。只支持公网源站，私网访问请联系管理员。</p><n-form-item :label="form.id?'Emby 用户名（留空保留）':'Emby 用户名'"><n-input v-model:value="form.emby_username" autocomplete="off" /></n-form-item><n-form-item :label="form.id?'Emby 密码（留空保留）':'Emby 密码'"><n-input v-model:value="form.emby_password" type="password" autocomplete="new-password" /></n-form-item><n-alert v-if="error" type="error">{{error}}</n-alert><n-button type="primary" :loading="busy" @click="save">保存</n-button></n-form></n-modal>
<n-modal :show="!!delivery" @update:show="v=>{if(!v)delivery=''}" preset="card" title="播放器导入信息" :style="{width:'600px',maxWidth:'calc(100vw - 32px)'}"><ImportDetails :text="delivery" /></n-modal>
</main>
</template>
<style scoped>
.portal{max-width:1180px;margin:auto;padding:24px;color:var(--text)}header,.portal-section,.source-title{display:flex;align-items:center;justify-content:space-between;gap:16px}header{padding:8px 0 24px;border-bottom:1px solid var(--line)}header a{color:inherit;text-decoration:none;font-weight:600}header>div,.portal-actions{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.portal-title{padding:36px 0 16px}.portal p,.portal small,.portal-stats span{color:var(--muted);line-height:1.7}.portal-stats{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:18px;margin:12px 0 28px}.portal-stats article,.source{border:1px solid var(--line);border-radius:14px;padding:24px;background:var(--surface)}.portal-stats strong{display:block;font-size:28px;margin:12px 0}.portal-stats small{display:block}.portal-section{padding:24px}.portal-section h2{margin:0}.portal-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,300px),1fr));gap:18px;padding:0 24px 24px}.source h3{margin:0}.source p,pre{overflow-wrap:anywhere;white-space:pre-wrap}.portal-login{max-width:420px;margin:70px auto}.portal-actions{margin-top:24px}@media(max-width:650px){.portal{padding:16px}.portal-stats{grid-template-columns:1fr}.portal-section{align-items:flex-start;flex-direction:column}.portal-login{margin-top:40px}.portal-stats article{padding:18px}.portal-title h1{font-size:26px}}

.portal{min-height:100dvh;padding-top:28px}
.portal>header{height:auto;min-height:64px;background:transparent;flex-wrap:wrap}
.portal-brand{display:flex;align-items:center;gap:10px;font-size:20px;letter-spacing:-.4px}
.portal-brand small{border-left:1px solid var(--line);padding-left:12px;font-size:12px;font-weight:500;letter-spacing:0}
.portal-login{max-width:480px;margin:64px auto;padding:36px;background:var(--surface);border:1px solid var(--line);border-radius:20px;box-shadow:var(--shadow)}
.portal-login h1{font-size:30px;margin:10px 0 12px;letter-spacing:-1px}
.portal-login>p{font-size:13px;margin-bottom:28px}
.portal-login-note{margin-top:24px;padding-top:20px;border-top:1px solid var(--line);color:var(--muted);font-size:12px;text-align:center;line-height:1.9}
.portal-title{margin-top:28px;margin-bottom:24px;padding:28px 32px;border:1px solid var(--line);border-radius:18px;background:linear-gradient(120deg,var(--accent-soft),var(--surface))}
.portal-title h1{font-size:30px;margin:10px 0;overflow-wrap:anywhere}.portal-title p{margin:0;font-size:13px}
.portal-stats article{padding:22px 24px}.portal-stats strong{font-size:30px;letter-spacing:-.7px}.portal-stats article:last-child strong{color:var(--accent)}
.portal-section p{font-size:13px;margin:8px 0 0}.source{background:var(--surface);box-shadow:var(--shadow)}
.source-title{align-items:flex-start}.source-title h3{font-size:17px;overflow-wrap:anywhere;min-width:0}.source-title .n-tag{flex-shrink:0}
.source>p{padding:10px 12px;background:var(--surface-soft);border-radius:8px;font-family:ui-monospace,monospace;font-size:12px;margin-top:18px}
.portal-grid>p{grid-column:1/-1;padding:32px;text-align:center;border:1px dashed var(--line);border-radius:12px}
@media(max-width:650px){.portal-login{padding:24px;margin:32px auto}.portal-title{padding:24px}.portal-brand{font-size:18px}.portal>header>div{gap:8px}.portal-section,.portal-grid{padding-left:18px;padding-right:18px}.source{padding:18px}}

.portal{max-width:1240px;padding:24px 32px 56px}
.portal>header{padding:0 0 22px;margin-bottom:32px}
.portal-title{position:relative;background:var(--surface);border-radius:16px;padding:30px 32px;margin-top:0;box-shadow:var(--shadow)}
.portal-title h1{font-size:28px;letter-spacing:-.6px}.portal-title p{max-width:80%;line-height:1.8}
.portal-welcome-icon{position:absolute;right:32px;top:32px;width:64px;height:64px;border-radius:18px;background:var(--accent-soft);color:var(--accent);display:grid;place-items:center}.portal-welcome-icon .ui-icon{width:32px;height:32px}
.portal-stats{gap:20px}.portal-stats article{position:relative;border-radius:16px;box-shadow:var(--shadow)}.portal-stats strong{font-size:30px;font-weight:650;margin:18px 0 8px}.portal-stats span{font-size:13px}.portal-stat-icon{float:right;display:grid;place-items:center;width:40px;height:40px;border-radius:12px;color:var(--accent);background:var(--accent-soft)}.portal-stat-icon.blue{color:#3b82f6;background:#3b82f615}.portal-stat-icon.purple{color:#8b5cf6;background:#8b5cf615}
.portal-section h2{font-size:18px}.source{box-shadow:none;border-radius:14px;padding:22px;transition:border-color .2s,box-shadow .2s}.source:hover{border-color:var(--accent);box-shadow:var(--shadow)}.source-icon{display:grid;place-items:center;width:36px;height:36px;background:var(--accent-soft);color:var(--accent);border-radius:10px;flex-shrink:0}.source-title{align-items:center;gap:10px}.source-title h3{flex:1;font-size:16px}.portal-actions{gap:8px}.portal-actions .n-button:first-child{flex:1}
@media(max-width:650px){.portal{padding:16px}.portal-title{padding:24px}.portal-title h1{font-size:24px;max-width:80%}.portal-title p{max-width:100%;font-size:12px}.portal-welcome-icon{width:40px;height:40px;right:20px;top:24px}.portal-welcome-icon .ui-icon{width:24px;height:24px}.portal-stats{gap:12px}.portal-stats article{padding:20px}.portal>header{margin-bottom:20px}.portal-brand small{display:none}}
</style>
