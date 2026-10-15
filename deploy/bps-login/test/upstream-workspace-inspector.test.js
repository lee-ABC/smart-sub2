import test from 'node:test';
import assert from 'node:assert/strict';
import { getEventListeners } from 'node:events';
import { createNativeWorkspaceInspector, WORKSPACE_CHECK_URL, WORKSPACE_LOOKUP_TIMEOUT_MS, WORKSPACE_CLOSE_TIMEOUT_MS, sanitizeWorkspaceInspection } from '../upstream/server/workspace-inspector.js';

const accessToken = 'synthetic-access-token-only-for-unit-tests';
const raw = () => ({ accounts: { default: {
  account: { account_id: 'workspace-synthetic', name: 'Synthetic team', plan_type: 'team', is_deactivated: false, is_view_only: false },
  entitlement: { subscription_plan: 'chatgptteamplan', has_active_subscription: true, expires_at: null },
} }, account_ordering: ['default'] });

function setup({ status = 200, body = raw(), throwError, getImpl, textImpl, closeImpl } = {}) {
  const calls = [];
  let closed = 0;
  class Session {
    constructor(options) { calls.push({ options }); }
    async get(url, options) {
      calls.push({ url, options });
      if (throwError) throw throwError;
      if (getImpl) return getImpl();
      return { status, text: async () => textImpl ? textImpl() : typeof body === 'string' ? body : JSON.stringify(body) };
    }
    async close() { closed += 1; return closeImpl?.(); }
  }
  const lookup = createNativeWorkspaceInspector({ tls: { Session }, rtClientIdentifier: () => 'existing-profile', headerOrderFor: keys => keys });
  return { lookup, calls, get closed() { return closed; } };
}

