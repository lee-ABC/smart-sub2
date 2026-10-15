import { SDK_EXCEPTION_CAUSES } from './sdk-diagnostics.js';

const PAGE_TYPES = {
  password: new Set(['login_password', 'password', 'password_verification']),
  mfa: new Set(['mfa', 'mfa_challenge', 'totp', 'totp_verification', 'two_factor', 'two_factor_verification', 'two-factor']),
  email_otp: new Set(['email_otp', 'email_otp_verification', 'email_verification', 'otp_verification']),
  authorization: new Set(['consent', 'codex', 'workspace', 'workspace_selection', 'workspace_select', 'select_workspace', 'oauth', 'oauth_consent', 'external_url', 'authorization_code', 'oauth_callback']),
};

const MESSAGES = {
  CANCELLED: '登录已取消',
  INVALID_LOGIN_CLIENT: '登录引擎接口不完整',
  AUTH_URL_REQUIRED: '缺少授权地址',
  EMAIL_REQUIRED: '缺少账号邮箱',
  PASSWORD_REQUIRED: '服务端要求密码，请补充账号密码',
  MFA_REQUIRED: '服务端要求两步验证，请补充 TOTP 资料',
  MFA_FACTOR_REQUIRED: '服务端未提供可回答的 TOTP 验证项',
  MFA_FACTOR_UNSUPPORTED: '此两步验证方式尚未接入，请人工处理',
  MFA_FACTOR_MISMATCH: '两步验证项与提供的资料不一致，已停止提交',
  TOTP_PROVIDER_REQUIRED: 'TOTP 计算服务未接入',
  TOTP_CODE_INVALID: 'TOTP 计算服务未返回有效验证码',
  EMAIL_CODE_REQUIRED: '服务端要求邮箱验证码，请接入验证码输入',
  EMAIL_CODE_INVALID: '请输入有效的邮箱验证码',
  REPEATED_LOGIN_STATE: '登录页面未推进，已停止重复提交',
  PHONE_STEP_UNSUPPORTED: '登录需要手机验证，请人工处理',
  PASSWORD_RESET_REQUIRED: '登录需要设置或重置密码，请人工处理',
  UNKNOWN_LOGIN_PAGE: '服务端返回了尚未接入的登录步骤',
  INVALID_LOGIN_RESPONSE: '登录引擎未返回有效的步骤信息',
  AUTHORIZATION_CODE_REQUIRED: '登录引擎未返回授权码',
  AUTHORIZATION_CLIENT_REQUIRED: '登录引擎缺少授权码获取接口',
  WORKSPACE_MISMATCH: '返回的工作空间与选择不一致',
  WORKSPACE_UNVERIFIED: '登录引擎无法确认所选工作空间',
  WORKSPACE_LOOKUP_REQUIRED: '工作空间核验尚未接入，已停止导入',
  WORKSPACE_LOOKUP_INCOMPLETE: '工作空间列表未能完整核验，已停止导入，请稍后重试',
  BUSINESS_WORKSPACE_NOT_FOUND: '未核验到有效 Business 工作空间，已停止导入，不会回落到 Free',
  BUSINESS_WORKSPACE_UNVERIFIED: 'Business 工作空间或订阅校验未通过，已停止导入',
  BUSINESS_USAGE_UNVERIFIED: '当前账号的 Business 实际套餐未能确认，已停止导入，请查看核验详情',
  OAUTH_STATE_MISMATCH: '授权回调校验失败',
  OAUTH_STATE_UNVERIFIED: '登录引擎无法确认授权回调',
  INVALID_HOP_LIMIT: '登录步骤上限无效',
  LOGIN_HOP_LIMIT: '登录步骤超过上限，已停止提交',
  LOGIN_STEP_FAILED: '登录阶段失败，请检查错误类别',
  SDK_RESULT_MISSING: 'SDK 未返回可检查的结果，本次尝试已停止',
  SDK_RESULT_INVALID_JSON: 'SDK 输出格式错误，本次尝试已停止',
  SDK_RESULT_INVALID_STRUCTURE: 'SDK 输出结构错误，本次尝试已停止',
  SDK_RESULT_INVALID_ENVELOPE: 'SDK 结果信封不完整，本次尝试已停止',
  SDK_RESULT_TOO_LARGE: 'SDK 输出超过诊断大小限制，本次尝试已停止',
  SDK_RESULT_INCOMPLETE: 'SDK 输出缺少完整上下文字段，本次尝试已停止',
  SDK_FLOW_MISMATCH: 'SDK 输出与当前操作不一致，本次尝试已停止',
  SDK_INTERNAL_ERROR_PAYLOAD: 'SDK 返回内部异常报文，本次尝试已停止，请使用官方交互流程验证',
  SDK_VERSION_DRIFT: '验证组件版本发生变化，本次尝试已停止，请先核验新版本',
  LOGIN_TRANSPORT_INCOMPLETE: '旧登录传输接口不完整',
  SDK_DIAGNOSTICS_REQUIRED: '旧传输缺少可检查的验证组件接口',
  UPSTREAM_UNAVAILABLE: '登录服务暂时不可用，请稍后重试',
  LOGIN_RATE_LIMITED: '登录请求受到限制，请稍后再试',
  LOGIN_REQUEST_REJECTED: '登录请求未通过，请检查错误类别',
  AUTHORIZATION_REDIRECT_LOOP: '授权跳转没有推进，已停止重复请求',
  AUTHORIZATION_REDIRECT_NOT_ALLOWED: '授权跳转到了未允许的地址，已停止',
  AUTHORIZATION_REDIRECT_FAILED: '授权跳转没有返回下一步，已保留当前失败原因',
  AUTHORIZATION_REDIRECT_LIMIT: '授权跳转次数超过上限',
  AUTHORIZATION_REJECTED: '授权未完成或已被拒绝',
  WORKSPACE_NOT_FOUND: '登录会话中没有选择的工作空间',
  WORKSPACE_SELECTION_REQUIRED: '账号有多个工作空间，请选择后重新提交',
  AUTHORIZATION_URL_REQUIRED: '工作空间授权没有返回继续地址',
  LOGIN_TEMPORARY_FAILURE: '演示登录服务暂时失败，可以重试',
  TOTP_SECRET_INVALID: '两步验证密钥格式不正确',
  PURE_PASSWORD_VERIFY_FAILED: '密码验证失败，请检查错误类别',
  PURE_PASSWORD_INVALID: '账号密码未通过验证',
  PURE_MFA_CODE_INCORRECT: '两步验证码未通过验证',
  PURE_MFA_LOCKED: '两步验证受到限制，请稍后人工处理',
  PURE_MFA_VERIFY_FAILED: '两步验证未通过，请检查错误类别',
  PURE_MFA_ISSUE_FAILED: '两步验证挑战未完成，请检查错误类别',
  PURE_MFA_TOTP_CALC_FAILED: '无法生成有效的两步验证码',
  PURE_SUBMIT_EMAIL_FAILED: '邮箱提交未通过，请检查错误类别',
  PURE_EMAIL_OTP_FAILED: '邮箱验证码未通过，请检查错误类别',
  EMAIL_OTP_INCORRECT: '邮箱验证码无效或已经过期',
  ACCOUNT_UNUSABLE: '此账号目前无法用于登录',
  CLOUDFLARE_BLOCK: '登录服务要求额外验证，请使用官方交互流程处理',
  SENTINEL_REJECTED: '登录服务未接受验证结果，请使用官方交互流程处理',
  ACCOUNT_MFA_REQUIRED: '账号开启了两步验证但未能自动完成，请人工处理',
  RT_WORKSPACE_NOT_FOUND: '账号中没有可用于该渠道的工作空间',
  PURE_BOOTSTRAP_NO_SESSION: '未取得有效的登录会话',
  PURE_BOOTSTRAP_NOT_ADVANCED: '登录会话未推进到下一步',
  PURE_BOOTSTRAP_FAILED: '登录会话初始化未完成',
  LOGIN_TRANSPORT_REQUIRED: '登录传输或令牌交换接口尚未接入',
  LOGIN_TIMEOUT_INVALID: '登录超时设置不正确',
  LOGIN_TIMEOUT: '登录等待已超时，请检查当前阶段',
  LOGIN_ENGINE_FAILED: '登录处理失败，请查看错误类别',
  TOKEN_STRUCTURE_INVALID: '访问令牌结构不正确',
  TOKEN_INCOMPLETE: '授权结果缺少访问令牌或刷新令牌',
  TOKEN_CLIENT_MISMATCH: '返回的令牌不属于选择的账号渠道',
  TOKEN_WORKSPACE_MISSING: '令牌缺少有效的账号或工作空间标识',
  TOKEN_WORKSPACE_MISMATCH: '令牌属于其他工作空间，已停止导入',
  TOKEN_ACCOUNT_MISMATCH: '返回令牌的账号与输入邮箱不一致',
  TOKEN_EXPIRY_INVALID: '令牌有效期缺失或不正确',
  TOKEN_EXPIRED: '返回的令牌已经过期',
  TOKEN_ENDPOINT_NOT_ALLOWED: '令牌交换地址未被允许',
  TOKEN_EXCHANGE_REJECTED: '授权结果换取令牌失败',
  TOKEN_EXCHANGE_TEMPORARY_FAILURE: '演示令牌交换服务暂时失败，可以重试',
  TOKEN_RESPONSE_INVALID: '令牌响应格式不正确',
};

