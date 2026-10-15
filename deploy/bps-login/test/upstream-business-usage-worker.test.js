import test from 'node:test';
import assert from 'node:assert/strict';
import { getEventListeners } from 'node:events';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { randomBytes } from 'node:crypto';
import os from 'node:os';
import path from 'node:path';
import { createNativeBusinessUsageVerifier, sanitizeBusinessUsageVerification, validateBusinessUsageBinding,
  BUSINESS_USAGE_URL, BUSINESS_USAGE_TIMEOUT_MS, BUSINESS_USAGE_CLOSE_TIMEOUT_MS } from '../upstream/server/business-usage-verifier.js';
import { createWorkerHost } from '../upstream/server/worker-host.js';
import { createBusinessUsageLookup, createWorkspaceLookup } from '../upstream/server/worker-client.js';

const now = Date.parse('2026-10-13T07:00:00Z');
const accountId = 'synthetic-workspace-A';
const token = (id = accountId, extra = {}) => `synthetic.${Buffer.from(JSON.stringify({
  client_id: 'app_fnr0pYvVwwFDocDumLG3H2Bp', exp: Math.floor(now / 1000) + 3600,
  'https://api.openai.com/auth': { chatgpt_account_id: id, chatgpt_plan_type: 'team' }, ...extra,
})).toString('base64url')}.signature`;
const accessToken = token();
const proof = (id = accountId, patch = {}) => ({ accountId: id, plan: 'team', checkedAt: new Date(now).toISOString(), allowed: true, limitReached: false, source: 'wham_usage', ...patch });
const raw = (patch = {}) => ({ plan_type: 'team', rate_limit: { allowed: true, limit_reached: false }, ...patch });
const workspace = () => ({ workspaces: [{ id: accountId, name: 'Synthetic', kind: 'business', plan: 'team', active: true, expiresAt: null, deactivated: false, viewOnly: false, aliases: [accountId] }], order: [accountId], complete: true });
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
const flush = async () => { for (let index = 0; index < 25; index++) await Promise.resolve(); };

function native({ status = 200, body = raw(), getImpl, closeImpl, textImpl, throwError } = {}) {
  const calls = [];
  let closed = 0;
  class Session {
    constructor(options) { calls.push({ options }); }
    async get(url, options) {
      calls.push({ url, options });
      if (throwError) throw throwError;
      if (getImpl) return getImpl(url, options);
      return { status, text: async () => textImpl ? textImpl() : typeof body === 'string' ? body : JSON.stringify(body) };
    }
    async close() { closed++; return closeImpl?.(); }
  }
  const lookup = createNativeBusinessUsageVerifier({ tls: { Session }, rtClientIdentifier: () => 'existing-profile', headerOrderFor: keys => keys, clock: () => now });
  return { lookup, calls, get closed() { return closed; } };
}

async function rpc(t, options = {}) {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'cpa-business-usage-'));
  const keyFile = path.join(directory, 'worker.key');
  const key = randomBytes(36).toString('base64url');
  await writeFile(keyFile, key, { mode: 0o600 });
  const worker = await createWorkerHost({ keyFile, enabled: true, nativeFactory: () => ({}), workspaceLookup: async () => workspace(), lookupBusinessUsage: async (_token, { accountId: id }) => proof(id), port: 0, ...options });
  const address = await worker.listen();
  const baseUrl = `http://127.0.0.1:${address.port}`;
  t.after(async () => { await worker.close(); await rm(directory, { recursive: true, force: true }); });
  return { worker, key, keyFile, baseUrl, lookup: createBusinessUsageLookup({ keyFile, baseUrl }), workspaceLookup: createWorkspaceLookup({ keyFile, baseUrl }) };
}

