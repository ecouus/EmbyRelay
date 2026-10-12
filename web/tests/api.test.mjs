import {readFileSync} from 'node:fs'
import {test,beforeEach} from 'node:test'
import assert from 'node:assert/strict'
import ts from 'typescript'
const source=readFileSync(new URL('../src/api.ts',import.meta.url),'utf8')
const {outputText}=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ES2022}})
const {requestAPI,AUTH_EXPIRED_EVENT}=await import('data:text/javascript;base64,'+Buffer.from(outputText).toString('base64'))
let events
beforeEach(()=>{
 events=[]
 globalThis.window=new EventTarget()
 globalThis.CustomEvent=class extends Event{constructor(type,options){super(type);this.detail=options.detail}}
 window.addEventListener(AUTH_EXPIRED_EVENT,event=>events.push(event.detail))
})
function respond(error,status=401){globalThis.fetch=async()=>new Response(JSON.stringify({error}),{status})}
test('expired admin session notifies the workspace from any component',async()=>{
 respond('authentication required')
 await requestAPI('/api/v1/device-policy')
 assert.deepEqual(events,[{scope:'admin'}])
})
test('wrong current password does not log out a valid session',async()=>{
 respond('current password is incorrect')
 await requestAPI('/api/v1/auth/password','POST',{current:'wrong'})
 assert.deepEqual(events,[])
})
test('portal expiry has its own scope while login failures do not expire sessions',async()=>{
 respond('用户名或密码错误，或账号已停用')
 await requestAPI('/api/v1/portal/login','POST',{})
 assert.deepEqual(events,[])
 respond('请重新登录')
 await requestAPI('/api/v1/portal/sources')
 assert.deepEqual(events,[{scope:'portal'}])
})
test('proxy HTML error produces a useful HTTP error',async()=>{
 globalThis.fetch=async()=>new Response('<html>Bad Gateway</html>',{status:502})
 const {data}=await requestAPI('/api/v1/user-sources')
 assert.match(data.error,/HTTP 502/)
 assert.deepEqual(events,[])
})