// Untrusted callback and response codes are values, not public text. Only this
// finite vocabulary can cross the error boundary; unknown codes are discarded.
const UPSTREAM_CAUSE_CODES = new Set([
  ...SDK_EXCEPTION_CAUSES,
  ...Object.keys(MESSAGES),
  'invalid_request', 'unauthorized_client', 'access_denied', 'unsupported_response_type',
  'invalid_scope', 'server_error', 'temporarily_unavailable', 'invalid_grant',
  'invalid_client', 'unsupported_grant_type', 'account_deactivated', 'invalid_auth_step',
  'invalid_state', 'invalid_password', 'incorrect_password', 'password_invalid',
  'invalid_credentials', 'rate_limit_exceeded', 'too_many_requests', 'mfa_required',
  'invalid_code', 'incorrect_code', 'expired_code', 'authentication_error',
]);

function safeCode(value) {
  return typeof value === 'string' && /^[A-Za-z][A-Za-z0-9_-]{0,79}$/.test(value) ? value : undefined;
}

function safeHttpStatus(value) {
  return Number.isInteger(value) && value >= 100 && value <= 599 ? value : undefined;
}

export const WORKSPACE_DIAGNOSTIC_REASONS = Object.freeze([
  'discovery_started', 'discovery_acquired', 'discovery_claim_mismatch',
  'incomplete_workspace_list', 'inspection_missing_current', 'no_active_business',
  'lookup_failed', 'business_selected', 'reselect_claim_mismatch', 'member_mismatch',
  'business_plan_mismatch', 'business_verified', 'business_usage_verified', 'business_usage_failed', 'business_identity_unverified', 'cancelled', 'timeout',
]);
const WORKSPACE_DIAGNOSTIC_PHASES = new Set(['discovery', 'business_reselect', 'inspection', 'final']);
const WORKSPACE_DIAGNOSTIC_REASON_SET = new Set(WORKSPACE_DIAGNOSTIC_REASONS);
const WORKSPACE_DIAGNOSTIC_FIELDS = ['phase', 'attempt', 'lookupAttempt', 'reason', 'expectedWorkspaceId', 'actualWorkspaceId', 'candidateCount'];