test('fixed usage endpoint binds the original token/account and uses unchanged TLS profile without cookies', async () => {
  const harness = native();
  assert.deepEqual(await harness.lookup(accessToken, { accountId }), proof());
  assert.deepEqual(harness.calls[0].options, { clientIdentifier: 'existing-profile', timeout: 30000, followRedirects: false });
  const call = harness.calls[1];
  assert.equal(call.url, BUSINESS_USAGE_URL);
  assert.equal(call.options.followRedirects, false);
  assert.equal(call.options.headers.authorization, `Bearer ${accessToken}`);
  assert.equal(call.options.headers['ChatGPT-Account-ID'], accountId);
  assert.equal(Object.hasOwn(call.options.headers, 'cookie'), false);
  assert.equal(harness.closed, 1);
});

test('missing or Free raw plan never falls back to the Business JWT claim', async () => {
  for (const body of [{}, { plan_type: 'free' }, { plan_type: 'enterprise' }, { plan_type: null }]) {
    const harness = native({ body });
    await assert.rejects(harness.lookup(accessToken, { accountId }), { code: 'BUSINESS_WORKSPACE_UNVERIFIED' });
    assert.equal(harness.closed, 1);
  }
  const camel = native({ body: { planType: 'business' } });
  assert.deepEqual(await camel.lookup(accessToken, { accountId }), proof(accountId, { plan: 'business', allowed: null, limitReached: null }));
});

test('explicit Business remains Business with exhausted usage or absent usage flags', async () => {
  assert.deepEqual(await native({ body: raw({ rate_limit: { allowed: false, limit_reached: true } }) }).lookup(accessToken, { accountId }), proof(accountId, { allowed: false, limitReached: true }));
  assert.deepEqual(await native({ body: { plan_type: 'team' } }).lookup(accessToken, { accountId }), proof(accountId, { allowed: null, limitReached: null }));
  for (const rate_limit of [{ allowed: 'true' }, { limit_reached: 0 }, { allowed: null }, null, []]) {
    await assert.rejects(native({ body: raw({ rate_limit }) }).lookup(accessToken, { accountId }), { code: 'INVALID_LOGIN_RESPONSE' });
  }
  await assert.rejects(native({ body: { plan_type: 'team', planType: 'free' } }).lookup(accessToken, { accountId }), { code: 'INVALID_LOGIN_RESPONSE' });
});

test('token client/account binding is checked before networking and optional response account must match', async () => {
  const harness = native();
  for (const [value, id, code] of [[accessToken, 'other', 'TOKEN_WORKSPACE_MISMATCH'], [token(accountId, { client_id: 'other' }), accountId, 'TOKEN_CLIENT_MISMATCH'], [token(accountId, { client_id: undefined, azp: 'app_fnr0pYvVwwFDocDumLG3H2Bp' }), accountId, 'TOKEN_CLIENT_MISMATCH'], ['not-a-jwt', accountId, 'TOKEN_STRUCTURE_INVALID'], [accessToken, 'bad\naccount', 'TOKEN_WORKSPACE_MISSING']]) {
    await assert.rejects(harness.lookup(value, { accountId: id }), { code });
  }
  assert.equal(harness.calls.length, 0);
  assert.equal(validateBusinessUsageBinding(accessToken, accountId), accountId);
  for (const name of ['account_id', 'accountId']) {
    await assert.rejects(native({ body: raw({ [name]: 'other' }) }).lookup(accessToken, { accountId }), { code: 'TOKEN_WORKSPACE_MISMATCH' });
    assert.equal((await native({ body: raw({ [name]: accountId }) }).lookup(accessToken, { accountId })).accountId, accountId);
  }
});

