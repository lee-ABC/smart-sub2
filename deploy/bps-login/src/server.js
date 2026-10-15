import http from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { JobStore } from './jobs.js';

export function createServer({key,engineFor,ready=()=>true,storeOptions={}}){
 if(typeof key!=='string'||!/^[A-Za-z0-9_-]{32,256}$/.test(key))throw new Error('LOGIN_KEY_INVALID');
 const expected=Buffer.from('Bearer '+key);const store=new JobStore({engineFor,ready,...storeOptions});
 const send=(res,status,body)=>{if(res.destroyed||res.writableEnded)return;res.writeHead(status,{'Content-Type':'application/json','Cache-Control':'no-store','X-Content-Type-Options':'nosniff'});res.end(JSON.stringify(body));};
 const body=async req=>{if(!String(req.headers['content-type']||'').startsWith('application/json'))throw Object.assign(new Error(),{code:'INVALID_INPUT',status:400});const chunks=[];let size=0;for await(const chunk of req){size+=chunk.length;if(size>32768)throw Object.assign(new Error(),{code:'BODY_TOO_LARGE',status:413});chunks.push(chunk);}try{return JSON.parse(Buffer.concat(chunks).toString('utf8'));}catch{throw Object.assign(new Error(),{code:'INVALID_INPUT',status:400});}};
 const server=http.createServer(async(req,res)=>{
  try{
   const url=new URL(req.url,'http://private.invalid');
   if(req.method==='GET'&&url.pathname==='/healthz'){send(res,200,{ready:ready()&&!store.stopped});return;}
   const supplied=Buffer.from(typeof req.headers.authorization==='string'?req.headers.authorization:'');
   if(supplied.length!==expected.length||!timingSafeEqual(supplied,expected)){send(res,401,{error:{code:'UNAUTHORIZED'}});return;}
   const owner=req.headers['x-sub2-bps-owner'];if(typeof owner!=='string'||!/^\d{1,20}$/.test(owner)||/^0+$/.test(owner)){send(res,400,{error:{code:'INVALID_OWNER'}});return;}
   if(url.search||req.headers.origin!==undefined){send(res,400,{error:{code:'INVALID_INPUT'}});return;}
   if(req.method==='POST'&&url.pathname==='/internal/v1/jobs'){send(res,201,store.create(owner,await body(req)));return;}
   const match=/^\/internal\/v1\/jobs\/([A-Za-z0-9_-]{32})(?:\/(input|result))?$/.exec(url.pathname);
   if(!match){send(res,404,{error:{code:'NOT_FOUND'}});return;}
   const [,id,action]=match;
   if(req.method==='GET'&&!action){send(res,200,store.status(store.get(owner,id)));return;}
   if(req.method==='DELETE'&&!action){send(res,200,store.cancel(owner,id));return;}
   if(req.method==='POST'&&action==='input'){send(res,200,store.input(owner,id,await body(req)));return;}
   if(req.method==='POST'&&action==='result'){send(res,200,store.result(owner,id));return;}
   send(res,405,{error:{code:'METHOD_NOT_ALLOWED'}});
  }catch(error){const known=new Set(['INVALID_INPUT','INVALID_PROXY','TOTP_SECRET_INVALID','BODY_TOO_LARGE','LOGIN_UNAVAILABLE','CAPACITY_EXCEEDED','NOT_FOUND','INPUT_NOT_EXPECTED','EMAIL_CODE_INVALID','WORKSPACE_NOT_FOUND','RESULT_NOT_READY']);send(res,known.has(error?.code)?(error.status||400):500,{error:{code:known.has(error?.code)?error.code:'INTERNAL_ERROR'}});}
 });
 server.requestTimeout=15000;server.headersTimeout=10000;server.keepAliveTimeout=5000;server.maxHeadersCount=40;
 return {server,store,async close(){await store.close();server.closeAllConnections();if(server.listening)await new Promise(resolve=>server.close(resolve));}};
}
