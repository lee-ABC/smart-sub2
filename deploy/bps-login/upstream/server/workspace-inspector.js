import { randomUUID } from 'node:crypto';
import policy from '../shared/business-workspace-policy.cjs';
import { LoginFlowError, sanitizeLoginError } from './login-state-machine.js';

export const WORKSPACE_CHECK_URL = 'https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27';
export const MAX_WORKSPACE_TOKEN_BYTES = 24576;
export const WORKSPACE_LOOKUP_TIMEOUT_MS = 30000;
export const WORKSPACE_CLOSE_TIMEOUT_MS = 2000;
const fail = (code, options) => new LoginFlowError(code, undefined, options);

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
    // Observe both outcomes even when cancellation wins and the native call settles later.
    Promise.resolve().then(() => {
      if (signal.aborted) throw abortError();
      return operation();
    }).then(value => signal.aborted ? cancel() : finish(resolve, value), error => finish(reject, error));
  });
}

export function validateWorkspaceAccessToken(value) {
  if (typeof value !== 'string' || value.length < 1 || Buffer.byteLength(value) > MAX_WORKSPACE_TOKEN_BYTES || !/^[\x21-\x7e]+$/.test(value)) throw fail('TOKEN_STRUCTURE_INVALID');
  return value;
}

export function sanitizeWorkspaceInspection(value, privateValues = []) {
  policy.chooseBusinessWorkspace(value);
  if (value.workspaces.length < 1 || value.workspaces.length > 100 || value.order.length > 100) throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
  const result = {
    workspaces: value.workspaces.map(item => {
      if (typeof item.name !== 'string' || item.name.length > 256 || /[\x00-\x1f\x7f]/.test(item.name)) throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
      return { id: item.id, name: item.name, kind: item.kind, plan: item.plan, active: item.active, expiresAt: item.expiresAt, deactivated: item.deactivated, viewOnly: item.viewOnly, aliases: [...new Set([item.id, ...(item.aliases || [])])] };
    }),
    order: [...value.order],
    complete: value.complete === undefined ? true : value.complete,
  };
  const encoded = JSON.stringify(result);
  if (privateValues.some(secret => typeof secret === 'string' && secret.length >= 4 && encoded.includes(secret))) throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
  return result;
}

/** Uses the installed native TLS profile and never invokes Sentinel or the SDK. */
export function createNativeWorkspaceInspector({ tls, rtClientIdentifier, headerOrderFor } = {}) {
  if (typeof tls?.Session !== 'function' || typeof rtClientIdentifier !== 'function' || typeof headerOrderFor !== 'function') throw fail('LOGIN_TRANSPORT_REQUIRED');
  return async (accessToken, { signal, proxyUrl } = {}) => {
    validateWorkspaceAccessToken(accessToken);
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
        const timer = setTimeout(resolve, WORKSPACE_CLOSE_TIMEOUT_MS);
        const finished = () => { clearTimeout(timer); resolve(); };
        Promise.resolve().then(() => session?.close()).then(finished, finished);
      });
      return closePromise;
    };
    const deadline = setTimeout(() => { timedOut = true; controller.abort(); }, WORKSPACE_LOOKUP_TIMEOUT_MS);
    signal?.addEventListener('abort', cancel, { once: true });
    controller.signal.addEventListener('abort', close, { once: true });
    try {
      session = new tls.Session({ clientIdentifier: rtClientIdentifier(), timeout: 30000, followRedirects: false, ...(proxyUrl ? { proxy: proxyUrl } : {}) });
      if (signal?.aborted) cancel();
      if (controller.signal.aborted) throw abortError();
      const headers = {
        authorization: `Bearer ${accessToken}`,
        'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
        accept: '*/*', 'accept-language': 'en-US,en;q=0.9', 'oai-language': 'en-US',
        'oai-device-id': randomUUID(), origin: 'https://chatgpt.com', referer: 'https://chatgpt.com/',
      };
      const response = await abortable(() => session.get(WORKSPACE_CHECK_URL, { headers, headerOrder: headerOrderFor(Object.keys(headers)), followRedirects: false }), controller.signal, abortError);
      if (controller.signal.aborted) throw abortError();
      const status = Number(response?.status);
      if (status === 401 || status === 403) throw fail('LOGIN_REQUEST_REJECTED', { httpStatus: status });
      if (status === 429) throw fail('LOGIN_RATE_LIMITED', { httpStatus: status, retryable: true });
      if (status >= 500 && status <= 599) throw fail('UPSTREAM_UNAVAILABLE', { httpStatus: status, retryable: true });
      if (status !== 200) throw fail('LOGIN_REQUEST_REJECTED', { httpStatus: status });
      const raw = await abortable(() => response.text(), controller.signal, abortError);
      if (controller.signal.aborted) throw abortError();
      if (typeof raw !== 'string' || Buffer.byteLength(raw) > 262144) throw fail('INVALID_LOGIN_RESPONSE', { retryable: true });
      if (/^\s*</.test(raw) && /(?:cloudflare|cf-chl-|challenge-platform|just a moment)/i.test(raw)) throw fail('CLOUDFLARE_BLOCK', { httpStatus: status });
      let value;
      try { value = JSON.parse(raw); } catch { throw fail('INVALID_LOGIN_RESPONSE', { retryable: true }); }
      return policy.parseWorkspaceCheckResponse(value);
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