test('DTO sanitization requires own proof fields, rejects getters/source mismatch and strips private extras', () => {
  const value = { ...proof(), access_token: accessToken, debug: { accessToken } };
  assert.deepEqual(sanitizeBusinessUsageVerification(value, { accountId, privateValues: [accessToken] }), proof());
  for (const patch of [{ source: 'jwt' }, { source: undefined }, { plan: 'free' }, { allowed: undefined }, { limitReached: 1 }, { checkedAt: '2026-02-30T00:00:00Z' }, { checkedAt: accessToken }]) {
    assert.throws(() => sanitizeBusinessUsageVerification({ ...proof(), ...patch }, { accountId }));
  }
  for (const name of Object.keys(proof())) {
    const missing = proof(); delete missing[name];
    assert.throws(() => sanitizeBusinessUsageVerification(missing, { accountId }));
    let invoked = false;
    const getter = proof(); Object.defineProperty(getter, name, { get() { invoked = true; return accessToken; } });
    assert.throws(() => sanitizeBusinessUsageVerification(getter, { accountId }));
    assert.equal(invoked, false);
  }
  assert.throws(() => sanitizeBusinessUsageVerification(proof(), { accountId: 'different' }), { code: 'TOKEN_WORKSPACE_MISMATCH' });
  assert.throws(() => sanitizeBusinessUsageVerification(proof(), { accountId, privateValues: [accountId] }), { code: 'INVALID_LOGIN_RESPONSE' });
});

test('HTTP status, challenge, malformed body and thrown errors are classified without upstream leaks', async () => {
  for (const [status, body, code, retryable] of [[401, accessToken, 'LOGIN_REQUEST_REJECTED', false], [403, accessToken, 'LOGIN_REQUEST_REJECTED', false], [302, accessToken, 'LOGIN_REQUEST_REJECTED', false], [429, accessToken, 'LOGIN_RATE_LIMITED', true], [503, accessToken, 'UPSTREAM_UNAVAILABLE', true], [200, '<html>Cloudflare challenge-platform</html>', 'CLOUDFLARE_BLOCK', false], [200, '{invalid', 'INVALID_LOGIN_RESPONSE', true], [200, 'x'.repeat(262145), 'INVALID_LOGIN_RESPONSE', true]]) {
    const harness = native({ status, body });
    await assert.rejects(harness.lookup(accessToken, { accountId }), error => error.code === code && error.retryable === retryable && !JSON.stringify(error).includes(accessToken));
    assert.equal(harness.closed, 1);
  }
  await assert.rejects(native({ throwError: new Error(accessToken) }).lookup(accessToken, { accountId }), error => error.code === 'UPSTREAM_UNAVAILABLE' && !error.message.includes(accessToken));
});

test('native cancellation closes once, observes late failures and removes abort listeners', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const pendingGet = deferred();
  const controller = new AbortController();
  const harness = native({ getImpl: () => pendingGet.promise, closeImpl: () => new Promise(() => {}) });
  const outcome = assert.rejects(harness.lookup(accessToken, { accountId, signal: controller.signal }), { code: 'CANCELLED' });
  await flush(); controller.abort(); await flush();
  assert.equal(harness.closed, 1);
  t.mock.timers.tick(BUSINESS_USAGE_CLOSE_TIMEOUT_MS);
  await outcome;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  pendingGet.reject(new Error(accessToken)); await flush();
  assert.equal(harness.closed, 1);
  const untouched = native();
  await assert.rejects(untouched.lookup(accessToken, { accountId, signal: AbortSignal.abort() }), { code: 'CANCELLED' });
  assert.equal(untouched.calls.length, 0);
});

test('independent deadline bounds a hanging native request and native close', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const harness = native({ getImpl: () => new Promise(() => {}), closeImpl: () => new Promise(() => {}) });
  const controller = new AbortController();
  const outcome = assert.rejects(harness.lookup(accessToken, { accountId, signal: controller.signal }), error => error.code === 'LOGIN_TIMEOUT' && error.retryable);
  await flush(); t.mock.timers.tick(BUSINESS_USAGE_TIMEOUT_MS); await flush();
  assert.equal(harness.closed, 1);
  t.mock.timers.tick(BUSINESS_USAGE_CLOSE_TIMEOUT_MS); await outcome;
  assert.equal(controller.signal.aborted, false);
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
});

