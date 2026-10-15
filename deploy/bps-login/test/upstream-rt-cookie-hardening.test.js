import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire, Module } from 'node:module';
import { fileURLToPath } from 'node:url';
import { dirname } from 'node:path';

const runtimeFile = fileURLToPath(new URL('../runtime/src/rtProtocolClient.js', import.meta.url));
const requireRuntime = createRequire(runtimeFile);
const { hardenCookieJar } = requireRuntime('./tlsClientBootstrap');

// Reproduce node-tls-client 2.1.0: tough-cookie rejects attribute-less
// prefixed cookies with undefined, then syncCookies/mergeCookies read .key.
class PrefixJar {
  async setCookie(text, url) {
    const [pair, ...attributes] = text.split(';').map(value => value.trim());
    const equal = pair.indexOf('=');
    const key = pair.slice(0, equal), value = pair.slice(equal + 1);
    if (key === 'rejected') throw new Error('cookie rejected');
    if (key === 'ignored') return undefined;
    if (/^__Secure-|^__Host-/.test(key) && (!attributes.includes('Secure') || !url.startsWith('https:'))) return undefined;
    if (key.startsWith('__Host-') && !attributes.includes('Path=/')) return undefined;
    return { key, value };
  }
  async getCookieString() { return ''; }
  async syncCookies(cookies, url) {
    const result = {};
    for (const [key, value] of Object.entries(cookies)) {
      const cookie = await this.setCookie(`${key}=${value}`, url);
      result[cookie.key] = cookie.value;
    }
    return result;
  }
  async mergeCookies(cookies, url) {
    return Promise.all(Object.entries(cookies).map(async ([key, value]) => {
      const cookie = await this.setCookie(`${key}=${value}`, url);
      return { name: cookie.key, value: cookie.value };
    }));
  }
}

function fixture({ status = 200, transportError } = {}) {
  const sessions = [];
  class Session {
    constructor(options) { this.options = options; this.jar = new PrefixJar(); sessions.push(this); }
    async post(url) {
      if (transportError) throw transportError;
      const cookies = await this.jar.syncCookies({ '__Secure-auth-session': 'fixture-secure', '__Host-auth-context': 'fixture-host' }, url);
      return { status, cookies, headers: {}, text: async () => JSON.stringify({ page: { type: 'external_url', payload: { url: 'https://example.test/callback' } } }) };
    }
    async close() { this.closed = true; }
  }
  const module = new Module(runtimeFile);
  module.filename = runtimeFile;
  module.paths = Module._nodeModulePaths(dirname(runtimeFile));
  module.require = name => {
    if (name === 'tough-cookie') return { CookieJar: PrefixJar };
    if (name === './tlsClientBootstrap') return { requireTlsClient: () => ({ Session, initTLS: async () => {} }), hardenCookieJar };
    if (name === './tlsProfile') return { rtClientIdentifier: () => 'fixture-profile' };
    return requireRuntime(name);
  };
  module._compile(readFileSync(runtimeFile, 'utf8'), runtimeFile);
  return { client: new module.exports.RtProtocolClient(), sessions };
}

test('fixture reproduces the dependency crash before hardening', async () => {
  const jar = new PrefixJar();
  await assert.rejects(jar.syncCookies({ '__Secure-auth-session': 'fixture' }, 'https://auth.example.test'), TypeError);
  await assert.rejects(jar.mergeCookies({ '__Host-auth-context': 'fixture' }, 'https://auth.example.test'), TypeError);
});

test('workspace selection preserves successful responses carrying prefixed cookies', async () => {
  const { client } = fixture();
  try {
    const result = await client._post('/api/accounts/workspace/select', { workspace_id: 'fixture-workspace' });
    assert.equal(result.status, 200);
    assert.equal(result.data.page.type, 'external_url');
    assert.equal(result.data.page.payload.url, 'https://example.test/callback');
  } finally { await client.close(); }
});

test('login session supports outgoing prefixed cookies and isolates rejected cookies', async () => {
  const { client } = fixture();
  try {
    const session = await client._ensureSession();
    assert.deepEqual(await session.jar.mergeCookies({ '__Host-auth-context': 'fixture', normal: 'ok', rejected: 'bad', ignored: 'bad' }, 'https://auth.example.test'), [
      { name: '__Host-auth-context', value: 'fixture' }, { name: 'normal', value: 'ok' },
    ]);
    assert.deepEqual(await session.jar.syncCookies({ '__Secure-auth-session': 'fixture', normal: 'ok', rejected: 'bad' }, 'https://auth.example.test'), {
      '__Secure-auth-session': 'fixture', normal: 'ok',
    });
  } finally { await client.close(); }
});

test('session reuse and recreation both retain cookie hardening', async () => {
  const { client, sessions } = fixture();
  const first = await client._ensureSession();
  assert.equal(await client._ensureSession(), first);
  await client.close();
  const second = await client._ensureSession();
  assert.notEqual(first, second);
  assert.equal(sessions.length, 2);
  assert.equal(first.closed, true);
  assert.equal(second.options.followRedirects, false);
  assert.equal(second.options.timeout, 30000);
  assert.deepEqual(await second.jar.syncCookies({ '__Host-auth-context': 'fixture' }, 'https://auth.example.test'), { '__Host-auth-context': 'fixture' });
  await client.close();
});

test('cookie hardening does not turn rejected HTTP responses into success', async () => {
  const { client } = fixture({ status: 403 });
  try { assert.equal((await client._post('/api/accounts/workspace/select', {})).status, 403); }
  finally { await client.close(); }
});

test('cookie hardening preserves transport failures and cookie-store rejections', async () => {
  const error = new Error('fixture connection failure');
  const { client } = fixture({ transportError: error });
  try {
    await assert.rejects(client._post('/api/accounts/workspace/select', {}), actual => actual === error);
    const session = await client._ensureSession();
    assert.deepEqual(await session.jar.syncCookies({ '__Secure-auth-session': 'fixture', normal: 'ok' }, 'http://auth.example.test'), { normal: 'ok' });
  } finally { await client.close(); }
});
