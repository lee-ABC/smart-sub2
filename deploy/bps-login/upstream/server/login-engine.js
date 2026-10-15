import { createHash, randomBytes } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import { runLoginStateMachine, LoginFlowError, sanitizeLoginError } from './login-state-machine.js';
import { generateTotp } from '../shared/totp.js';
import workspacePolicy from '../shared/business-workspace-policy.cjs';

export const ADDIN_AUTHORIZATION = Object.freeze({
  provider: 'oai-basispoints', clientId: 'app_fnr0pYvVwwFDocDumLG3H2Bp',
  authorizationUrl: 'https://auth.openai.com/oauth/authorize',
  redirectUri: 'https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback',
  tokenUrl: 'https://auth.openai.com/oauth/token?unified=true',
  scope: 'openid offline_access email profile organization.read',
  audience: 'https://api.openai.com/v1',
});

const fail = (code, message, options) => new LoginFlowError(code, message, options);
const codePattern = /^[A-Za-z][A-Za-z0-9_-]{0,79}$/;
const PHASES = new Set(['session', 'email', 'password', 'mfa', 'email_otp', 'authorization', 'exchange', 'validate']);
// A generic LOGIN_STEP_FAILED after the credentials were accepted is the upstream swallowing a
// transient authorization or token-exchange hop; permanent outcomes carry their own codes, so a
// residual generic failure at these phases is safe to re-attempt with a fresh login.
const POST_CREDENTIAL_PHASES = new Set(['session', 'email', 'mfa', 'authorization', 'exchange']);

function abortable(operation, signal) {
  if (signal.aborted) return Promise.reject(fail('CANCELLED', '登录已取消'));
  return new Promise((resolve, reject) => {
    let finished = false;
    const finish = (callback, value) => { if (finished) return; finished = true; signal.removeEventListener('abort', aborted); callback(value); };
    const aborted = () => finish(reject, fail('CANCELLED', '登录已取消'));
    signal.addEventListener('abort', aborted, { once: true });
    Promise.resolve().then(() => { if (signal.aborted) throw fail('CANCELLED', '登录已取消'); return operation(); }).then(value => finish(resolve, value), error => finish(reject, error));
  });
}

export async function getLoginTotp({ secret, signal, clock = Date.now } = {}) {
  const activeSignal = signal || new AbortController().signal;
  let code;
  try { code = await generateTotp({ secret, algorithm: 'SHA1', digits: 6, period: 30 }, clock()); }
  catch { throw fail('TOTP_SECRET_INVALID', '2FA 密钥格式不正确'); }
  if (code.remaining < 3) {
    await new Promise((resolve, reject) => {
      if (activeSignal.aborted) return reject(fail('CANCELLED', '登录已取消'));
      const timer = setTimeout(() => { activeSignal.removeEventListener('abort', cancel); resolve(); }, code.remaining * 1000 + 50);
      const cancel = () => { clearTimeout(timer); activeSignal.removeEventListener('abort', cancel); reject(fail('CANCELLED', '登录已取消')); };
      activeSignal.addEventListener('abort', cancel, { once: true });
    });
    code = await generateTotp({ secret, algorithm: 'SHA1', digits: 6, period: 30 }, clock());
  }
  return code.code;
}

function jwtPayload(token) {
  if (typeof token !== 'string' || token.length > 131072 || token.split('.').length !== 3) throw fail('TOKEN_STRUCTURE_INVALID', '访问令牌结构不正确');
  try {
    const payload = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString('utf8'));
    if (!payload || typeof payload !== 'object' || Array.isArray(payload)) throw new Error();
    return payload;
  } catch { throw fail('TOKEN_STRUCTURE_INVALID', '访问令牌结构不正确'); }
}