test('cancellation races a hanging response body without leaking its late failure', async () => {
  const pending = deferred();
  const controller = new AbortController();
  const harness = native({ textImpl: () => pending.promise });
  const outcome = assert.rejects(harness.lookup(accessToken, { accountId, signal: controller.signal }), { code: 'CANCELLED' });
  await flush(); controller.abort(); await outcome;
  assert.equal(harness.closed, 1);
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  pending.reject(new Error(accessToken)); await flush();
});

test('concurrent native lookups keep separate account headers and cancel only their own request', async () => {
  const waiting = new Map();
  const harness = native({ getImpl: (_url, options) => {
    const pending = deferred(); waiting.set(options.headers['ChatGPT-Account-ID'], pending);
    return pending.promise;
  } });
  const secondId = 'synthetic-workspace-B';
  const controller = new AbortController();
  const first = assert.rejects(harness.lookup(accessToken, { accountId, signal: controller.signal }), { code: 'CANCELLED' });
  const second = harness.lookup(token(secondId), { accountId: secondId });
  await flush();
  const calls = harness.calls.filter(call => call.url);
  assert.equal(calls.length, 2);
  for (const call of calls) assert.equal(call.options.headers.authorization, `Bearer ${token(call.options.headers['ChatGPT-Account-ID'])}`);
  assert.notEqual(calls[0].options.headers['oai-device-id'], calls[1].options.headers['oai-device-id']);
  controller.abort();
  waiting.get(secondId).resolve({ status: 200, text: async () => JSON.stringify(raw({ account_id: secondId })) });
  assert.deepEqual(await second, proof(secondId));
  await first;
  waiting.get(accountId).reject(new Error(accessToken)); await flush();
  assert.equal(harness.closed, 2);
});

test('authenticated Business RPC returns only the account-bound DTO and uses the native factory property', async t => {
  let called = 0;
  const factory = () => ({});
  factory.businessUsageLookup = async (value, { accountId: id }) => { called++; assert.equal(value, accessToken); return { ...proof(id), access_token: value }; };
  const harness = await rpc(t, { nativeFactory: factory, lookupBusinessUsage: undefined });
  assert.deepEqual(await harness.lookup(accessToken, { accountId }), proof());
  assert.equal(called, 1);
});

test('RPC rejects missing auth, browser origin, extra body properties and mismatched token account', async t => {
  let called = 0;
  const harness = await rpc(t, { lookupBusinessUsage: async () => { called++; return proof(); } });
  const request = (headers, body) => fetch(`${harness.baseUrl}/workspaces/verify-business`, { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers }, body: JSON.stringify(body) });
  assert.equal((await request({}, { accessToken, accountId })).status, 401);
  assert.equal((await request({ Authorization: `Bearer ${harness.key}`, Origin: 'https://example.test' }, { accessToken, accountId })).status, 403);
  for (const body of [{ accessToken }, { accessToken, accountId, url: 'https://example.test' }, { accessToken, accountId: 'different' }, { accessToken: 'a'.repeat(24577), accountId }]) {
    const response = await request({ Authorization: `Bearer ${harness.key}` }, body);
    assert.equal(response.status, 502);
    assert.equal((await response.text()).includes(accessToken), false);
  }
  assert.equal(called, 0);
});

test('RPC capacity is shared with normal workspace inspections', async t => {
  const started = deferred(), pending = deferred();
  const harness = await rpc(t, { workspaceLookup: async () => { started.resolve(); await pending.promise; return workspace(); } });
  const first = harness.workspaceLookup('synthetic-workspace-access');
  await started.promise;
  await assert.rejects(harness.lookup(accessToken, { accountId }), error => error.code === 'UPSTREAM_UNAVAILABLE' && error.retryable);
  pending.resolve(); await first;
  assert.deepEqual(await harness.lookup(accessToken, { accountId }), proof());
});