/** Only bounded decision metadata may cross the error or history boundary. */
export function sanitizeWorkspaceDiagnostics(value, privateValues = []) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined;
  let fields;
  try { fields = Object.getOwnPropertyDescriptors(value); } catch { return undefined; }
  for (const key of WORKSPACE_DIAGNOSTIC_FIELDS) {
    if (Object.hasOwn(fields, key) && !Object.hasOwn(fields[key], 'value')) return undefined;
  }
  const read = key => fields[key]?.value;
  const phase = read('phase'), reason = read('reason');
  if (!WORKSPACE_DIAGNOSTIC_PHASES.has(phase) || !WORKSPACE_DIAGNOSTIC_REASON_SET.has(reason)) return undefined;
  const result = { phase, reason };
  for (const key of ['attempt', 'lookupAttempt', 'candidateCount']) {
    const number = read(key);
    if (number === undefined) continue;
    if (!Number.isSafeInteger(number) || number < (key === 'attempt' ? 1 : 0) || number > (key === 'lookupAttempt' ? 3 : 100)) return undefined;
    result[key] = number;
  }
  for (const key of ['expectedWorkspaceId', 'actualWorkspaceId']) {
    const id = read(key);
    if (id === undefined) continue;
    if (typeof id !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$/.test(id) || /^[^.]+\.[^.]+\.[^.]+$/.test(id)) return undefined;
    result[key] = id;
  }
  const encoded = JSON.stringify(result);
  if (Array.isArray(privateValues) && privateValues.some(secret => typeof secret === 'string' && secret.length >= 4 && encoded.includes(secret))) return undefined;
  return result;
}

