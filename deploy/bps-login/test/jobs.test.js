import test from 'node:test';
import assert from 'node:assert/strict';
import {JobStore,validateInput} from '../src/jobs.js';
import {createServer} from '../src/server.js';
import {createLoginEngine,ADDIN_AUTHORIZATION} from '../upstream/server/login-engine.js';
import {validateProxy} from '../src/native-runtime.js';

const input={email:'member@example.test',password:'test-login-password-never-output',totp_secret:'JBSWY3DPEHPK3PXP',workspace_preference:'first'};
const key='test-private-login-key-012345678901234567890';
const credential=()=>({type:'oai-basispoints',client_id:ADDIN_AUTHORIZATION.clientId,access_token:'test-access-token-secret',refresh_token:'test-refresh-token-secret',account_id:'workspace-a',email:input.email,expired:'2099-01-01T00:00:00.000Z',plan_type:'self_serve_business_prolite'});
const tick=()=>new Promise(r=>setImmediate(r));
async function settle(store,id,status='ready'){for(let i=0;i<50;i++){await tick();const job=store.get('1',id);if(job.status===status)return store.status(job);if(job.status==='failed')throw new Error(job.code);}throw new Error('did not reach '+status);}
const doneEngine={login:async()=>({credential:credential()})};
function make(t,options={}){const s=new JobStore({engineFor:()=>doneEngine,...options});t.after(()=>s.close());return s;}

test('ready result retained for idempotent import; status never includes credentials',async t=>{
 const store=make(t);const created=store.create('1',input);assert.equal(created.status,'running');assert.equal(created.id.length,32);await settle(store,created.id);
 const status=JSON.stringify(store.status(store.get('1',created.id)));
 for(const secret of [input.password,input.totp_secret,credential().access_token,credential().refresh_token])assert.ok(!status.includes(secret));
 assert.deepEqual(store.result('1',created.id),store.result('1',created.id));
 assert.throws(()=>store.get('2',created.id),{code:'NOT_FOUND'});assert.throws(()=>store.result('2',created.id),{code:'NOT_FOUND'});assert.throws(()=>store.cancel('2',created.id),{code:'NOT_FOUND'});
 assert.equal(store.cancel('1',created.id).status,'cancelled');assert.throws(()=>store.get('1',created.id),{code:'NOT_FOUND'});
});

test('email OTP/workspace waits accept only current authorized choice',async t=>{
 const seen=[];const store=make(t,{engineFor:()=>({login:async(_r,options)=>{
  seen.push(await options.requestEmailCode({signal:options.signal}));
  seen.push(await options.requestWorkspaceSelection({signal:options.signal,choices:[{id:'a',name:'Team A',kind:'team'},{id:'b',name:'Team B',kind:'team'}]}));return {credential:credential()};
 }})});const job=store.create('1',input);await settle(store,job.id,'awaiting_input');assert.equal(store.status(store.get('1',job.id)).challenge_kind,'email_otp');
 assert.throws(()=>store.input('2',job.id,{kind:'email_otp',value:'123456'}),{code:'NOT_FOUND'});assert.throws(()=>store.input('1',job.id,{kind:'email_otp',value:'oops'}),{code:'EMAIL_CODE_INVALID'});store.input('1',job.id,{kind:'email_otp',value:'123456'});await settle(store,job.id,'awaiting_input');assert.throws(()=>store.input('1',job.id,{kind:'workspace',value:'not-offered'}),{code:'WORKSPACE_NOT_FOUND'});store.input('1',job.id,{kind:'workspace',value:'b'});await settle(store,job.id);assert.deepEqual(seen,['123456','b']);assert.throws(()=>store.input('1',job.id,{kind:'workspace',value:'a'}),{code:'INPUT_NOT_EXPECTED'});
});

test('cancellation before start and during wait erases data and settles capacity',async t=>{
 const store=make(t,{engineFor:()=>({login:async(_r,{requestEmailCode,signal})=>{await requestEmailCode({signal});return {credential:credential()};}})});
 const early=store.create('1',input);store.cancel('1',early.id);await tick();assert.equal(store.active,0);
 const waiting=store.create('1',input);await settle(store,waiting.id,'awaiting_input');store.cancel('1',waiting.id);await tick();assert.equal(store.active,0);assert.equal(store.jobs.size,0);
});

test('ten minute absolute TTL also erases ready credentials and waits',async t=>{
 let now=0;const store=make(t,{clock:()=>now});const job=store.create('1',input);await settle(store,job.id);now=599999;assert.ok(store.result('1',job.id).credential);now=600000;assert.throws(()=>store.result('1',job.id),{code:'NOT_FOUND'});assert.equal(store.jobs.size,0);
});

test('bounded capacity reserves running/awaiting jobs and caps terminal entries',async t=>{
 const store=make(t,{engineFor:()=>({login:async(_r,{requestEmailCode,signal})=>requestEmailCode({signal})})});const a=store.create('1',input);const b=store.create('1',input);assert.throws(()=>store.create('1',input),{code:'CAPACITY_EXCEEDED'});store.cancel('1',a.id);store.cancel('1',b.id);await tick();assert.equal(store.active,0);
 const full=make(t);for(let i=0;i<32;i++){const item=full.create('1',input);await settle(full,item.id);}assert.throws(()=>full.create('1',input),{code:'CAPACITY_EXCEEDED'});
});

