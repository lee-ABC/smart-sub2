import { readFileSync, statSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { LoginFlowError, sanitizeLoginError } from './login-state-machine.js';
import { ADDIN_AUTHORIZATION } from './login-engine.js';
import { sanitizeWorkspaceInspection, validateWorkspaceAccessToken } from './workspace-inspector.js';
import { sanitizeBusinessUsageVerification, validateBusinessUsageBinding } from './business-usage-verifier.js';

const METHODS = new Set(['bootstrapAuthorize', 'submitEmail', 'submitPassword', 'issueMfaChallenge', 'verifyMfaTotp', 'submitEmailOtp', 'getAuthorizationCode']);
const fail = (code = 'LOGIN_TRANSPORT_REQUIRED', options = {}) => new LoginFlowError(code, undefined, options);

export function readWorkerKey(keyFile) {
  try {
    const stat = statSync(keyFile);
    if (!stat.isFile() || (process.platform !== 'win32' && (stat.mode & 0o077) !== 0)) throw new Error();
    const key = readFileSync(keyFile, 'utf8').trim();
    if (!/^[A-Za-z0-9_-]{32,256}$/.test(key)) throw new Error();
    return key;
  } catch { throw fail(); }
}

function workerUrl(baseUrl) {
  let url;
  try { url = new URL(baseUrl); } catch { throw fail(); }
  if (url.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(url.hostname) || url.username || url.password || url.search || url.hash || url.pathname !== '/') throw fail();
  return url.origin;
}

export async function workerHealth({ baseUrl = 'http://127.0.0.1:19418', fetchImpl = fetch } = {}) {
  try {
    const response = await fetchImpl(`${workerUrl(baseUrl)}/health`, { signal: AbortSignal.timeout(2000), redirect: 'error' });
    if (!response.ok) return { ready: false };
    const value = await response.json();
    return { ready: value.ready === true, enabled: value.enabled === true, transport: value.transport === 'legacy-stdin-private' ? value.transport : 'blocked', maxSessions: Number.isSafeInteger(value.maxSessions) && value.maxSessions >= 1 ? value.maxSessions : 1 };
  } catch { return { ready: false }; }
}

export async function setWorkerCapacity({ baseUrl = 'http://127.0.0.1:19418', keyFile, maxSessions, fetchImpl = fetch } = {}) {
  if (!Number.isSafeInteger(maxSessions) || maxSessions < 1) throw fail('LOGIN_REQUEST_REJECTED');
  const base = workerUrl(baseUrl);
  const key = readWorkerKey(keyFile);
  try {
    const response = await fetchImpl(`${base}/capacity`, {
      method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ maxSessions }), signal: AbortSignal.timeout(5000), redirect: 'error',
    });
    const text = await response.text();
    if (text.length > 32768) throw fail('INVALID_LOGIN_RESPONSE');
    let value;
    try { value = JSON.parse(text); } catch { throw fail('INVALID_LOGIN_RESPONSE'); }
    if (value?.error) throw sanitizeLoginError(value.error, [key]);
    if (!response.ok || !Number.isSafeInteger(value?.maxSessions) || value.maxSessions !== maxSessions) throw fail('INVALID_LOGIN_RESPONSE');
    return { maxSessions: value.maxSessions };
  } catch (error) { throw sanitizeLoginError(error, [key]); }
}

/** The access token exists only in the authenticated loopback request body. */
export function createWorkspaceLookup({ baseUrl = 'http://127.0.0.1:19418', keyFile, fetchImpl = fetch } = {}) {
  const base = workerUrl(baseUrl);
  return async (accessToken, { signal } = {}) => {
    validateWorkspaceAccessToken(accessToken);
    if (signal?.aborted) throw fail('CANCELLED');
    const key = readWorkerKey(keyFile);
    const requestSignal = signal ? AbortSignal.any([signal, AbortSignal.timeout(35000)]) : AbortSignal.timeout(35000);
    try {
      const response = await fetchImpl(`${base}/workspaces/check`, {
        method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ accessToken }), signal: requestSignal, redirect: 'error',
      });
      const text = await response.text();
      if (Buffer.byteLength(text) > 262144) throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
      let value;
      try { value = JSON.parse(text); } catch { throw fail('INVALID_LOGIN_RESPONSE', { retryable: true }); }
      if (value?.error) throw sanitizeLoginError(value.error, [key, accessToken]);
      if (!response.ok || !value || typeof value !== 'object') throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
      if (signal?.aborted) throw fail('CANCELLED');
      return sanitizeWorkspaceInspection(value.result, [key, accessToken]);
    } catch (error) {
      if (signal?.aborted) throw fail('CANCELLED');
      if (requestSignal.aborted) throw fail('LOGIN_TIMEOUT', { retryable: true });
      const safe = sanitizeLoginError(error, [key, accessToken]);
      if (safe.code === 'LOGIN_STEP_FAILED') throw fail('UPSTREAM_UNAVAILABLE', { retryable: true });
      throw safe;
    }
  };
}