// Public errors deliberately exclude the upstream message, response and cause object.
export class LoginFlowError extends Error {
  constructor(code, message, { causeCode, httpStatus, statusCode, retryable = false, workspaceDiagnostics } = {}) {
    const publicCode = safeCode(code) || 'LOGIN_STEP_FAILED';
    super(message || MESSAGES[publicCode] || MESSAGES.LOGIN_STEP_FAILED);
    this.name = 'LoginFlowError';
    this.code = publicCode;
    const publicCause = safeCode(causeCode);
    if (publicCause) this.causeCode = publicCause;
    const status = safeHttpStatus(httpStatus) ?? safeHttpStatus(statusCode);
    if (status !== undefined) { this.httpStatus = status; this.statusCode = status; }
    this.retryable = retryable === true;
    const diagnostics = sanitizeWorkspaceDiagnostics(workspaceDiagnostics);
    if (diagnostics) this.workspaceDiagnostics = diagnostics;
  }
}

const flowError = (code) => new LoginFlowError(code, MESSAGES[code]);

function throwIfAborted(signal) {
  if (signal?.aborted) throw flowError('CANCELLED');
}

function abortableDelay(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(flowError('CANCELLED'));
    const onAbort = () => { clearTimeout(timer); reject(flowError('CANCELLED')); };
    const timer = setTimeout(() => { signal?.removeEventListener('abort', onAbort); resolve(); }, ms);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

// The final authorization hop (workspace select → follow the external_url redirects → capture the
// code at the callback) is where the upstream intermittently rejects an already-authenticated
// session: it surfaces as a bare transport error or a generic LOGIN_STEP_FAILED and clears when the
// same live session is asked again. Those are retried in place. The outcomes below are permanent for
// this session, so they are never retried: a changed workspace/MFA state, a deactivated account, or
// a verification wall that another retry on the same egress cannot pass.
const PERMANENT_AUTH_CODES = new Set([
  'CANCELLED', 'ACCOUNT_UNUSABLE', 'ACCOUNT_MFA_REQUIRED', 'RT_WORKSPACE_NOT_FOUND',
  'CLOUDFLARE_BLOCK', 'SENTINEL_REJECTED', 'WORKSPACE_MISMATCH', 'WORKSPACE_UNVERIFIED',
  'OAUTH_STATE_MISMATCH', 'OAUTH_STATE_UNVERIFIED', 'AUTHORIZATION_CLIENT_REQUIRED',
  'TOKEN_CLIENT_MISMATCH', 'MFA_FACTOR_MISMATCH', 'WORKSPACE_SELECTION_REQUIRED', 'WORKSPACE_NOT_FOUND',
]);

function retryableAuthStep(error) {
  const code = safeCode(error?.code);
  return !(code && PERMANENT_AUTH_CODES.has(code));
}

// An injected client must also use the forwarded signal to close its transport.
// This race prevents an uncooperative pending request from advancing the flow.
function abortableOperation(operation, signal) {
  throwIfAborted(signal);
  return new Promise((resolve, reject) => {
    let settled = false;
    const cleanup = () => signal?.removeEventListener('abort', abort);
    const finish = (handler, value) => {
      if (settled) return;
      settled = true;
      cleanup();
      handler(value);
    };
    const abort = () => finish(reject, flowError('CANCELLED'));
    signal?.addEventListener('abort', abort, { once: true });
    Promise.resolve().then(() => {
      throwIfAborted(signal);
      return operation();
    }).then(value => {
      if (signal?.aborted) return abort();
      finish(resolve, value);
    }, error => finish(reject, error));
  });
}

function pageInfo(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data) || typeof data.page?.type !== 'string') {
    throw flowError('INVALID_LOGIN_RESPONSE');
  }
  const type = data.page.type.trim().toLowerCase();
  if (/^(?:add[-_]phone(?:[-_].*)?|phone(?:[-_].*)?)$/.test(type)) throw flowError('PHONE_STEP_UNSUPPORTED');
  if (/^(?:reset_password(?:_.*)?|password_reset(?:_.*)?|new_password|set_password|password_add)$/.test(type)) {
    throw flowError('PASSWORD_RESET_REQUIRED');
  }
  for (const [phase, types] of Object.entries(PAGE_TYPES)) if (types.has(type)) return { type, phase };
  throw flowError('UNKNOWN_LOGIN_PAGE');
}

