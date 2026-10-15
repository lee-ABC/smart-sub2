import { randomBytes } from 'node:crypto';
import { sanitizeLoginError } from '../upstream/server/login-state-machine.js';
import { decodeBase32 } from '../upstream/shared/totp.js';
import { validateProxy } from './native-runtime.js';

const phases=new Set(['session','email','password','mfa','email_otp','authorization','exchange','validate']);
const codes=new Set(['SESSION_STARTED','SESSION_READY','EMAIL_STARTED','EMAIL_ACCEPTED','PASSWORD_STARTED','PASSWORD_VERIFIED','MFA_STARTED','MFA_VERIFIED','EMAIL_OTP_STARTED','EMAIL_CODE_REQUIRED','EMAIL_CODE_VERIFIED','AUTHORIZATION_STARTED','AUTHORIZATION_RETRY','AUTHORIZATION_COMPLETED','TOKEN_EXCHANGE_STARTED','TOKEN_EXCHANGE_COMPLETED','TOKEN_VALIDATION_STARTED','WORKSPACE_CHECK_STARTED','WORKSPACE_CHECK_RETRY','WORKSPACE_CHECK_COMPLETED','CREDENTIAL_VALIDATED']);
const terminal=new Set(['ready','failed','cancelled']);
const err=(code,status=400)=>Object.assign(new Error(code),{code,status});
const object=x=>x&&typeof x==='object'&&!Array.isArray(x);
const cleanText=(value,max)=>typeof value==='string'&&value.length<=max&&!/[\x00-\x1f\x7f]/.test(value);

export function validateInput(input){
 if(!object(input))throw err('INVALID_INPUT');
 const allowed=new Set(['email','password','totp_secret','workspace_id','workspace_preference','proxy_url']);
 if(Object.keys(input).some(k=>!allowed.has(k)))throw err('INVALID_INPUT');
 const email=typeof input.email==='string'?input.email.trim():'';
 if(!cleanText(email,320)||!/^[^\s@]+@[^\s@]+$/.test(email)||!cleanText(input.password,1000)||!input.password||!cleanText(input.totp_secret??'',512))throw err('INVALID_INPUT');
 if(input.totp_secret){try{decodeBase32(input.totp_secret);}catch{throw err('TOTP_SECRET_INVALID');}}
 const workspaceId=input.workspace_id??'';if(!cleanText(workspaceId,256))throw err('INVALID_INPUT');
 const workspacePreference=input.workspace_preference??'team_first';if(!['team_first','personal','team','first'].includes(workspacePreference))throw err('INVALID_INPUT');
 let proxyUrl;try{proxyUrl=validateProxy(input.proxy_url);}catch{throw err('INVALID_PROXY');}
 return {email,password:input.password,totpSecret:input.totp_secret??'',workspaceId,workspacePreference,proxyUrl};
}