function validateBusinessUsage(value, accountId, now, privateValues) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw fail('BUSINESS_USAGE_UNVERIFIED');
  const keys = ['accountId', 'plan', 'checkedAt', 'allowed', 'limitReached', 'source'];
  const descriptors = Object.getOwnPropertyDescriptors(value);
  const clean = {};
  for (const key of keys) {
    const descriptor = descriptors[key];
    if (!descriptor || !Object.hasOwn(descriptor, 'value')) throw fail('BUSINESS_USAGE_UNVERIFIED');
    clean[key] = descriptor.value;
  }
  if (clean.accountId !== accountId || clean.source !== 'wham_usage' || !workspacePolicy.isBusinessPlan(clean.plan)) throw fail('BUSINESS_USAGE_UNVERIFIED');
  if (![clean.allowed, clean.limitReached].every(flag => flag === null || typeof flag === 'boolean')) throw fail('BUSINESS_USAGE_UNVERIFIED');
  if (typeof clean.checkedAt !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(clean.checkedAt)) throw fail('BUSINESS_USAGE_UNVERIFIED');
  const stamp = Date.parse(clean.checkedAt);
  if (!Number.isFinite(stamp) || !Number.isFinite(now) || Math.abs(stamp - now) > 60000) throw fail('BUSINESS_USAGE_UNVERIFIED');
  if (new Date(stamp).toISOString().slice(0, 19) !== clean.checkedAt.slice(0, 19)) throw fail('BUSINESS_USAGE_UNVERIFIED');
  const encoded = JSON.stringify(clean);
  if (privateValues.some(secret => typeof secret === 'string' && secret.length >= 4 && encoded.includes(secret))) throw fail('BUSINESS_USAGE_UNVERIFIED');
  return clean;
}

export function buildLoginCredential(tokens, { authorization, email, targetWorkspaceId, clock = Date.now } = {}) {
  if (!tokens || typeof tokens.access_token !== 'string' || !tokens.access_token || typeof tokens.refresh_token !== 'string' || !tokens.refresh_token) throw fail('TOKEN_INCOMPLETE', '授权结果缺少访问令牌或刷新令牌');
  const payload = jwtPayload(tokens.access_token);
  const clientId = payload.client_id || payload.azp;
  if (!clientId || clientId !== authorization.clientId) throw fail('TOKEN_CLIENT_MISMATCH', '返回的令牌不属于选择的账号渠道');
  const auth = payload['https://api.openai.com/auth'] || {};
  const accountId = auth.chatgpt_account_id;
  if (typeof accountId !== 'string' || !accountId) throw fail('TOKEN_WORKSPACE_MISSING', '令牌缺少有效的账号或工作空间标识');
  if (targetWorkspaceId && accountId !== targetWorkspaceId) throw fail('TOKEN_WORKSPACE_MISMATCH', '令牌属于其他工作空间，已停止导入');
  const profileEmail = payload.email || payload['https://api.openai.com/profile']?.email;
  if (profileEmail && String(profileEmail).toLowerCase() !== email.toLowerCase()) throw fail('TOKEN_ACCOUNT_MISMATCH', '返回令牌的账号与输入邮箱不一致');
  const expiresIn = Number(tokens.expires_in);
  if (!Number.isFinite(expiresIn) || expiresIn <= 0) throw fail('TOKEN_EXPIRY_INVALID', '令牌有效期缺失或不正确');
  const started = clock();
  let expiresAt = started + expiresIn * 1000;
  if (Number.isFinite(payload.exp)) expiresAt = Math.min(expiresAt, payload.exp * 1000);
  if (!Number.isFinite(expiresAt) || expiresAt <= started || !Number.isFinite(new Date(expiresAt).getTime())) throw fail('TOKEN_EXPIRED', '返回的令牌已经过期');
  const userId = typeof auth.chatgpt_user_id === 'string' ? auth.chatgpt_user_id : typeof payload.sub === 'string' ? payload.sub : '';
  const identity = JSON.stringify([authorization.provider, clientId, userId || email.toLowerCase(), accountId]);
  const filename = `${authorization.provider}-${createHash('sha256').update(identity).digest('hex').slice(0, 24)}.json`;
  return {
    type: authorization.provider, email, account_id: accountId, workspace_id: accountId,
    ...(userId ? { user_id: userId } : {}),
    access_token: tokens.access_token, refresh_token: tokens.refresh_token,
    ...(typeof tokens.id_token === 'string' && tokens.id_token ? { id_token: tokens.id_token } : {}),
    expired: new Date(expiresAt).toISOString(), last_refresh: new Date(started).toISOString(),
    client_id: clientId, file_name: filename, disabled: false,
    ...(typeof auth.chatgpt_plan_type === 'string' ? { plan_type: auth.chatgpt_plan_type } : {}),
    ...(authorization.provider === 'oai-basispoints' ? { auth_kind: 'oauth', auth_mode: 'chatgpt', refresh_interval_seconds: 43200 } : {}),
    ...(authorization.simulated ? { simulated: true, credential_source: 'sandbox-fixture' } : {}),
  };
}