async function flushMicrotasks() {
  for (let index = 0; index < 20; index += 1) await Promise.resolve();
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test('uses fixed HTTPS account check through unchanged native profile and disables redirects', async () => {
  const harness = setup();
  const result = await harness.lookup(accessToken);
  assert.deepEqual(harness.calls[0].options, { clientIdentifier: 'existing-profile', timeout: 30000, followRedirects: false });
  assert.equal(harness.calls[1].url, WORKSPACE_CHECK_URL);
  assert.equal(harness.calls[1].options.followRedirects, false);
  assert.equal(harness.calls[1].options.headers.authorization, `Bearer ${accessToken}`);
  assert.equal(result.workspaces[0].id, 'workspace-synthetic');
  assert.deepEqual(result.workspaces[0].aliases, ['workspace-synthetic', 'default']);
  assert.equal(result.complete, true);
  assert.equal(JSON.stringify(result).includes(accessToken), false);
  assert.equal(harness.closed, 1);
});

test('classifies auth, redirect, rate-limit, outage, challenge and malformed responses without leaking upstream data', async () => {
  for (const [status, body, code, retryable] of [
    [401, accessToken, 'LOGIN_REQUEST_REJECTED', false], [403, accessToken, 'LOGIN_REQUEST_REJECTED', false],
    [302, accessToken, 'LOGIN_REQUEST_REJECTED', false], [429, accessToken, 'LOGIN_RATE_LIMITED', true],
    [503, accessToken, 'UPSTREAM_UNAVAILABLE', true],
    [200, '<html>Cloudflare challenge-platform</html>', 'CLOUDFLARE_BLOCK', false],
    [200, '{invalid', 'INVALID_LOGIN_RESPONSE', true], [200, {}, 'INVALID_LOGIN_RESPONSE', true],
  ]) {
    const harness = setup({ status, body });
    await assert.rejects(harness.lookup(accessToken), error => error.code === code && error.retryable === retryable && !JSON.stringify(error).includes(accessToken));
    assert.equal(harness.closed, 1);
  }
  const harness = setup({ throwError: new Error(accessToken) });
  await assert.rejects(harness.lookup(accessToken), error => error.code === 'UPSTREAM_UNAVAILABLE' && error.retryable && !error.message.includes(accessToken));
});

test('cancellation and invalid token never make a request', async () => {
  const harness = setup();
  await assert.rejects(harness.lookup(accessToken, { signal: AbortSignal.abort() }), { code: 'CANCELLED' });
  for (const token of ['', 'token\nwith-newline', 'a'.repeat(24577)]) await assert.rejects(harness.lookup(token), { code: 'TOKEN_STRUCTURE_INVALID' });
  assert.equal(harness.calls.length, 0);
});

test('sanitized DTO rejects reflected private values and unknown provider fields do not escape', async () => {
  const result = await setup().lookup(accessToken);
  assert.equal(JSON.stringify(sanitizeWorkspaceInspection({ ...result, access_token: accessToken }, [accessToken])).includes(accessToken), false);
  assert.throws(() => sanitizeWorkspaceInspection({ ...result, workspaces: [{ ...result.workspaces[0], name: accessToken }] }, [accessToken]), { code: 'INVALID_LOGIN_RESPONSE' });
  assert.throws(() => sanitizeWorkspaceInspection({ ...result, workspaces: [{ ...result.workspaces[0], aliases: [accessToken] }] }, [accessToken]), { code: 'INVALID_LOGIN_RESPONSE' });
});

test('sanitizer preserves incomplete snapshots and normalizes old metadata without accepting poisoned aliases', async () => {
  const result = await setup({ body: { ...raw(), has_more: true } }).lookup(accessToken);
  const sanitized = sanitizeWorkspaceInspection({ ...result, next_cursor: accessToken, extra: { accessToken } }, [accessToken]);
  assert.equal(sanitized.complete, false);
  assert.deepEqual(sanitized.workspaces[0].aliases, ['workspace-synthetic', 'default']);
  assert.equal(JSON.stringify(sanitized).includes(accessToken), false);
  const { complete, ...legacy } = result;
  delete legacy.workspaces[0].aliases;
  const normalized = sanitizeWorkspaceInspection(legacy);
  assert.equal(normalized.complete, true);
  assert.deepEqual(normalized.workspaces[0].aliases, ['workspace-synthetic']);
  assert.throws(() => sanitizeWorkspaceInspection({ ...normalized, complete: 'true' }), { code: 'INVALID_LOGIN_RESPONSE' });
  assert.throws(() => sanitizeWorkspaceInspection({ ...normalized, workspaces: [...normalized.workspaces, { ...normalized.workspaces[0], id: 'second', aliases: ['workspace-synthetic'] }] }), { code: 'INVALID_LOGIN_RESPONSE' });
});

test('concurrent lookups keep independent native sessions, bearer headers, aliases and completeness', async () => {
  const calls = [];
  const requests = new Map();
  const sessions = [];
  class Session {
    constructor() { this.closed = 0; sessions.push(this); }
    async get(url, options) {
      calls.push({ session: this, url, options });
      const token = options.headers.authorization.slice(7);
      const request = deferred();
      requests.set(token, request);
      return request.promise;
    }
    async close() { this.closed++; }
  }
  const lookup = createNativeWorkspaceInspector({ tls: { Session }, rtClientIdentifier: () => 'synthetic-profile', headerOrderFor: keys => keys });
  const tokenA = 'synthetic-lookup-A', tokenB = 'synthetic-lookup-B';
  const controller = new AbortController();
  const first = assert.rejects(lookup(tokenA, { signal: controller.signal }), { code: 'CANCELLED' });
  const second = lookup(tokenB);
  await flushMicrotasks();
  assert.equal(sessions.length, 2);
  assert.notEqual(calls[0].session, calls[1].session);
  assert.notEqual(calls[0].options.headers['oai-device-id'], calls[1].options.headers['oai-device-id']);
  assert.ok(calls.every(call => !Object.hasOwn(call.options.headers, 'cookie')));
  controller.abort();
  const body = raw();
  body.accounts.default.account.account_id = 'workspace-B';
  body.has_more = true;
  requests.get(tokenB).resolve({ status: 200, text: async () => JSON.stringify(body) });
  const result = await second;
  await first;
  assert.equal(result.workspaces[0].id, 'workspace-B');
  assert.deepEqual(result.workspaces[0].aliases, ['workspace-B', 'default']);
  assert.equal(result.complete, false);
  assert.ok(sessions.every(session => session.closed === 1));
  requests.get(tokenA).reject(new Error(tokenA));
  await flushMicrotasks();
});

test('successful lookup releases a hanging native close within two seconds and removes cancellation callbacks', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const controller = new AbortController();
  const harness = setup({ closeImpl: () => new Promise(() => {}) });
  let settled = false;
  const pending = harness.lookup(accessToken, { signal: controller.signal }).then(result => { settled = true; return result; });
  await flushMicrotasks();
  assert.equal(harness.closed, 1);
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  t.mock.timers.tick(WORKSPACE_CLOSE_TIMEOUT_MS - 1);
  await flushMicrotasks();
  assert.equal(settled, false);
  t.mock.timers.tick(1);
  assert.equal((await pending).workspaces[0].id, 'workspace-synthetic');
  controller.abort();
  t.mock.timers.tick(WORKSPACE_LOOKUP_TIMEOUT_MS);
  await flushMicrotasks();
  assert.equal(harness.closed, 1);
});