/** The Business proof must come from the authenticated, account-bound usage RPC. */
export function createBusinessUsageLookup({ baseUrl = 'http://127.0.0.1:19418', keyFile, fetchImpl = fetch } = {}) {
  const base = workerUrl(baseUrl);
  return async (accessToken, { accountId, signal } = {}) => {
    validateBusinessUsageBinding(accessToken, accountId);
    if (signal?.aborted) throw fail('CANCELLED');
    const key = readWorkerKey(keyFile);
    const requestSignal = signal ? AbortSignal.any([signal, AbortSignal.timeout(35000)]) : AbortSignal.timeout(35000);
    try {
      const response = await fetchImpl(`${base}/workspaces/verify-business`, {
        method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ accessToken, accountId }), signal: requestSignal, redirect: 'error',
      });
      const text = await response.text();
      if (Buffer.byteLength(text) > 32768) throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
      let value;
      try { value = JSON.parse(text); } catch { throw fail('INVALID_LOGIN_RESPONSE', { retryable: true }); }
      if (value?.error) throw sanitizeLoginError(value.error, [key, accessToken]);
      if (!response.ok || !value || typeof value !== 'object') throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
      if (signal?.aborted) throw fail('CANCELLED');
      return sanitizeBusinessUsageVerification(value.result, { accountId, privateValues: [key, accessToken] });
    } catch (error) {
      if (signal?.aborted) throw fail('CANCELLED');
      if (requestSignal.aborted) throw fail('LOGIN_TIMEOUT', { retryable: true });
      const safe = sanitizeLoginError(error, [key, accessToken]);
      if (safe.code === 'LOGIN_STEP_FAILED') throw fail('UPSTREAM_UNAVAILABLE', { retryable: true });
      throw safe;
    }
  };
}