test('concurrent Business RPC calls preserve account/token isolation while settling in reverse order', async t => {
  const started = deferred();
  const pending = new Map();
  const harness = await rpc(t, { maxSessions: 2, lookupBusinessUsage: (value, { accountId: id }) => {
    assert.equal(value, token(id));
    const item = deferred(); pending.set(id, item);
    if (pending.size === 2) started.resolve();
    return item.promise;
  } });
  const first = harness.lookup(accessToken, { accountId });
  const secondId = 'synthetic-workspace-B';
  const second = harness.lookup(token(secondId), { accountId: secondId });
  await started.promise;
  pending.get(secondId).resolve(proof(secondId, { allowed: false, limitReached: true }));
  pending.get(accountId).resolve(proof());
  assert.deepEqual(await first, proof());
  assert.deepEqual(await second, proof(secondId, { allowed: false, limitReached: true }));
});

test('cancelled RPC aborts worker inspection and frees its capacity', async t => {
  const started = deferred(), aborted = deferred();
  let calls = 0;
  const harness = await rpc(t, { lookupBusinessUsage: async (_value, { accountId: id, signal }) => {
    if (++calls > 1) return proof(id);
    started.resolve();
    await new Promise((resolve, reject) => signal.addEventListener('abort', () => { aborted.resolve(); reject(Object.assign(new Error(), { code: 'CANCELLED' })); }, { once: true }));
  } });
  const controller = new AbortController();
  const outcome = assert.rejects(harness.lookup(accessToken, { accountId, signal: controller.signal }), { code: 'CANCELLED' });
  await started.promise; controller.abort(); await outcome; await aborted.promise;
  assert.deepEqual(await harness.lookup(accessToken, { accountId }), proof());
});

test('worker shutdown cancels the in-flight native usage session and closes it once', async t => {
  const started = deferred(), pending = deferred();
  const provider = native({ getImpl: () => { started.resolve(); return pending.promise; } });
  const harness = await rpc(t, { lookupBusinessUsage: provider.lookup });
  const result = harness.lookup(accessToken, { accountId }).catch(error => error);
  await started.promise;
  await harness.worker.close();
  assert.equal(harness.worker.health().ready, false);
  const outcome = await result;
  assert.ok(['CANCELLED', 'UPSTREAM_UNAVAILABLE'].includes(outcome.code));
  assert.equal(provider.closed, 1);
  pending.reject(new Error(accessToken)); await flush();
  assert.equal(provider.closed, 1);
});

test('untrusted RPC proof sources, Free results and token-reflecting errors never confirm Business', async t => {
  let value;
  const harness = await rpc(t, { lookupBusinessUsage: async () => value });
  for (value of [{ ...proof(), source: 'jwt' }, { ...proof(), plan: 'free' }, { ...proof(), accountId: 'different' }, { ...proof(), allowed: undefined }]) {
    await assert.rejects(harness.lookup(accessToken, { accountId }), error => !JSON.stringify(error).includes(accessToken) && !JSON.stringify(error).includes(harness.key));
  }
  const throwing = await rpc(t, { lookupBusinessUsage: async () => { throw Object.assign(new Error(accessToken), { code: accessToken, causeCode: accessToken }); } });
  await assert.rejects(throwing.lookup(accessToken, { accountId }), error => error.code === 'UPSTREAM_UNAVAILABLE' && !JSON.stringify(error).includes(accessToken));
});

test('disabled or non-loopback Business clients never perform provider calls', async t => {
  let called = false;
  const harness = await rpc(t, { enabled: false, lookupBusinessUsage: async () => { called = true; return proof(); } });
  await assert.rejects(harness.lookup(accessToken, { accountId }), { code: 'LOGIN_TRANSPORT_REQUIRED' });
  assert.equal(called, false);
  assert.throws(() => createBusinessUsageLookup({ baseUrl: 'https://example.test', keyFile: harness.keyFile }), { code: 'LOGIN_TRANSPORT_REQUIRED' });
});
