import { AsyncLocalStorage } from 'node:async_hooks';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { loadLegacyWorkerFactory } from '../upstream/server/worker-host.js';
import { createLegacyTransportBridge } from '../upstream/server/legacy-transport-bridge.js';
import { ADDIN_AUTHORIZATION, createLoginEngine, createHttpsTokenExchange } from '../upstream/server/login-engine.js';
import { LoginFlowError } from '../upstream/server/login-state-machine.js';

export const SDK_SHA256='49d0284bf3eea8a59ebcad0e6b5dd8a53edd4c72606f15bbf51ebe5610a88efd';
export const RUNTIME_ROOT=fileURLToPath(new URL('../runtime/',import.meta.url));
const fail=code=>new LoginFlowError(code);

export function validateProxy(raw){
 if(raw===undefined||raw===null||raw==='')return '';
 if(typeof raw!=='string'||raw.length>2048||/[\s\x00-\x1f\x7f]/.test(raw))throw fail('LOGIN_REQUEST_REJECTED');
 let u;try{u=new URL(raw);}catch{throw fail('LOGIN_REQUEST_REJECTED');}
 if(!['http:','https:','socks5:','socks5h:'].includes(u.protocol)||!u.hostname||u.hash||u.search||(u.pathname!==''&&u.pathname!=='/')||(u.port&&(!/^\d+$/.test(u.port)||Number(u.port)<1||Number(u.port)>65535)))throw fail('LOGIN_REQUEST_REJECTED');
 return u.toString().replace(/\/$/,'');
}

// Same per-job proxy governs the token hop too; the CPA default fetch exchange
// would otherwise bypass the native login transport's proxy.
export function createNativeTokenExchange({tls,rtClientIdentifier,headerOrderFor,hardenCookieJar,proxyUrl=''}){
 const fixedProxy=validateProxy(proxyUrl);
 return createHttpsTokenExchange({fetchImpl:async(url,options)=>{
  if(String(url)!==ADDIN_AUTHORIZATION.tokenUrl||options.method!=='POST'||options.redirect!=='error')throw fail('TOKEN_ENDPOINT_NOT_ALLOWED');
  if(options.signal?.aborted)throw fail('CANCELLED');
  const session=hardenCookieJar(new tls.Session({clientIdentifier:rtClientIdentifier(),timeout:30000,followRedirects:false,...(fixedProxy?{proxy:fixedProxy}:{})}));
  let closePromise;const close=()=>closePromise||=(Promise.resolve().then(()=>session.close()).catch(()=>{}));
  const abort=()=>{void close();};options.signal?.addEventListener('abort',abort,{once:true});
  try{
   const headers={...options.headers};
   const response=await session.post(String(url),{headers,headerOrder:headerOrderFor(Object.keys(headers)),body:String(options.body),followRedirects:false});
   if(options.signal?.aborted)throw fail('CANCELLED');
   const status=Number(response?.status);if(!Number.isInteger(status)||status<100||status>599)throw fail('TOKEN_RESPONSE_INVALID');
   const text=await response.text();if(typeof text!=='string'||Buffer.byteLength(text)>262144)throw fail('TOKEN_RESPONSE_INVALID');
   if(options.signal?.aborted)throw fail('CANCELLED');
   return {status,ok:status>=200&&status<300,json:async()=>JSON.parse(text)};
  }finally{options.signal?.removeEventListener('abort',abort);await Promise.race([close(),new Promise(r=>{const t=setTimeout(r,2000);t.unref();})]);}
 }});
}

// Reuses CPA's reviewed, hash-checked paired SDK host in-process. Only the SDK
// sandbox uses a child process; private state travels over stdin/IPC, never argv.
export async function createNativeRuntime({runtimeRoot=RUNTIME_ROOT,factoryLoader=loadLegacyWorkerFactory,dependencies}={}){
 const storage=new AsyncLocalStorage();
 const factory=await factoryLoader(runtimeRoot,storage,{sdkContextMode:'paired',approvedSdkSha256:SDK_SHA256,workerKeyConfigured:true});
 const requireRuntime=createRequire(resolve(runtimeRoot,'package.json'));
 const bootstrap=requireRuntime('./src/tlsClientBootstrap.js');
 const tls=dependencies?.tls||bootstrap.requireTlsClient();
 const rtClientIdentifier=dependencies?.rtClientIdentifier||requireRuntime('./src/tlsProfile.js').rtClientIdentifier;
 const headerOrderFor=dependencies?.headerOrderFor||requireRuntime('./src/chromeHeaderOrder.js').headerOrderFor;
 return {
  ready:true,
  engineFor(record){
   const proxyUrl=validateProxy(record.proxyUrl);
   const createClient=async context=>{
    const controller=new AbortController();const children=new Set();const owner={controller,children};
    let native;let bridge;let closing;
    const close=()=>{
     if(closing)return closing;
     controller.abort();for(const child of children){try{child.kill('SIGKILL');}catch{}}
     context.signal?.removeEventListener('abort',abort);
     closing=Promise.resolve().then(()=>bridge?.close()??native?.close()).catch(()=>{});return closing;
    };
    const abort=()=>{void close();};context.signal?.addEventListener('abort',abort,{once:true});
    try{
     if(context.signal?.aborted)throw fail('CANCELLED');
     native=await storage.run(owner,()=>factory({...context,proxyUrl,authorization:ADDIN_AUTHORIZATION,signal:controller.signal}));
     if(controller.signal.aborted){await native?.close();throw fail('CANCELLED');}
     bridge=createLegacyTransportBridge(native,{authorization:ADDIN_AUTHORIZATION,signal:controller.signal,workspacePreference:context.workspacePreference});
     const result={close};
     for(const method of ['bootstrapAuthorize','submitEmail','submitPassword','issueMfaChallenge','verifyMfaTotp','submitEmailOtp'])result[method]=(...args)=>storage.run(owner,()=>bridge[method](...args));
     result.getAuthorizationCode=async(data,options={})=>{
      try{return await storage.run(owner,()=>bridge.getAuthorizationCode(data,options));}
      catch(error){
       if(error?.code!=='WORKSPACE_SELECTION_REQUIRED'||context.targetWorkspaceId||typeof context.requestWorkspaceSelection!=='function')throw error;
       const choices=error.workspaceCandidates;
       if(!Array.isArray(choices)||!choices.length||choices.length>100)throw fail('INVALID_LOGIN_RESPONSE');
       const selectedWorkspaceId=await context.requestWorkspaceSelection({choices,signal:controller.signal});
       if(controller.signal.aborted)throw fail('CANCELLED');
       if(!choices.some(choice=>choice.id===selectedWorkspaceId))throw fail('WORKSPACE_NOT_FOUND');
       return storage.run(owner,()=>bridge.getAuthorizationCode(data,{...options,selectedWorkspaceId}));
      }
     };
     return result;
    }catch(error){await close();throw error;}
   };
   return createLoginEngine({createClient,exchangeCode:createNativeTokenExchange({tls,rtClientIdentifier,headerOrderFor,hardenCookieJar:bootstrap.hardenCookieJar,proxyUrl}),
    lookupWorkspaces:(token,options)=>factory.workspaceLookup(token,{...options,proxyUrl}),
    verifyBusinessUsage:(token,options)=>factory.businessUsageLookup(token,{...options,proxyUrl}),
    timeoutMs:600000,authRetries:0,
   });
  },
 };
}
