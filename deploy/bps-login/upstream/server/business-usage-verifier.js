import { randomUUID } from 'node:crypto';
import policy from '../shared/business-workspace-policy.cjs';
import { LoginFlowError, sanitizeLoginError } from './login-state-machine.js';
import { validateWorkspaceAccessToken } from './workspace-inspector.js';

export const BUSINESS_USAGE_URL = 'https://chatgpt.com/backend-api/wham/usage';
export const BUSINESS_USAGE_TIMEOUT_MS = 30000;
export const BUSINESS_USAGE_CLOSE_TIMEOUT_MS = 2000;
const CLIENT_ID = 'app_fnr0pYvVwwFDocDumLG3H2Bp';
const fail = (code, options) => new LoginFlowError(code, undefined, options);
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const identifier = value => typeof value === 'string' && /^[A-Za-z0-9_-]{1,256}$/.test(value)
  && !['__proto__', 'prototype', 'constructor'].includes(value);
const invalid = () => fail('INVALID_LOGIN_RESPONSE', { retryable: true });

function field(value, key, required = false) {
  if (!object(value)) throw invalid();
  let descriptor;
  try { descriptor = Object.getOwnPropertyDescriptor(value, key); } catch { throw invalid(); }
  if (!descriptor) {
    if (required) throw invalid();
    return undefined;
  }
  if (!Object.hasOwn(descriptor, 'value')) throw invalid();
  return descriptor.value;
}

/** The provider authenticates the JWT; these checks bind this request to its acquired account/client. */
export function validateBusinessUsageBinding(accessToken, accountId) {
  validateWorkspaceAccessToken(accessToken);
  if (!identifier(accountId)) throw fail('TOKEN_WORKSPACE_MISSING');
  const parts = accessToken.split('.');
  if (parts.length !== 3 || !parts.every(part => /^[A-Za-z0-9_-]+$/.test(part))) throw fail('TOKEN_STRUCTURE_INVALID');
  let payload;
  try { payload = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8')); }
  catch { throw fail('TOKEN_STRUCTURE_INVALID'); }
  if (!object(payload)) throw fail('TOKEN_STRUCTURE_INVALID');
  const clientId = field(payload, 'client_id');
  if (clientId !== CLIENT_ID) throw fail('TOKEN_CLIENT_MISMATCH');
  const auth = field(payload, 'https://api.openai.com/auth');
  if (!object(auth) || field(auth, 'chatgpt_account_id') !== accountId) throw fail('TOKEN_WORKSPACE_MISMATCH');
  return accountId;
}

/** Required own data fields prevent getters or a token claim from manufacturing proof. */
export function sanitizeBusinessUsageVerification(value, { accountId, privateValues = [] } = {}) {
  if (!identifier(accountId)) throw fail('TOKEN_WORKSPACE_MISSING');
  const observedAccountId = field(value, 'accountId', true);
  const plan = field(value, 'plan', true);
  const checkedAt = field(value, 'checkedAt', true);
  const allowed = field(value, 'allowed', true);
  const limitReached = field(value, 'limitReached', true);
  if (field(value, 'source', true) !== 'wham_usage') throw invalid();
  if (!identifier(observedAccountId) || observedAccountId !== accountId) throw fail('TOKEN_WORKSPACE_MISMATCH');
  if (typeof plan !== 'string' || plan.trim() !== plan || plan.length > 100 || !policy.isBusinessPlan(plan)) throw fail('BUSINESS_WORKSPACE_UNVERIFIED');
  if (typeof checkedAt !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(checkedAt) || !Number.isFinite(Date.parse(checkedAt))) throw invalid();
  const normalizedCheckedAt = new Date(checkedAt).toISOString();
  if (normalizedCheckedAt.slice(0, 19) !== checkedAt.slice(0, 19)) throw invalid();
  if (allowed !== null && typeof allowed !== 'boolean' || limitReached !== null && typeof limitReached !== 'boolean') throw invalid();
  const result = { accountId, plan, checkedAt: normalizedCheckedAt, allowed, limitReached, source: 'wham_usage' };
  const encoded = JSON.stringify(result);
  if (!Array.isArray(privateValues) || privateValues.some(secret => typeof secret === 'string' && secret.length >= 4 && encoded.includes(secret))) throw invalid();
  return result;
}

function responseProof(raw, accountId, checkedAt) {
  if (!object(raw)) throw invalid();
  for (const name of ['account_id', 'accountId']) {
    const value = field(raw, name);
    if (value !== undefined && value !== accountId) throw fail('TOKEN_WORKSPACE_MISMATCH');
  }
  const snake = field(raw, 'plan_type');
  const camel = field(raw, 'planType');
  if (snake !== undefined && camel !== undefined && (typeof snake !== 'string' || typeof camel !== 'string' || snake.toLowerCase() !== camel.toLowerCase())) throw invalid();
  const plan = snake ?? camel;
  if (typeof plan !== 'string' || plan.trim() !== plan || !policy.isBusinessPlan(plan)) throw fail('BUSINESS_WORKSPACE_UNVERIFIED');
  const rateSnake = field(raw, 'rate_limit');
  const rateCamel = field(raw, 'rateLimit');
  if (rateSnake !== undefined && rateCamel !== undefined) throw invalid();
  const rate = rateSnake !== undefined ? rateSnake : rateCamel;
  if (rate !== undefined && !object(rate)) throw invalid();
  const flag = names => {
    if (rate === undefined) return null;
    let result = null;
    for (const name of names) {
      const value = field(rate, name);
      if (value === undefined) continue;
      if (typeof value !== 'boolean' || result !== null && result !== value) throw invalid();
      result = value;
    }
    return result;
  };
  return sanitizeBusinessUsageVerification({ accountId, plan, checkedAt, allowed: flag(['allowed']),
    limitReached: flag(['limit_reached', 'limitReached']), source: 'wham_usage' }, { accountId });
}

function abortable(operation, signal, abortError) {
  if (signal.aborted) return Promise.reject(abortError());
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (handler, value) => {
      if (settled) return;
      settled = true;
      signal.removeEventListener('abort', cancel);
      handler(value);
    };
    const cancel = () => finish(reject, abortError());
    signal.addEventListener('abort', cancel, { once: true });
    Promise.resolve().then(() => {
      if (signal.aborted) throw abortError();
      return operation();
    }).then(value => signal.aborted ? cancel() : finish(resolve, value), error => finish(reject, error));
  });
}