export class JobStore{
 constructor({engineFor,ready=()=>true,clock=Date.now,ttlMs=600000,maxActive=5,maxJobs=128}={}){
  this.engineFor=engineFor;this.ready=ready;this.clock=clock;this.ttlMs=ttlMs;this.maxActive=maxActive;this.maxJobs=maxJobs;this.jobs=new Map();this.active=0;this.stopped=false;
  this.sweeper=setInterval(()=>this.sweep(),1000);this.sweeper.unref();
 }
 sweep(){for(const job of this.jobs.values())if(this.clock()>=job.expires)this.erase(job);}
 erase(job){job.controller.abort();if(job.pending){job.pending.reject(err('CANCELLED'));job.pending=null;}job.credential=null;if(job.record){job.record.password='';job.record.totpSecret='';job.record.proxyUrl='';}job.record=null;job.preview=null;job.status='cancelled';job.code='CANCELLED';this.jobs.delete(job.id);}
 get(owner,id){this.sweep();const job=this.jobs.get(id);if(!job||job.owner!==owner)throw err('NOT_FOUND',404);return job;}
 status(job){return {id:job.id,status:job.status,phase:job.phase,code:job.code,expires_at:new Date(job.expires).toISOString(),...(job.pending?{challenge_kind:job.pending.kind,...(job.pending.choices?{workspace_choices:job.pending.choices}: {})}:{}),...(job.preview?{preview:{...job.preview}}:{})};}
 create(owner,input){
  this.sweep();if(this.stopped||!this.ready()||typeof this.engineFor!=='function')throw err('LOGIN_UNAVAILABLE',503);
  if(this.active>=this.maxActive||this.jobs.size>=this.maxJobs)throw err('CAPACITY_EXCEEDED',429);
  const record=validateInput(input);const job={id:randomBytes(24).toString('base64url'),owner,status:'running',phase:'session',code:'SESSION_STARTED',expires:this.clock()+this.ttlMs,record,credential:null,preview:null,pending:null,controller:new AbortController()};
  this.jobs.set(job.id,job);this.active++;queueMicrotask(()=>{void this.run(job);});return this.status(job);
 }
 async wait(job,kind,{choices,signal}={}){
  if(job.controller.signal.aborted||signal?.aborted)throw err('CANCELLED');
  if(job.pending)throw err('INPUT_ALREADY_PENDING',409);
  let safeChoices;
  if(kind==='workspace'){
   if(!Array.isArray(choices)||!choices.length||choices.length>100)throw err('INVALID_LOGIN_RESPONSE');
   safeChoices=choices.map(c=>{if(!cleanText(c?.id,256)||!c.id||!cleanText(c?.name,80)||!['team','personal','unknown'].includes(c.kind))throw err('INVALID_LOGIN_RESPONSE');return {id:c.id,name:c.name,kind:c.kind};});
   if(new Set(safeChoices.map(c=>c.id)).size!==safeChoices.length)throw err('INVALID_LOGIN_RESPONSE');
   const serialized=JSON.stringify(safeChoices);if([job.record.password,job.record.totpSecret].some(s=>s.length>=4&&serialized.includes(s)))throw err('INVALID_LOGIN_RESPONSE');
  }
  job.status='awaiting_input';job.phase=kind==='email_otp'?'email_otp':'authorization';job.code=kind==='email_otp'?'EMAIL_CODE_REQUIRED':'WORKSPACE_SELECTION_REQUIRED';
  return new Promise((resolve,reject)=>{
   const abort=()=>finish(reject,err('CANCELLED'));
   const finish=(fn,value)=>{signal?.removeEventListener('abort',abort);job.controller.signal.removeEventListener('abort',abort);job.pending=null;if(!job.controller.signal.aborted){job.status='running';job.code=kind==='email_otp'?'EMAIL_OTP_STARTED':'AUTHORIZATION_STARTED';}fn(value);};
   job.pending={kind,choices:safeChoices,resolve:value=>finish(resolve,value),reject:error=>finish(reject,error)};
   signal?.addEventListener('abort',abort,{once:true});job.controller.signal.addEventListener('abort',abort,{once:true});
  });
 }
 input(owner,id,body){const job=this.get(owner,id);if(!object(body)||Object.keys(body).length!==2||!['email_otp','workspace'].includes(body.kind)||typeof body.value!=='string')throw err('INVALID_INPUT');if(job.status!=='awaiting_input'||!job.pending||job.pending.kind!==body.kind)throw err('INPUT_NOT_EXPECTED',409);if(body.kind==='email_otp'&&!/^\d{6,8}$/.test(body.value))throw err('EMAIL_CODE_INVALID');if(body.kind==='workspace'&&!job.pending.choices?.some(c=>c.id===body.value))throw err('WORKSPACE_NOT_FOUND');job.pending.resolve(body.value);return this.status(job);}
 result(owner,id){const job=this.get(owner,id);if(job.status!=='ready'||!job.credential)throw err('RESULT_NOT_READY',409);return {credential:{...job.credential}};}
 cancel(owner,id){const job=this.get(owner,id);this.erase(job);return {id,status:'cancelled',phase:job.phase,code:'CANCELLED',expires_at:new Date(job.expires).toISOString()};}
 async run(job){
  const record=job.record;
  try{
   if(job.controller.signal.aborted)return;
   const engine=await this.engineFor(record);
   const result=await engine.login(record,{signal:job.controller.signal,
    onProgress:event=>{if(job.controller.signal.aborted||terminal.has(job.status)||job.status==='awaiting_input')return;if(phases.has(event.phase))job.phase=event.phase;if(codes.has(event.code))job.code=event.code;},
    requestEmailCode:options=>this.wait(job,'email_otp',options),
    requestWorkspaceSelection:options=>this.wait(job,'workspace',options),
   });
   if(job.controller.signal.aborted||!this.jobs.has(job.id)||this.clock()>=job.expires)return;
   const c=result?.credential;if(!object(c)||c.type!=='oai-basispoints'||c.client_id!=='app_fnr0pYvVwwFDocDumLG3H2Bp'||typeof c.access_token!=='string'||!c.access_token||typeof c.refresh_token!=='string'||!c.refresh_token)throw err('TOKEN_INCOMPLETE');
   job.credential=c;job.status='ready';job.phase='validate';job.code='CREDENTIAL_VALIDATED';
   const preview={email:c.email,workspace_id:c.account_id,plan_type:c.plan_type,client_id:c.client_id,expires_at:c.expired};
   for(const [key,value] of Object.entries(preview)){if(!cleanText(value,320)||[record.password,record.totpSecret,c.access_token,c.refresh_token,c.id_token].some(s=>typeof s==='string'&&s.length>=4&&value.includes(s)))delete preview[key];}
   job.preview=preview;
  }catch(error){if(this.jobs.has(job.id)&&!job.controller.signal.aborted){const safe=sanitizeLoginError(error,[record.email,record.password,record.totpSecret,record.proxyUrl]);job.status='failed';job.code=safe.code;job.credential=null;job.preview=null;}}
  finally{job.record=null;if(record){record.password='';record.totpSecret='';record.proxyUrl='';}this.active--;}
 }
 async close(){this.stopped=true;clearInterval(this.sweeper);for(const job of [...this.jobs.values()])this.erase(job);}
}