test('unexpected failures/progress are fixed codes; no raw secrets leak',async t=>{
 const store=make(t,{engineFor:()=>({login:async(_r,{onProgress})=>{onProgress({phase:input.password,code:input.totp_secret,status:'running'});throw new Error(input.password+' '+input.totp_secret);}})});const job=store.create('1',input);for(let i=0;i<5;i++)await tick();const status=store.status(store.get('1',job.id));assert.equal(status.status,'failed');assert.equal(status.code,'LOGIN_STEP_FAILED');assert.equal(status.phase,'session');assert.ok(!JSON.stringify(status).includes(input.password));assert.throws(()=>store.result('1',job.id),{code:'RESULT_NOT_READY'});
});

test('input/proxy validation fails closed instead of bypassing proxy',()=>{
 for(const proxy of ['bad','ftp://127.0.0.1','http://127.0.0.1:99999','http://127.0.0.1?token=a','http://127.0.0.1/path','http://a b/'])assert.throws(()=>validateProxy(proxy));
 assert.equal(validateProxy('http://name:pass@127.0.0.1:10809/'),'http://name:pass@127.0.0.1:10809');assert.equal(validateProxy(''),'');
 assert.throws(()=>validateInput({...input,totp_secret:'123456'}),{code:'TOTP_SECRET_INVALID'});assert.throws(()=>validateInput({...input,proxy_url:'bad'}),{code:'INVALID_PROXY'});assert.throws(()=>validateInput({...input,extra:1}),{code:'INVALID_INPUT'});assert.equal(validateInput({email:input.email,password:input.password}).workspacePreference,'team_first');
});

test('synthetic CPA password/TOTP/email-OTP/state/PKCE full flow issues BPS pair',async t=>{
 const trace=[];let context;const store=make(t,{engineFor:()=>createLoginEngine({
  createClient:async c=>{context=c;return {bootstrapAuthorize:async url=>{const u=new URL(url);assert.equal(u.searchParams.get('client_id'),ADDIN_AUTHORIZATION.clientId);assert.equal(u.searchParams.get('state'),c.expectedState);trace.push('authorize');},submitEmail:async()=>({page:{type:'password'}}),submitPassword:async password=>{assert.equal(password,input.password);return {page:{type:'mfa',payload:{factor_id:'factor1',factor_type:'totp'}}};},issueMfaChallenge:async()=>trace.push('mfa'),verifyMfaTotp:async(_factor,code)=>{assert.equal(code,'123456');return {page:{type:'email_otp'}};},submitEmailOtp:async code=>{assert.equal(code,'654321');return {page:{type:'consent'}};},getAuthorizationCode:async()=>({code:'synthetic-code',state:c.expectedState,workspaceId:'workspace-a'}),close:async()=>trace.push('close')};},
  totpProvider:()=> '123456',exchangeCode:async values=>{assert.equal(values.code,'synthetic-code');assert.equal(values.clientId,ADDIN_AUTHORIZATION.clientId);assert.ok(values.verifier.length>=43);trace.push('token');const claims={client_id:ADDIN_AUTHORIZATION.clientId,sub:'user-a',email:input.email,exp:Math.floor(Date.now()/1000)+3600,'https://api.openai.com/auth':{chatgpt_account_id:'workspace-a',chatgpt_user_id:'user-a'}};return {access_token:'e30.'+Buffer.from(JSON.stringify(claims)).toString('base64url')+'.test',refresh_token:'synthetic-refresh',expires_in:3600};},
 })});const j=store.create('1',input);await settle(store,j.id,'awaiting_input');store.input('1',j.id,{kind:'email_otp',value:'654321'});await settle(store,j.id);assert.equal(store.result('1',j.id).credential.client_id,ADDIN_AUTHORIZATION.clientId);assert.deepEqual(trace,['authorize','mfa','token','close']);assert.equal(context.password,input.password);
});

test('private HTTP API requires key and owner and never returns token on status',async t=>{
 const app=createServer({key,engineFor:()=>doneEngine});await new Promise(r=>app.server.listen(0,'127.0.0.1',r));t.after(()=>app.close());const base='http://127.0.0.1:'+app.server.address().port;
 const req=async(path,{method='GET',owner='1',auth=key,body}={})=>fetch(base+path,{method,headers:{authorization:'Bearer '+auth,'x-sub2-bps-owner':owner,'content-type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});
 assert.deepEqual(await(await req('/healthz')).json(),{ready:true});assert.equal((await req('/internal/v1/jobs',{method:'POST',auth:'bad',body:input})).status,401);assert.equal((await req('/internal/v1/jobs',{method:'POST',owner:'',body:input})).status,400);
 const created=await(await req('/internal/v1/jobs',{method:'POST',body:input})).json();await tick();const status=await(await req('/internal/v1/jobs/'+created.id)).json();assert.equal(status.status,'ready');assert.ok(!JSON.stringify(status).includes(credential().refresh_token));assert.equal((await req('/internal/v1/jobs/'+created.id,{owner:'2'})).status,404);assert.equal((await req('/internal/v1/jobs/'+created.id+'/result',{method:'POST'})).status,200);assert.equal((await req('/internal/v1/jobs/'+created.id,{method:'DELETE'})).status,200);assert.equal((await req('/internal/v1/jobs/'+created.id)).status,404);
});