function mfaFactor(data, expectedFactorId) {
  const payload = data.page.payload || {};
  const factors = Array.isArray(payload.factors) ? payload.factors : [];
  const explicitId = typeof payload.factor_id === 'string' ? payload.factor_id.trim() : '';
  const totpFactor = factors.find(factor => String(factor?.factor_type || '').toLowerCase() === 'totp');
  const factorId = explicitId || (typeof totpFactor?.id === 'string' ? totpFactor.id.trim() : '');
  if (!factorId) throw flowError('MFA_FACTOR_REQUIRED');
  const explicitFactor = factors.find(factor => factor?.id === factorId);
  if (explicitId && factors.length && !explicitFactor) throw flowError('MFA_FACTOR_MISMATCH');
  const factorType = String(payload.factor_type || explicitFactor?.factor_type || '').toLowerCase();
  if (factorType && factorType !== 'totp') throw flowError('MFA_FACTOR_UNSUPPORTED');
  if (factors.length && !totpFactor && !factorType) throw flowError('MFA_FACTOR_UNSUPPORTED');
  if (expectedFactorId && expectedFactorId !== factorId) throw flowError('MFA_FACTOR_MISMATCH');
  return factorId;
}

function codeValue(value, errorCode) {
  const code = typeof value === 'string' ? value : value?.code;
  if (typeof code !== 'string' || !/^\d{6,8}$/.test(code)) throw flowError(errorCode);
  return code;
}

export function sanitizeLoginError(error, privateValues = []) {
  const secrets = Array.isArray(privateValues) ? privateValues.filter(value => typeof value === 'string' && value) : [];
  const privateCode = value => typeof value === 'string' && secrets.some(secret => secret.length >= 4 && value.includes(secret));
  const rawCode = safeCode(error?.code);
  const rawCauseCode = safeCode(error?.causeCode);
  const code = !privateCode(rawCode) && rawCode && UPSTREAM_CAUSE_CODES.has(rawCode) ? rawCode : undefined;
  const causeCode = !privateCode(rawCauseCode) && rawCauseCode && UPSTREAM_CAUSE_CODES.has(rawCauseCode) ? rawCauseCode : undefined;
  return new LoginFlowError(code || 'LOGIN_STEP_FAILED', MESSAGES[code] || MESSAGES.LOGIN_STEP_FAILED, {
    causeCode,
    httpStatus: safeHttpStatus(error?.httpStatus) ?? safeHttpStatus(error?.statusCode),
    retryable: Boolean(code) && code !== 'LOGIN_STEP_FAILED' && error?.retryable === true,
    workspaceDiagnostics: sanitizeWorkspaceDiagnostics(error?.workspaceDiagnostics, secrets),
  });
}