/** Credentials and RPC results stay in memory; the remote transport owns its session. */
export function createWorkerClient({ baseUrl = 'http://127.0.0.1:19418', keyFile, authorization = ADDIN_AUTHORIZATION, fetchImpl = fetch } = {}) {
  const base = workerUrl(baseUrl);
  return async context => {
    if (context.signal?.aborted) throw fail('CANCELLED');
    const key = readWorkerKey(keyFile);
    const privateValues = [key, context.email, context.password, context.totpSecret, context.expectedState].filter(value => typeof value === 'string');
    let sessionId;
    const creationId = randomBytes(24).toString('base64url');
    let closePromise;
    let closed = false;
    const workspaceWaiters = new Set();
    const request = async (path, { method = 'POST', body, signal } = {}) => {
      if (signal?.aborted) throw fail('CANCELLED');
      try {
        const response = await fetchImpl(`${base}${path}`, {
          method, headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
          ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal, redirect: 'error',
        });
        const text = await response.text();
        if (text.length > 262144) throw fail('INVALID_LOGIN_RESPONSE');
        let decoded;
        try { decoded = JSON.parse(text); } catch { throw fail('INVALID_LOGIN_RESPONSE'); }
        if (decoded?.error) throw sanitizeLoginError(decoded.error, privateValues);
        if (!response.ok || !decoded || typeof decoded !== 'object') throw fail('LOGIN_ENGINE_FAILED');
        if (signal?.aborted) throw fail('CANCELLED');
        return decoded;
      } catch (error) {
        if (signal?.aborted) throw fail('CANCELLED');
        throw sanitizeLoginError(error, privateValues);
      }
    };
    const remoteClose = () => {
      if (closePromise) return closePromise;
      closed = true;
      for (const cancel of [...workspaceWaiters]) cancel();
      context.signal?.removeEventListener('abort', onAbort);
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 2000);
      closePromise = request(`/sessions/${sessionId || creationId}`, { method: 'DELETE', signal: controller.signal })
        .catch(() => {}).finally(() => { clearTimeout(timeout); if (!sessionId) closePromise = undefined; });
      return closePromise;
    };
    const onAbort = () => { void remoteClose(); };
    context.signal?.addEventListener('abort', onAbort, { once: true });
    let created;
    try { created = await request('/sessions', {
      body: { provider: authorization.provider, requestId: creationId, context: {
        email: context.email, password: context.password, totpSecret: context.totpSecret,
        targetWorkspaceId: context.targetWorkspaceId, expectedState: context.expectedState, workspacePreference: context.workspacePreference,
      } }, signal: context.signal,
    }); } catch (error) { await remoteClose(); throw error; }
    if (typeof created.sessionId !== 'string' || !/^[A-Za-z0-9_-]{32,80}$/.test(created.sessionId)) { await remoteClose(); throw fail('INVALID_LOGIN_RESPONSE'); }
    sessionId = created.sessionId;
    if (context.signal?.aborted) { await remoteClose(); throw fail('CANCELLED'); }
    const rpc = async (method, args, signal = context.signal) => {
      if (closed || signal?.aborted) { await remoteClose(); throw fail('CANCELLED'); }
      if (!METHODS.has(method)) throw fail('LOGIN_REQUEST_REJECTED');
      const aborted = () => { void remoteClose(); };
      signal?.addEventListener('abort', aborted, { once: true });
      try {
        const response = await request(`/sessions/${sessionId}/call`, { body: { method, args }, signal });
        return response.result;
      } finally { signal?.removeEventListener('abort', aborted); }
    };
    const getAuthorizationCode = async (_data, options = {}) => {
      const signal = options.signal || context.signal;
      const fixed = { targetWorkspaceId: options.targetWorkspaceId, expectedState: options.expectedState };
      const first = await rpc('getAuthorizationCode', [null, fixed], signal);
      if (first?.workspaceSelectionRequired !== true) return first;
      if (context.targetWorkspaceId || !['team', 'personal'].includes(context.workspacePreference) || typeof context.requestWorkspaceSelection !== 'function') throw fail('WORKSPACE_SELECTION_REQUIRED');
      if (!Array.isArray(first.choices) || first.choices.length < 1 || first.choices.length > 100) throw fail('INVALID_LOGIN_RESPONSE');
      const choices = first.choices.map(choice => {
        if (!choice || typeof choice.id !== 'string' || !choice.id.trim() || choice.id.length > 256 || /[\x00-\x1f\x7f]/.test(choice.id) || typeof choice.name !== 'string' || Array.from(choice.name).length > 80 || /[\x00-\x1f\x7f]/.test(choice.name) || !['team', 'personal', 'unknown'].includes(choice.kind)) throw fail('INVALID_LOGIN_RESPONSE');
        return Object.freeze({ id: choice.id, name: choice.name, kind: choice.kind });
      });
      const ids = new Set(choices.map(choice => choice.id));
      if (ids.size !== choices.length) throw fail('INVALID_LOGIN_RESPONSE');
      if (signal?.aborted || closed) throw fail('CANCELLED');
      let selectedWorkspaceId;
      try {
        selectedWorkspaceId = await new Promise((resolve, reject) => {
          let settled = false;
          const finish = (callback, value) => { if (settled) return; settled = true; signal?.removeEventListener('abort', cancel); workspaceWaiters.delete(closing); callback(value); };
          const cancel = () => { void remoteClose(); finish(reject, fail('CANCELLED')); };
          const closing = () => finish(reject, fail('CANCELLED'));
          workspaceWaiters.add(closing);
          signal?.addEventListener('abort', cancel, { once: true });
          Promise.resolve().then(() => {
            if (signal?.aborted || closed) throw fail('CANCELLED');
            return context.requestWorkspaceSelection({ choices: Object.freeze(choices), signal });
          }).then(value => finish(resolve, value), error => finish(reject, error));
        });
      } catch (error) { throw signal?.aborted || closed ? fail('CANCELLED') : sanitizeLoginError(error, privateValues); }
      if (signal?.aborted || closed) throw fail('CANCELLED');
      if (typeof selectedWorkspaceId !== 'string' || !ids.has(selectedWorkspaceId)) throw fail('WORKSPACE_NOT_FOUND');
      const selected = await rpc('getAuthorizationCode', [null, { ...fixed, selectedWorkspaceId }], signal);
      if (selected?.workspaceSelectionRequired === true) throw fail('WORKSPACE_SELECTION_REQUIRED');
      return selected;
    };
    return {
      bootstrapAuthorize: (url, options = {}) => rpc('bootstrapAuthorize', [url], options.signal),
      submitEmail: (email, options = {}) => rpc('submitEmail', [email], options.signal),
      submitPassword: (password, options = {}) => rpc('submitPassword', [password], options.signal),
      issueMfaChallenge: (factorId, options = {}) => rpc('issueMfaChallenge', [factorId], options.signal),
      verifyMfaTotp: (factorId, code, options = {}) => rpc('verifyMfaTotp', [factorId, code], options.signal),
      submitEmailOtp: (code, options = {}) => rpc('submitEmailOtp', [code], options.signal),
      getAuthorizationCode,
      close: remoteClose,
    };
  };
}