/** Fresh, account-bound usage lookup through the existing native TLS profile. */
export function createNativeBusinessUsageVerifier({ tls, rtClientIdentifier, headerOrderFor, clock = Date.now } = {}) {
  if (typeof tls?.Session !== 'function' || typeof rtClientIdentifier !== 'function' || typeof headerOrderFor !== 'function' || typeof clock !== 'function') throw fail('LOGIN_TRANSPORT_REQUIRED');
  return async (accessToken, { accountId, signal, proxyUrl } = {}) => {
    validateBusinessUsageBinding(accessToken, accountId);
    if (signal?.aborted) throw fail('CANCELLED');
    let session;
    let closePromise;
    let timedOut = false;
    const controller = new AbortController();
    const abortError = () => timedOut ? fail('LOGIN_TIMEOUT', { retryable: true }) : fail('CANCELLED');
    const cancel = () => controller.abort();
    const close = () => {
      if (closePromise) return closePromise;
      closePromise = new Promise(resolve => {
        const timer = setTimeout(resolve, BUSINESS_USAGE_CLOSE_TIMEOUT_MS);
        const finished = () => { clearTimeout(timer); resolve(); };
        Promise.resolve().then(() => session?.close()).then(finished, finished);
      });
      return closePromise;
    };
    const deadline = setTimeout(() => { timedOut = true; controller.abort(); }, BUSINESS_USAGE_TIMEOUT_MS);
    signal?.addEventListener('abort', cancel, { once: true });
    controller.signal.addEventListener('abort', close, { once: true });
    try {
      session = new tls.Session({ clientIdentifier: rtClientIdentifier(), timeout: BUSINESS_USAGE_TIMEOUT_MS, followRedirects: false, ...(proxyUrl ? { proxy: proxyUrl } : {}) });
      if (signal?.aborted) cancel();
      if (controller.signal.aborted) throw abortError();
      const headers = {
        authorization: `Bearer ${accessToken}`, 'ChatGPT-Account-ID': accountId,
        'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
        accept: '*/*', 'accept-language': 'en-US,en;q=0.9', 'oai-language': 'en-US',
        'oai-device-id': randomUUID(), origin: 'https://chatgpt.com', referer: 'https://chatgpt.com/',
      };
      const response = await abortable(() => session.get(BUSINESS_USAGE_URL, { headers, headerOrder: headerOrderFor(Object.keys(headers)), followRedirects: false }), controller.signal, abortError);
      if (controller.signal.aborted) throw abortError();
      const status = Number(response?.status);
      if (status === 401 || status === 403) throw fail('LOGIN_REQUEST_REJECTED', { httpStatus: status });
      if (status === 429) throw fail('LOGIN_RATE_LIMITED', { httpStatus: status, retryable: true });
      if (status >= 500 && status <= 599) throw fail('UPSTREAM_UNAVAILABLE', { httpStatus: status, retryable: true });
      if (status !== 200) throw fail('LOGIN_REQUEST_REJECTED', { httpStatus: status });
      const raw = await abortable(() => response.text(), controller.signal, abortError);
      if (controller.signal.aborted) throw abortError();
      if (typeof raw !== 'string' || Buffer.byteLength(raw) > 262144) throw invalid();
      if (/^\s*</.test(raw) && /(?:cloudflare|cf-chl-|challenge-platform|just a moment)/i.test(raw)) throw fail('CLOUDFLARE_BLOCK', { httpStatus: status });
      let value;
      try { value = JSON.parse(raw); } catch { throw invalid(); }
      const now = clock();
      if (!Number.isFinite(now)) throw invalid();
      return sanitizeBusinessUsageVerification(responseProof(value, accountId, new Date(now).toISOString()), { accountId, privateValues: [accessToken] });
    } catch (error) {
      if (signal?.aborted) throw fail('CANCELLED');
      if (timedOut) throw fail('LOGIN_TIMEOUT', { retryable: true });
      const safe = sanitizeLoginError(error, [accessToken]);
      if (safe.code === 'LOGIN_STEP_FAILED') throw fail('UPSTREAM_UNAVAILABLE', { retryable: true });
      throw safe;
    } finally {
      clearTimeout(deadline);
      signal?.removeEventListener('abort', cancel);
      controller.signal.removeEventListener('abort', close);
      await close();
    }
  };
}
