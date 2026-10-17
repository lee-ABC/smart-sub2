import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {createNativeRuntime,createNativeTokenExchange,SDK_SHA256} from '../src/native-runtime.js';
import {ADDIN_AUTHORIZATION} from '../upstream/server/login-engine.js';
import {createNativeWorkspaceInspector,WORKSPACE_CHECK_URL} from '../upstream/server/workspace-inspector.js';
import {createNativeBusinessUsageVerifier,BUSINESS_USAGE_URL} from '../upstream/server/business-usage-verifier.js';

const proxy='http://proxyuser:proxypass@127.0.0.1:10809';
const email='synthetic@example.test';
const token=()=> 'e30.'+Buffer.from(JSON.stringify({client_id:ADDIN_AUTHORIZATION.clientId,sub:'synthetic-user',email,exp:Math.floor(Date.now()/1000)+3600,'https://api.openai.com/auth':{chatgpt_account_id:'business',chatgpt_user_id:'synthetic-user',chatgpt_plan_type:'team'}})).toString('base64url')+'.synthetic';
const proof=()=>({workspaces:[{id:'business',name:'Synthetic Team',kind:'business',plan:'team',active:true,expiresAt:null,deactivated:false,viewOnly:false,aliases:['business']}],order:['business'],complete:true});
const raw=()=>({accounts:{business:{account:{account_id:'business',name:'Synthetic Team',plan_type:'team',is_deactivated:false,is_view_only:false},entitlement:{subscription_plan:'chatgptteamplan',has_active_subscription:true,expires_at:null}}},account_ordering:['business']});

test('reviewed paired SDK and Cookie fix are pinned; native readiness has no network',()=>{
 assert.equal(createHash('sha256').update(readFileSync(new URL('../runtime/sentinel/sdk.js',import.meta.url))).digest('hex'),SDK_SHA256);
 const result=spawnSync(process.execPath,['src/main.js','--check-native'],{cwd:new URL('../',import.meta.url),encoding:'utf8',timeout:30000});assert.equal(result.status,0,result.stderr);assert.deepEqual(JSON.parse(result.stdout.trim()),{ready:true});
});

test('native proxy reaches authorize/password/TOTP/workspace select/token and Business lookup',async()=>{
 const trace=[];let authState,prewarmed=0;
 const factory=async context=>{assert.equal(context.proxyUrl,proxy);const native={lastSession:{workspaces:[{id:'business',name:'Team',kind:'team'}]},
  _sentinel:async()=>({}),bootstrapAuthorize:async url=>{authState=new URL(url).searchParams.get('state');trace.push(['authorize',context.proxyUrl]);},
  _postWithSentinel:async(path)=>{trace.push([path,context.proxyUrl]);return {status:200,data:{page:path.includes('password')?{type:'mfa',payload:{factor_id:'f',factor_type:'totp'}}:{type:'password'}}};},
  issueMfaChallenge:async()=>trace.push(['issue-mfa',context.proxyUrl]),verifyMfaTotp:async()=>{trace.push(['verify-mfa',context.proxyUrl]);return {page:{type:'consent'}};},
  _post:async path=>{trace.push([path,context.proxyUrl]);return {status:200,data:{page:{type:'external_url',payload:{url:ADDIN_AUTHORIZATION.redirectUri+'?code=fake-code&state='+authState}}}};},
  _getRaw:async()=>{throw new Error('unexpected redirect hop');},close:async()=>{},
 };return native;};
 factory.workspaceLookup=async(_token,options)=>{assert.equal(options.proxyUrl,proxy);trace.push(['workspace-check',options.proxyUrl]);return proof();};
 factory.businessUsageLookup=async(_token,options)=>{assert.equal(options.proxyUrl,proxy);return {accountId:'business',plan:'team',source:'wham_usage',checkedAt:new Date().toISOString(),allowed:true,limitReached:false};};
 class Session{constructor(options){assert.equal(options.proxy,proxy);trace.push(['token-session',options.proxy]);}async post(url,options){assert.equal(url,ADDIN_AUTHORIZATION.tokenUrl);assert.equal(options.followRedirects,false);return {status:200,text:async()=>JSON.stringify({access_token:token(),refresh_token:'synthetic-refresh',expires_in:3600})};}async close(){}}
 const runtime=await createNativeRuntime({factoryLoader:async(_root,_storage,options)=>{assert.equal(options.sdkContextMode,'paired');assert.equal(options.approvedSdkSha256,SDK_SHA256);return factory;},dependencies:{tls:{Session,initTLS:async()=>{prewarmed++;}},rtClientIdentifier:()=> 'fixture',headerOrderFor:x=>x}});
 const record={email,password:'synthetic-password',totpSecret:'JBSWY3DPEHPK3PXP',workspacePreference:'team_first',proxyUrl:proxy};
 const result=await runtime.engineFor(record).login(record);assert.equal(result.credential.account_id,'business');assert.equal(prewarmed,1);
 for(const name of ['authorize','/api/accounts/authorize/continue','/api/accounts/password/verify','issue-mfa','verify-mfa','/api/accounts/workspace/select','token-session','workspace-check'])assert.ok(trace.some(row=>row[0]===name&&row[1]===proxy),name);
});

test('workspace and usage native Session constructors both apply selected proxy',async()=>{
 const calls=[];class Session{constructor(options){calls.push(options);}async get(url){return {status:200,text:async()=>JSON.stringify(url===WORKSPACE_CHECK_URL?raw():{plan_type:'team',rate_limit:{allowed:true,limit_reached:false}})};}async close(){}}
 const deps={tls:{Session},rtClientIdentifier:()=> 'fixture',headerOrderFor:x=>x};
 assert.equal((await createNativeWorkspaceInspector(deps)(token(),{proxyUrl:proxy})).workspaces[0].id,'business');
 assert.equal((await createNativeBusinessUsageVerifier(deps)(token(),{accountId:'business',proxyUrl:proxy})).accountId,'business');
 assert.equal(calls.length,2);for(const call of calls){assert.equal(call.proxy,proxy);assert.equal(call.followRedirects,false);}
});

test('token exchange refuses redirects, mismatched endpoints, bad proxy and closes on cancel',async()=>{
 let calls=0,closed=0;class Session{async post(){calls++;return {status:302,text:async()=> '{}'};}async close(){closed++;}}
 const deps={tls:{Session},rtClientIdentifier:()=> 'fixture',headerOrderFor:x=>x,hardenCookieJar:x=>x,proxyUrl:proxy};
 const exchange=createNativeTokenExchange(deps);const args={code:'synthetic',verifier:'verifier',redirectUri:ADDIN_AUTHORIZATION.redirectUri,clientId:ADDIN_AUTHORIZATION.clientId,tokenUrl:ADDIN_AUTHORIZATION.tokenUrl};
 await assert.rejects(exchange(args),{code:'TOKEN_EXCHANGE_REJECTED'});assert.equal(calls,1);assert.equal(closed,1);
 await assert.rejects(exchange({...args,tokenUrl:'https://auth.openai.com/arbitrary'}),{code:'TOKEN_ENDPOINT_NOT_ALLOWED'});await assert.rejects(exchange({...args,signal:AbortSignal.abort()}),{code:'CANCELLED'});assert.equal(calls,1);
 assert.throws(()=>createNativeTokenExchange({...deps,proxyUrl:'invalid'}));
});