/**
 * Run one login flow against an injected, raw-response client. This module does
 * not construct authorization URLs, make network requests or close the client.
 */
export async function runLoginStateMachine({
  client, authUrl, email, password, totpSecret, totpFactorId, totpProvider,
  requestEmailCode, targetWorkspaceId, signal, onProgress = () => {},
  maxHops = 12, expectedState, authRetries = 0, authRetryBackoffMs = [],
} = {}) {
  let currentPhase = 'session';
  const privateValues = [email, password, totpSecret, authUrl, expectedState]
    .filter(value => typeof value === 'string' && value)
    .flatMap(value => [value, value.trim()]);
  const emit = (phase, status, code) => {
    try {
      const result = onProgress(Object.freeze({ phase, status, code }));
      if (result && typeof result.catch === 'function') result.catch(() => {});
    } catch { /* Observer failures must not repeat authentication operations. */ }
  };
  const invoke = (method, ...args) => abortableOperation(() => {
    if (typeof client?.[method] !== 'function') throw flowError('INVALID_LOGIN_CLIENT');
    return client[method](...args);
  }, signal);
  const step = async (phase, method, args, successCode) => {
    currentPhase = phase;
    throwIfAborted(signal);
    emit(phase, 'running', `${phase.toUpperCase()}_STARTED`);
    const result = await invoke(method, ...args);
    throwIfAborted(signal);
    emit(phase, 'completed', successCode);
    return result;
  };

  try {
    throwIfAborted(signal);
    if (!client || typeof client !== 'object') throw flowError('INVALID_LOGIN_CLIENT');
    if (typeof authUrl !== 'string' || !authUrl) throw flowError('AUTH_URL_REQUIRED');
    if (typeof email !== 'string' || !email.trim()) throw flowError('EMAIL_REQUIRED');
    if (!Number.isInteger(maxHops) || maxHops < 1 || maxHops > 128) throw flowError('INVALID_HOP_LIMIT');
    if (targetWorkspaceId != null && typeof targetWorkspaceId !== 'string') throw flowError('WORKSPACE_MISMATCH');
    if (expectedState != null && typeof expectedState !== 'string') throw flowError('OAUTH_STATE_MISMATCH');

    await step('session', 'bootstrapAuthorize', [authUrl, { signal }], 'SESSION_READY');
    let data = await step('email', 'submitEmail', [email.trim(), { autoPassword: false, raw: true, signal }], 'EMAIL_ACCEPTED');
    const pageTypes = [];
    const seenPages = new Set();
    const seenFactors = new Set();

    for (let hop = 0; hop < maxHops; hop++) {
      throwIfAborted(signal);
      const { type, phase } = pageInfo(data);
      currentPhase = phase;
      pageTypes.push(type);
      if (seenPages.has(phase)) throw flowError('REPEATED_LOGIN_STATE');
      seenPages.add(phase);

      if (phase === 'password') {
        if (typeof password !== 'string' || !password) throw flowError('PASSWORD_REQUIRED');
        data = await step('password', 'submitPassword', [password, { raw: true, signal }], 'PASSWORD_VERIFIED');
        continue;
      }

      if (phase === 'mfa') {
        if (typeof totpSecret !== 'string' || !totpSecret) throw flowError('MFA_REQUIRED');
        const factorId = mfaFactor(data, totpFactorId);
        if (seenFactors.has(factorId)) throw flowError('REPEATED_LOGIN_STATE');
        seenFactors.add(factorId);
        const getTotpCode = typeof totpProvider === 'function'
          ? () => totpProvider(totpSecret)
          : typeof totpProvider?.getCode === 'function'
            ? () => totpProvider.getCode({ secret: totpSecret, signal })
            : null;
        if (!getTotpCode) throw flowError('TOTP_PROVIDER_REQUIRED');
        throwIfAborted(signal);
        emit('mfa', 'running', 'MFA_STARTED');
        await invoke('issueMfaChallenge', factorId, { signal });
        const code = codeValue(await abortableOperation(getTotpCode, signal), 'TOTP_CODE_INVALID');
        data = await invoke('verifyMfaTotp', factorId, code, { raw: true, signal });
        throwIfAborted(signal);
        emit('mfa', 'completed', 'MFA_VERIFIED');
        continue;
      }

      if (phase === 'email_otp') {
        if (typeof requestEmailCode !== 'function') throw flowError('EMAIL_CODE_REQUIRED');
        throwIfAborted(signal);
        emit('email_otp', 'waiting', 'EMAIL_CODE_REQUIRED');
        const code = codeValue(await abortableOperation(() => requestEmailCode({ signal }), signal), 'EMAIL_CODE_INVALID');
        data = await step('email_otp', 'submitEmailOtp', [code, { raw: true, signal }], 'EMAIL_CODE_VERIFIED');
        continue;
      }

      const options = { targetWorkspaceId, expectedState, signal };
      emit('authorization', 'running', 'AUTHORIZATION_STARTED');
      const acquireCode = async () => {
        if (typeof client.getAuthorizationCode === 'function') return invoke('getAuthorizationCode', data, options);
        if (typeof client.classifyNextStep === 'function' && typeof client.obtainAuthorizationCode === 'function') {
          const classification = await invoke('classifyNextStep', data);
          return invoke('obtainAuthorizationCode', classification, options);
        }
        throw flowError('AUTHORIZATION_CLIENT_REQUIRED');
      };
      // The post-credential authorization hop is the one that intermittently fails upstream; retry it
      // on the same live session before giving up, so a transient rejection stops being a dead end.
      const maxAuthRetries = Number.isInteger(authRetries) && authRetries > 0 ? Math.min(authRetries, 10) : 0;
      let result;
      for (let attempt = 0; ; attempt++) {
        try { result = await acquireCode(); break; }
        catch (error) {
          throwIfAborted(signal);
          if (attempt >= maxAuthRetries || !retryableAuthStep(error)) throw error;
          const backoff = Array.isArray(authRetryBackoffMs) ? authRetryBackoffMs[Math.min(attempt, authRetryBackoffMs.length - 1)] : 0;
          emit('authorization', 'running', 'AUTHORIZATION_RETRY');
          if (backoff > 0) await abortableDelay(backoff, signal);
          throwIfAborted(signal);
        }
      }
      const code = typeof result === 'string' ? result : result?.code;
      if (typeof code !== 'string' || !code.trim()) throw flowError('AUTHORIZATION_CODE_REQUIRED');
      const workspaceId = typeof result?.workspaceId === 'string' ? result.workspaceId
        : typeof result?.workspace_id === 'string' ? result.workspace_id : '';
      const state = typeof result?.state === 'string' ? result.state : undefined;
      if (targetWorkspaceId && workspaceId && workspaceId !== targetWorkspaceId) throw flowError('WORKSPACE_MISMATCH');
      if (expectedState && state !== undefined && state !== expectedState) throw flowError('OAUTH_STATE_MISMATCH');
      if (targetWorkspaceId && !workspaceId) throw flowError('WORKSPACE_UNVERIFIED');
      if (expectedState && state === undefined) throw flowError('OAUTH_STATE_UNVERIFIED');
      throwIfAborted(signal);
      emit('authorization', 'completed', 'AUTHORIZATION_READY');
      throwIfAborted(signal);
      return { code, pageTypes, workspaceId };
    }
    throw flowError('LOGIN_HOP_LIMIT');
  } catch (error) {
    const publicError = signal?.aborted ? flowError('CANCELLED') : sanitizeLoginError(error, privateValues);
    emit(currentPhase, publicError.code === 'CANCELLED' ? 'cancelled' : 'failed', publicError.code);
    throw publicError;
  }
}