test('independent thirty-second deadline bounds a hanging GET and close', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const controller = new AbortController();
  const harness = setup({ getImpl: () => new Promise(() => {}), closeImpl: () => new Promise(() => {}) });
  let settled = false;
  const outcome = assert.rejects(harness.lookup(accessToken, { signal: controller.signal }), error => error.code === 'LOGIN_TIMEOUT' && error.retryable).then(() => { settled = true; });
  await flushMicrotasks();
  t.mock.timers.tick(WORKSPACE_LOOKUP_TIMEOUT_MS - 1);
  await flushMicrotasks();
  assert.equal(harness.closed, 0);
  assert.equal(settled, false);
  t.mock.timers.tick(1);
  await flushMicrotasks();
  assert.equal(harness.closed, 1);
  assert.equal(settled, false);
  t.mock.timers.tick(WORKSPACE_CLOSE_TIMEOUT_MS);
  await outcome;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  assert.equal(controller.signal.aborted, false);
});

test('cancellation bounds hanging GET and close, observes late rejection, and closes only once', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const controller = new AbortController();
  const request = deferred();
  const closing = deferred();
  const harness = setup({ getImpl: () => request.promise, closeImpl: () => closing.promise });
  const outcome = assert.rejects(harness.lookup(accessToken, { signal: controller.signal }), { code: 'CANCELLED' });
  await flushMicrotasks();
  controller.abort();
  await flushMicrotasks();
  assert.equal(harness.closed, 1);
  t.mock.timers.tick(WORKSPACE_CLOSE_TIMEOUT_MS);
  await outcome;
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
  request.reject(new Error('synthetic late native request failure'));
  closing.reject(new Error('synthetic late native close failure'));
  await flushMicrotasks();
  t.mock.timers.tick(WORKSPACE_LOOKUP_TIMEOUT_MS);
  assert.equal(harness.closed, 1);
});

test('cancellation races a hanging response body and observes its late rejection', async () => {
  const controller = new AbortController();
  const body = deferred();
  const harness = setup({ textImpl: () => body.promise });
  const outcome = assert.rejects(harness.lookup(accessToken, { signal: controller.signal }), { code: 'CANCELLED' });
  await flushMicrotasks();
  controller.abort();
  await outcome;
  body.reject(new Error('synthetic late body failure'));
  await flushMicrotasks();
  assert.equal(harness.closed, 1);
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
});
