<script setup lang="ts">
import {h,ref} from 'vue'
import {NButton,NModal,NTag} from 'naive-ui'
import ResponsiveTable from './ResponsiveTable.vue'
const props=defineProps<{records:any[];accounts?:any[];upstreams?:any[]}>()
const selected=ref<any>(null)
const mode=(r:any)=>r.identity.scope==='default'?'兼容默认':({original:'原始标识',stable:'稳定标识',custom:'自定义标识',grouped:'身份分组'} as Record<string,string>)[r.identity.mode]||r.identity.mode
const date=(n:number)=>n?new Date(n*1000).toLocaleString('zh-CN'):'—'
const name=(id:string)=>props.accounts?.find(a=>a.account.id===id)?.account.username||'当前账号'
const source=(id:string)=>props.upstreams?.find(u=>u.id===id)?.name||id
const columns=[
 {title:'设备 / 最近登录映射',key:'identity',render:(r:any)=>h('div',{class:'device-cell'},[h('b',r.identity.original.name||r.identity.original.client||'未命名设备'),h('small',r.identity.original.id||'未提供设备 ID'),h('small','→ '+r.identity.applied.id)])},
 {title:'模式',key:'mode',render:(r:any)=>h(NTag,{size:'small',bordered:false,type:'info'},()=>mode(r))},
 {title:'会话状态',key:'login_sessions',render:(r:any)=>`${r.login_sessions} 个登录 · ${r.active_requests} 个媒体请求`},
 {title:'最近活动',key:'last_seen',render:(r:any)=>date(r.last_seen)},
 {title:'操作',key:'actions',render:(r:any)=>h(NButton,{size:'small',secondary:true,onClick:()=>selected.value=r},()=> '详情')}
]
</script>
<template>
 <ResponsiveTable :columns="columns" :data="records" :pagination="{pageSize:10}" />
 <n-modal :show="!!selected" @update:show="v=>{if(!v)selected=null}" preset="card" title="设备身份详情" :style="{width:'680px',maxWidth:'calc(100vw - 32px)'}">
  <template v-if="selected">
   <p class="device-record-note">显示最近一次登录尝试的映射。旧登录会话继续使用各自保存的身份；媒体请求数不等于播放器数量。</p>
   <dl class="device-record-fields"><dt>账号 / 源站</dt><dd>{{name(selected.account_id)}} / {{source(selected.upstream_id)}}</dd><dt>模式 / 配置来源</dt><dd>{{mode(selected)}} / {{selected.identity.scope==='account'?'账号覆盖':selected.identity.scope==='source'?'源站默认':'兼容默认'}}</dd><dt>原始设备 ID</dt><dd>{{selected.identity.original.id||'未提供'}}</dd><dt>发送到源站的 ID</dt><dd>{{selected.identity.applied.id}}</dd><dt>原始 / 上游设备名称</dt><dd>{{selected.identity.original.name||'未提供'}} / {{selected.identity.applied.name}}</dd><dt>原始客户端</dt><dd>{{selected.identity.original.client||'未提供'}} {{selected.identity.original.version}}</dd><dt>上游客户端</dt><dd>{{selected.identity.applied.client}} {{selected.identity.applied.version}}</dd><dt>身份组</dt><dd>{{selected.identity.group||'未分组'}}</dd><dt>最近登录结果</dt><dd>{{selected.result}} {{selected.source_status?'· HTTP '+selected.source_status:''}}</dd><dt>最近请求响应</dt><dd>{{selected.last_request_status?'HTTP '+selected.last_request_status:'暂无记录'}} · {{date(selected.last_request_at)}}</dd><dt>最近登录 / 活动</dt><dd>{{date(selected.last_login)}} / {{date(selected.last_seen)}}</dd></dl>
  </template>
 </n-modal>
</template>
<style>
.device-cell{min-width:0}.device-cell b{font-weight:600;font-size:13px}.device-cell small{display:block;color:var(--muted);font-family:ui-monospace,monospace;font-size:11px;line-height:1.8;overflow-wrap:anywhere}.device-record-note{font-size:12px;color:var(--muted);line-height:1.8}.device-record-fields{display:grid;grid-template-columns:140px minmax(0,1fr);gap:14px;font-size:13px}.device-record-fields dt{color:var(--muted)}.device-record-fields dd{margin:0;overflow-wrap:anywhere}@media(max-width:650px){.device-record-fields{grid-template-columns:100px minmax(0,1fr)}}
</style>
