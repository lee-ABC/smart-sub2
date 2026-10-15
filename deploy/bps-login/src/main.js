import { readWorkerKey } from '../upstream/server/worker-client.js';
import { createNativeRuntime } from './native-runtime.js';
import { createServer } from './server.js';

let runtime;
try{runtime=await createNativeRuntime();}catch{runtime={ready:false};}
if(process.argv.includes('--check-native')){process.stdout.write(JSON.stringify({ready:runtime.ready===true})+'\n');process.exit(runtime.ready===true?0:1);}
let key;try{key=readWorkerKey(process.env.BPS_LOGIN_KEY_FILE||'/run/secrets/login-key');}catch{process.stderr.write('BPS_LOGIN_KEY_UNAVAILABLE\n');process.exit(1);}
const app=createServer({key,ready:()=>runtime.ready===true,engineFor:record=>runtime.engineFor(record)});
const port=Number(process.env.BPS_LOGIN_PORT||8001);if(!Number.isInteger(port)||port<1||port>65535){process.stderr.write('BPS_LOGIN_PORT_INVALID\n');process.exit(1);}
app.server.listen(port,'0.0.0.0',()=>{process.stdout.write(`Private BPS login listening; ready=${runtime.ready===true}\n`);});
app.server.on('error',()=>{process.stderr.write('BPS_LOGIN_SERVER_FAILED\n');process.exit(1);});
let stopping=false;const stop=()=>{if(stopping)return;stopping=true;void app.close().finally(()=>process.exit(0));setTimeout(()=>process.exit(1),10000).unref();};
process.once('SIGTERM',stop);process.once('SIGINT',stop);