/** Credential acquisition only. Importing and verifying CPA registration is separate. */
export function createLoginEngine({ createClient, exchangeCode, lookupWorkspaces, verifyBusinessUsage, authorization = ADDIN_AUTHORIZATION, totpProvider, clock = Date.now, timeoutMs = 120000, authRetries = 0, authRetryBackoffMs = [800, 2500, 6000], lookupAttempts = 3, lookupRetryBackoffMs = [250, 750], waitImpl = delay } = {}) {
  return {
    async login(record, { signal, onProgress = () => {}, requestEmailCode, requestWorkspaceSelection } = {}) {
      if (typeof createClient !== 'function' || typeof exchangeCode !== 'function') throw Object.assign(fail('LOGIN_TRANSPORT_REQUIRED', '登录传输或令牌交换接口尚未接入'), { workspaceDiagnostics: { phase: 'discovery', attempt: 1, lookupAttempt: 0, reason: 'discovery_started' } });
      if (!Number.isInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 600000) throw Object.assign(fail('LOGIN_TIMEOUT_INVALID', '登录超时设置不正确'), { workspaceDiagnostics: { phase: 'discovery', attempt: 1, lookupAttempt: 0, reason: 'discovery_started' } });
      const email = String(record.email || '').trim();
      const password = typeof record.password === 'string' ? record.password : '';
      const secret = String(record.totpSecret || '').trim();
      const privateValues = [email, password, secret].filter(Boolean);
      const targetWorkspaceId = String(record.workspaceId || '').trim();
      const workspacePreference = record.workspacePreference || 'team_first';
      const businessFirst = workspacePreference === 'team_first' && !targetWorkspaceId;
      if (businessFirst && typeof lookupWorkspaces !== 'function') throw Object.assign(fail('WORKSPACE_LOOKUP_REQUIRED', '工作空间核验尚未接入，已停止导入'), { workspaceDiagnostics: { phase: 'inspection', attempt: 1, lookupAttempt: 0, reason: 'lookup_failed' } });
      const transaction = new AbortController();
      const cancelled = () => transaction.abort();
      if (signal?.aborted) cancelled(); else signal?.addEventListener('abort', cancelled, { once: true });
      let timedOut = false;
      const timer = setTimeout(() => { timedOut = true; transaction.abort(); }, timeoutMs);
      let client;
      let disposed = false;
      const closed = new WeakSet();
      const closeClient = async value => {
        if (!value || typeof value !== 'object' || closed.has(value) || typeof value.close !== 'function') return;
        closed.add(value);
        let closeTimer;
        try { await Promise.race([Promise.resolve().then(() => value.close()), new Promise(resolve => { closeTimer = setTimeout(resolve, 2000); })]); } catch {}
        finally { clearTimeout(closeTimer); }
      };
      let phase = 'session';
      let outcome;
      let lastEvent = '';
      let oauthAttempt = 1;
      let workspaceDiagnostics = { phase: 'discovery', attempt: 1, lookupAttempt: 0, reason: 'discovery_started' };
      const diagnostic = (stage, reason, details = {}) => {
        const safeId = value => typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$/.test(value) && !privateValues.some(secret => secret.length >= 4 && value.includes(secret));
        workspaceDiagnostics = {
          phase: stage, attempt: oauthAttempt, lookupAttempt: Number.isInteger(details.lookupAttempt) ? details.lookupAttempt : 0, reason,
          ...(safeId(details.expectedWorkspaceId) ? { expectedWorkspaceId: details.expectedWorkspaceId } : {}),
          ...(safeId(details.actualWorkspaceId) ? { actualWorkspaceId: details.actualWorkspaceId } : {}),
          ...(Number.isInteger(details.candidateCount) && details.candidateCount >= 0 && details.candidateCount <= 100 ? { candidateCount: details.candidateCount } : {}),
        };
      };
      const emit = event => {
        if (!PHASES.has(event.phase) || !['running', 'completed', 'waiting', 'failed', 'cancelled'].includes(event.status) || !codePattern.test(event.code)) return;
        phase = event.phase;
        const key = `${phase}|${event.status}|${event.code}`;
        if (lastEvent === key) return;
        lastEvent = key;
        try { const observation = onProgress(Object.freeze({ phase, status: event.status, code: event.code })); observation?.catch?.(() => {}); } catch {}
      };
      try {
        // A discovery token stays in memory and is never returned to the importer.
        const acquire = async (workspaceId, { discovery = false } = {}) => {
        diagnostic(oauthAttempt === 1 ? 'discovery' : 'business_reselect', oauthAttempt === 1 ? 'discovery_started' : 'business_selected', { expectedWorkspaceId: workspaceId });
        const verifier = randomBytes(32).toString('base64url');
        const state = randomBytes(24).toString('base64url');
        const challenge = createHash('sha256').update(verifier).digest('base64url');
        const url = new URL(authorization.authorizationUrl);
        for (const [key, value] of Object.entries({ client_id: authorization.clientId, response_type: 'code', redirect_uri: authorization.redirectUri, scope: authorization.scope, state, code_challenge: challenge, code_challenge_method: 'S256', ...(authorization.audience ? { audience: authorization.audience } : {}) })) url.searchParams.set(key, value);
        privateValues.push(state, verifier, url.toString());
        const created = await abortable(async () => {
          const value = await createClient({ email, password, totpSecret: secret, targetWorkspaceId: workspaceId, workspacePreference, requestWorkspaceSelection, expectedState: state, signal: transaction.signal, log: () => {} });
          if (disposed || transaction.signal.aborted) { await closeClient(value); throw fail('CANCELLED', '登录已取消'); }
          client = value;
          return value;
        }, transaction.signal);
        client = created;
        const result = await runLoginStateMachine({
          client, authUrl: url.toString(), email, password, totpSecret: secret,
          totpFactorId: record.totpFactorId, targetWorkspaceId: workspaceId, expectedState: state,
          signal: transaction.signal, requestEmailCode, onProgress: emit,
          totpProvider: totpProvider || { getCode: options => getLoginTotp({ ...options, clock }) },
          authRetries, authRetryBackoffMs,
        });
        privateValues.push(result.code);
        if (transaction.signal.aborted) throw fail('CANCELLED', '登录已取消');
        emit({ phase: 'exchange', status: 'running', code: 'TOKEN_EXCHANGE_STARTED' });
        const tokens = await abortable(() => exchangeCode({ code: result.code, verifier, redirectUri: authorization.redirectUri, clientId: authorization.clientId, tokenUrl: authorization.tokenUrl, signal: transaction.signal }), transaction.signal);
        for (const key of ['access_token', 'refresh_token', 'id_token']) if (typeof tokens?.[key] === 'string') privateValues.push(tokens[key]);
        if (transaction.signal.aborted) throw fail('CANCELLED', '登录已取消');
        emit({ phase: 'exchange', status: 'completed', code: 'TOKEN_EXCHANGE_COMPLETED' });
        if (transaction.signal.aborted) throw fail('CANCELLED', '登录已取消');
        emit({ phase: 'validate', status: 'running', code: 'TOKEN_VALIDATION_STARTED' });
        const expectedWorkspaceId = workspaceId || result.workspaceId;
        const payload = tokens && typeof tokens.access_token === 'string' && tokens.access_token && typeof tokens.refresh_token === 'string' && tokens.refresh_token ? jwtPayload(tokens.access_token) : {};
        const actualWorkspaceId = payload['https://api.openai.com/auth']?.chatgpt_account_id;
        if (expectedWorkspaceId && actualWorkspaceId && expectedWorkspaceId !== actualWorkspaceId) diagnostic(oauthAttempt === 1 ? 'discovery' : 'business_reselect', oauthAttempt === 1 ? 'discovery_claim_mismatch' : 'reselect_claim_mismatch', { expectedWorkspaceId, actualWorkspaceId });
        // Only the automatic discovery pass may inspect a token from a different callback workspace.
        // It is never imported until an active canonical Business binding is proven below.
        const credential = buildLoginCredential(tokens, { authorization, email, targetWorkspaceId: discovery ? undefined : expectedWorkspaceId, clock });
        const bindingVerified = Boolean(expectedWorkspaceId && credential.account_id === expectedWorkspaceId);
        const memberEmail = payload.email || payload['https://api.openai.com/profile']?.email;
        diagnostic(oauthAttempt === 1 ? 'discovery' : 'business_reselect', bindingVerified ? 'discovery_acquired' : oauthAttempt === 1 ? 'discovery_claim_mismatch' : 'reselect_claim_mismatch', { expectedWorkspaceId, actualWorkspaceId: credential.account_id });
        // Workspace lookup shares the worker's session limit. Release OAuth before querying it.
        await closeClient(client);
        client = undefined;
        return { credential, pageTypes: result.pageTypes, bindingVerified, memberEmail: typeof memberEmail === 'string' ? memberEmail.toLowerCase() : '' };
        };
        let acquired;
        try { acquired = await acquire(targetWorkspaceId, { discovery: businessFirst }); }
        catch (error) {
          if (error.code === 'TOKEN_ACCOUNT_MISMATCH') diagnostic('discovery', 'member_mismatch', { expectedWorkspaceId: targetWorkspaceId });
          throw error;
        }
        let workspaceSelection;
        if (businessFirst) {
          const inspect = async (credential, { acquisition, allowUsageFallback = false } = {}) => {
            const attempts = Number.isInteger(lookupAttempts) ? Math.max(1, Math.min(3, lookupAttempts)) : 3;
            let lastError;
            for (let lookupAttempt = 1; lookupAttempt <= attempts; lookupAttempt++) {
              emit({ phase: 'validate', status: 'running', code: 'WORKSPACE_CHECK_STARTED' });
              diagnostic('inspection', 'lookup_failed', { lookupAttempt, actualWorkspaceId: credential.account_id });
              let inspection, selected, validInspection = false;
              try {
                inspection = await abortable(() => lookupWorkspaces(credential.access_token, { signal: transaction.signal }), transaction.signal);
                selected = workspacePolicy.chooseBusinessWorkspace(inspection, { currentWorkspaceId: credential.account_id, now: clock() });
                validInspection = true;
              } catch (error) {
                if (transaction.signal.aborted || error.code === 'CANCELLED') throw error;
                lastError = error;
                if (error.retryable !== true) throw error;
              }
              if (validInspection) {
                const candidateCount = inspection.workspaces.filter(item => item.kind === 'business' && item.active && !item.deactivated && !item.viewOnly && workspacePolicy.isBusinessPlan(item.plan) && (item.expiresAt === null || Date.parse(item.expiresAt) > clock())).length;
                const current = inspection.workspaces.find(item => item.id === credential.account_id || Array.isArray(item.aliases) && item.aliases.includes(credential.account_id));
                const details = { lookupAttempt, actualWorkspaceId: credential.account_id, candidateCount };
                if (inspection.complete !== true) {
                  diagnostic('inspection', 'incomplete_workspace_list', details);
                  lastError = fail('WORKSPACE_LOOKUP_INCOMPLETE', '工作空间列表尚未完整，已停止导入', { retryable: true });
                } else if (!current) {
                  diagnostic('inspection', 'inspection_missing_current', details);
                  lastError = fail('WORKSPACE_LOOKUP_INCOMPLETE', '工作空间核验暂未包含当前令牌身份，已停止导入', { retryable: true });
                  // A complete directory may omit the account to which an already-bound
                  // Business token belongs. Verify that exact account independently;
                  // this never makes a Personal token or a mismatched callback eligible.
                  if (allowUsageFallback && workspacePolicy.isBusinessPlan(credential.plan_type)) {
                    if (acquisition?.bindingVerified !== true || acquisition.memberEmail !== email.toLowerCase()) {
                      diagnostic('inspection', 'business_identity_unverified', details);
                      throw fail('BUSINESS_USAGE_UNVERIFIED', '当前 Business 账号身份未能严格核验，已停止导入');
                    }
                    try {
                      if (typeof verifyBusinessUsage !== 'function') throw fail('BUSINESS_USAGE_UNVERIFIED');
                      const usage = await abortable(() => verifyBusinessUsage(credential.access_token, { accountId: credential.account_id, signal: transaction.signal }), transaction.signal);
                      const checked = validateBusinessUsage(usage, credential.account_id, clock(), privateValues);
                      diagnostic('final', 'business_usage_verified', details);
                      return { inspection, businessUsage: checked, candidateCount, lookupAttempt };
                    } catch (error) {
                      if (transaction.signal.aborted || error?.code === 'CANCELLED') throw error;
                      diagnostic('inspection', 'business_usage_failed', details);
                      throw fail('BUSINESS_USAGE_UNVERIFIED', '当前 Business 套餐未能独立核验，已停止导入', { retryable: true });
                    }
                  }
                } else if (!selected) {
                  diagnostic('inspection', 'no_active_business', details);
                  lastError = fail('BUSINESS_WORKSPACE_NOT_FOUND', '未确认有效 Business 工作空间，已停止导入', { retryable: true });
                } else {
                  return { inspection, selected, current, candidateCount, lookupAttempt };
                }
              }
              if (lookupAttempt === attempts) throw lastError || fail('WORKSPACE_LOOKUP_INCOMPLETE', '工作空间核验未完成，已停止导入', { retryable: true });
              const configured = Array.isArray(lookupRetryBackoffMs) ? lookupRetryBackoffMs[Math.min(lookupAttempt - 1, lookupRetryBackoffMs.length - 1)] : 0;
              const backoff = Number.isFinite(configured) ? Math.max(0, Math.min(5000, configured)) : 0;
              await abortable(() => waitImpl(backoff, undefined, { signal: transaction.signal }), transaction.signal);
            }
          };
          const first = await inspect(acquired.credential, { acquisition: acquired, allowUsageFallback: true });
          if (first.businessUsage) {
            workspaceSelection = { policy: 'team_first', kind: 'business', id: acquired.credential.account_id, name: 'Business', reason: 'current_account_business_usage', reselected: false };
          } else {
            let final = first;
            const discovery = acquired;
            const reselected = !acquired.bindingVerified || first.selected.id !== acquired.credential.account_id || !workspacePolicy.isBusinessPlan(acquired.credential.plan_type);
            if (reselected) {
              emit({ phase: 'authorization', status: 'running', code: 'BUSINESS_WORKSPACE_SELECTED' });
              oauthAttempt = 2;
              try { acquired = await acquire(first.selected.id); }
              catch (error) {
                if (error.code === 'TOKEN_ACCOUNT_MISMATCH') diagnostic('business_reselect', 'member_mismatch', { expectedWorkspaceId: first.selected.id });
                throw error;
              }
              const sameUser = discovery.credential.user_id && acquired.credential.user_id && discovery.credential.user_id === acquired.credential.user_id;
              const sameEmail = discovery.memberEmail && acquired.memberEmail && discovery.memberEmail === acquired.memberEmail;
              if (discovery.credential.user_id && acquired.credential.user_id && !sameUser || !sameUser && !sameEmail) {
                diagnostic('final', 'member_mismatch', { expectedWorkspaceId: first.selected.id, actualWorkspaceId: acquired.credential.account_id });
                throw fail('TOKEN_ACCOUNT_MISMATCH', '重新授权的成员身份不一致，已停止导入');
              }
              final = await inspect(acquired.credential);
            }
            if (acquired.credential.account_id !== first.selected.id || final.selected.id !== first.selected.id || !workspacePolicy.isBusinessPlan(acquired.credential.plan_type)) {
              diagnostic('final', 'business_plan_mismatch', { expectedWorkspaceId: first.selected.id, actualWorkspaceId: acquired.credential.account_id, candidateCount: final.candidateCount, lookupAttempt: final.lookupAttempt });
              throw fail('BUSINESS_WORKSPACE_UNVERIFIED', 'Business 工作空间或订阅校验未通过，已停止导入', { retryable: true });
            }
            diagnostic('final', 'business_verified', { expectedWorkspaceId: first.selected.id, actualWorkspaceId: acquired.credential.account_id, candidateCount: final.candidateCount, lookupAttempt: final.lookupAttempt });
            workspaceSelection = { policy: 'team_first', kind: 'business', id: first.selected.id, name: final.current.name, reason: 'active_business', reselected };
          }
        }
        emit({ phase: 'validate', status: 'completed', code: 'CREDENTIAL_VALIDATED' });
        if (transaction.signal.aborted) throw fail('CANCELLED', '登录已取消');
        const credential = acquired.credential;
        outcome = { credential, filename: credential.file_name, accountId: credential.account_id, workspaceId: credential.workspace_id, pageTypes: acquired.pageTypes, simulated: Boolean(authorization.simulated), ...(workspaceSelection ? { workspaceSelection, workspaceDiagnostics: { ...workspaceDiagnostics } } : {}) };
      } catch (error) {
        const converted = timedOut ? fail('LOGIN_TIMEOUT', '登录等待已超时，请检查当前阶段', { retryable: true }) : transaction.signal.aborted ? fail('CANCELLED', '登录已取消') : sanitizeLoginError(error, privateValues);
        converted.phase = phase;
        if (timedOut || transaction.signal.aborted) workspaceDiagnostics = { ...workspaceDiagnostics, reason: timedOut ? 'timeout' : 'cancelled' };
        converted.workspaceDiagnostics = { ...workspaceDiagnostics };
        if (!converted.retryable && converted.code === 'LOGIN_STEP_FAILED' && POST_CREDENTIAL_PHASES.has(phase)) converted.retryable = true;
        emit({ phase, status: converted.code === 'CANCELLED' ? 'cancelled' : 'failed', code: converted.code });
        throw converted;
      } finally {
        disposed = true; clearTimeout(timer);
        await closeClient(client);
        signal?.removeEventListener('abort', cancelled);
      }
      if (transaction.signal.aborted || signal?.aborted) { const error = fail('CANCELLED', '登录已取消'); error.phase = phase; error.workspaceDiagnostics = { ...workspaceDiagnostics, reason: 'cancelled' }; emit({ phase, status: 'cancelled', code: error.code }); throw error; }
      return outcome;
    },
  };
}

export function createHttpsTokenExchange({ fetchImpl = fetch, allowedHost = 'auth.openai.com' } = {}) {
  return async ({ code, verifier, redirectUri, clientId, tokenUrl, signal }) => {
    const url = new URL(tokenUrl);
    if (url.protocol !== 'https:' || url.hostname !== allowedHost || url.username || url.password) throw fail('TOKEN_ENDPOINT_NOT_ALLOWED', '令牌交换地址未被允许');
    const response = await fetchImpl(url, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: new URLSearchParams({ grant_type: 'authorization_code', code, code_verifier: verifier, redirect_uri: redirectUri, client_id: clientId }), signal, redirect: 'error' });
    if (!response.ok) throw fail('TOKEN_EXCHANGE_REJECTED', '授权结果换取令牌失败', { httpStatus: response.status, retryable: response.status >= 500 });
    try { return await response.json(); } catch { throw fail('TOKEN_RESPONSE_INVALID', '令牌响应格式不正确'); }
  };
}
