import test from 'node:test';
import assert from 'node:assert/strict';
import {scopedSdkExports} from '../upstream/server/worker-host.js';

test('balanced login dwell keeps randomized anti-risk bounds while reducing excessive waits',()=>{
 const previous=process.env.BPS_LOGIN_DWELL_PROFILE;const random=Math.random;process.env.BPS_LOGIN_DWELL_PROFILE='balanced';
 try{
  class Base{constructor(options){this.options=options;} _dwellMsFor(){return 99999;}}
  const Client=scopedSdkExports({SentinelClient:Base},'paired').SentinelClient;const client=new Client({});assert.equal(client.options.ipcHandshake,true);
  Math.random=()=>0;assert.equal(client._dwellMsFor('login'),5000);assert.equal(client._dwellMsFor('create_account'),13250);
  Math.random=()=>1;assert.equal(client._dwellMsFor('login'),9000);assert.equal(client._dwellMsFor('create_account'),16000);
 } finally {Math.random=random;if(previous===undefined)delete process.env.BPS_LOGIN_DWELL_PROFILE;else process.env.BPS_LOGIN_DWELL_PROFILE=previous;}
});

test('legacy dwell remains untouched unless balanced profile is explicit',()=>{
 delete process.env.BPS_LOGIN_DWELL_PROFILE;class Base{_dwellMsFor(){return 12345;}}const Client=scopedSdkExports({SentinelClient:Base},'paired').SentinelClient;assert.equal(new Client()._dwellMsFor('login'),12345);
});
