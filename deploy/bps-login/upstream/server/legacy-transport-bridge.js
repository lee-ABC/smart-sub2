import { LoginFlowError } from './login-state-machine.js';
import { validateSdkResult } from './sdk-diagnostics.js';

const fail = (code, message, options) => new LoginFlowError(code, message, options);
const staticCode = value => typeof value === 'string' && /^[A-Za-z][A-Za-z0-9_-]{0,79}$/.test(value) ? value : undefined;

function workspaceKind(workspace) {
  if (!workspace || typeof workspace !== 'object' || Array.isArray(workspace)) return null;
  const kinds = new Set();
  for (const field of ['kind', 'structure', 'type', 'workspace_type', 'account_type']) {
    if (typeof workspace[field] !== 'string') continue;
    const value = workspace[field].trim().toLowerCase();
    if (['personal', 'individual'].includes(value)) kinds.add('personal');
    // OpenAI marks a team/business space's structure as 'workspace' and a personal one as 'personal'.
    if (['team', 'business', 'enterprise', 'organization', 'org', 'workspace'].includes(value)) kinds.add('team');
  }
  for (const field of ['plan_type', 'plan', 'subscription_plan', 'subscription']) {
    if (typeof workspace[field] === 'string' && /business|enterprise|\bteam\b|\bedu\b/.test(workspace[field].trim().toLowerCase())) kinds.add('team');
  }
  if (workspace.is_personal !== undefined && workspace.is_personal !== null) {
    if (typeof workspace.is_personal !== 'boolean') return null;
    kinds.add(workspace.is_personal ? 'personal' : 'team');
  }
  return kinds.size === 1 ? [...kinds][0] : null;
}

const validWorkspaceId = value => typeof value === 'string' && value.length <= 256 && Boolean(value.trim()) && !/[\x00-\x1f\x7f]/.test(value);

/** Candidate records contain only fields needed for a same-session user choice. */
export function getWorkspaceCandidates(workspaces) {
  const seen = new Set();
  const result = [];
  for (const workspace of Array.isArray(workspaces) ? workspaces : []) {
    if (!validWorkspaceId(workspace?.id) || seen.has(workspace.id)) continue;
    seen.add(workspace.id);
    const rawName = typeof workspace.name === 'string' ? workspace.name.replace(/[\x00-\x1f\x7f]/g, '').trim() : '';
    const name = Array.from(rawName || '未命名工作空间').slice(0, 80).join('');
    result.push({ id: workspace.id, name, kind: workspaceKind(workspace) || 'unknown' });
  }
  return result;
}

function choiceRequired(list, message) {
  const error = fail('WORKSPACE_SELECTION_REQUIRED', message);
  error.workspaceCandidates = getWorkspaceCandidates(list);
  return error;
}

/** Explicit IDs win; default/first follows server order without inferring a type. */
export function selectWorkspace(workspaces, { targetWorkspaceId = '', selectedWorkspaceId = '', workspacePreference = '' } = {}) {
  const list = Array.isArray(workspaces) ? workspaces : [];
  const explicitId = targetWorkspaceId || selectedWorkspaceId;
  if (explicitId) {
    const selected = validWorkspaceId(explicitId) ? list.find(item => item?.id === explicitId) : undefined;
    if (!selected) throw fail('WORKSPACE_NOT_FOUND', '登录会话中没有选择的工作空间');
    return selected;
  }
  // This provisional authorization enables the complete subscription check in login-engine.
  // Session structure alone does not establish an active Business subscription.
  if (workspacePreference === 'team_first') {
    const teams = list.filter(workspace => validWorkspaceId(workspace?.id) && workspaceKind(workspace) === 'team');
    if (teams.length === 1) return teams[0];
    if (teams.length > 1) return [...teams].sort((left, right) => String(left.id).localeCompare(String(right.id)))[0];
    return list[0];
  }
  if (!workspacePreference || workspacePreference === 'first') return list[0];
  if (workspacePreference) {
    if (!['team', 'personal'].includes(workspacePreference)) throw choiceRequired(list, '请明确选择工作空间后重试');
    const candidates = list.map(workspace => ({ workspace, kind: workspaceKind(workspace) }));
    if (candidates.some(candidate => !candidate.kind || !validWorkspaceId(candidate.workspace.id))) throw choiceRequired(list, '工作空间类型无法确定，请明确选择后重试');
    const matching = candidates.filter(candidate => candidate.kind === workspacePreference);
    if (matching.length > 1) throw choiceRequired(list, '有多个符合偏好的工作空间，请明确选择后重试');
    if (!matching.length) throw fail('WORKSPACE_NOT_FOUND', '登录会话中没有符合偏好的工作空间');
    return matching[0].workspace;
  }
  return list[0];
}

/** Reuses an injected transport; never changes SDK solving or fingerprint behavior. */
export function createLegacyTransportBridge(nativeClient, { authorization, signal, onSdkDiagnostic = () => {}, workspacePreference = '' } = {}) {
  if (!nativeClient || typeof nativeClient._postWithSentinel !== 'function' || typeof nativeClient._getRaw !== 'function') throw fail('LOGIN_TRANSPORT_INCOMPLETE', '旧登录传输接口不完整');
  const previousAbort = nativeClient.abortCheck;
  nativeClient.abortCheck = () => Boolean(signal?.aborted || typeof previousAbort === 'function' && previousAbort.call(nativeClient));
  nativeClient.log = () => {};
  const checkAbort = () => { if (signal?.aborted) throw fail('CANCELLED', '登录已取消'); };
  if (typeof nativeClient._sentinel === 'function') {
    const original = nativeClient._sentinel.bind(nativeClient);
    nativeClient._sentinel = async flow => {
      checkAbort();
      const result = await original(flow);
      const diagnostic = validateSdkResult(result, { expectedFlow: flow });
      try { onSdkDiagnostic(diagnostic); } catch {}
      if (diagnostic.status === 'blocked') throw fail(diagnostic.code, diagnostic.reason, { causeCode: diagnostic.safeSummary.exceptionCause || undefined });
      return result;
    };
  } else throw fail('SDK_DIAGNOSTICS_REQUIRED', '旧传输缺少可检查的验证组件接口');

  const accept = (response, stage) => {
    checkAbort();
    const status = Number(response?.status);
    if (status !== 200 && status !== 204) {
      const causeCode = staticCode(response?.data?.error?.code);
      const code = status >= 500 ? 'UPSTREAM_UNAVAILABLE' : status === 429 ? 'LOGIN_RATE_LIMITED' : 'LOGIN_REQUEST_REJECTED';
      throw fail(code, status >= 500 ? '登录服务暂时不可用，请稍后重试' : status === 429 ? '登录请求受到限制，请稍后再试' : `${stage}未通过，请检查错误类别`, { causeCode, httpStatus: status, retryable: status >= 500 });
    }
    const data = response.data;
    if (!data || typeof data !== 'object' || !data.page) throw fail('INVALID_LOGIN_RESPONSE', '登录服务没有返回有效的下一步');
    nativeClient.lastOtpResponse = data;
    if (data['oai-client-auth-session'] && typeof data['oai-client-auth-session'] === 'object') nativeClient.lastSession = data['oai-client-auth-session'];
    return data;
  };

  async function followCallback(startUrl, { expectedState, workspaceId = '' } = {}) {
    const redirect = new URL(authorization.redirectUri);
    const authOrigin = new URL(authorization.authorizationUrl).origin;
    let current = new URL(startUrl, authOrigin);
    const visited = new Set();
    for (let hop = 0; hop < 12; hop++) {
      checkAbort();
      if (visited.has(current.href)) throw fail('AUTHORIZATION_REDIRECT_LOOP', '授权跳转没有推进，已停止重复请求');
      visited.add(current.href);
      if (current.origin === redirect.origin && current.pathname === redirect.pathname) {
        if (!expectedState || current.searchParams.get('state') !== expectedState) throw fail('OAUTH_STATE_MISMATCH', '授权回调校验失败');
        if (current.searchParams.has('error')) throw fail('AUTHORIZATION_REJECTED', '授权未完成或已被拒绝', { causeCode: staticCode(current.searchParams.get('error')) });
        const code = current.searchParams.get('code');
        if (!code) throw fail('AUTHORIZATION_CODE_REQUIRED', '授权回调缺少授权码');
        return { code, state: expectedState, workspaceId };
      }
      const allowed = new Set([authOrigin, ...(authorization.allowedRedirectOrigins || [])]);
      if (current.protocol !== 'https:' || current.username || current.password || !allowed.has(current.origin)) throw fail('AUTHORIZATION_REDIRECT_NOT_ALLOWED', '授权跳转到了未允许的地址，已停止');
      const response = await nativeClient._getRaw(current.toString());
      if (response.status >= 300 && response.status < 400 && response.location) { current = new URL(response.location, current); continue; }
      if (response.status === 200) {
        let data;
        try { data = response.data || JSON.parse(response.text); } catch {}
        const next = data?.page?.payload?.url || data?.continue_url;
        if (typeof next === 'string' && next) { current = new URL(next, current); continue; }
      }
      throw fail('AUTHORIZATION_REDIRECT_FAILED', '授权跳转没有返回下一步，已保留当前失败原因', { httpStatus: Number(response.status), retryable: Number(response.status) >= 500 });
    }
    throw fail('AUTHORIZATION_REDIRECT_LIMIT', '授权跳转次数超过上限');
  }

  return {
    async bootstrapAuthorize(url) { checkAbort(); await nativeClient.bootstrapAuthorize(url); checkAbort(); },
    async submitEmail(email) {
      return accept(await nativeClient._postWithSentinel('/api/accounts/authorize/continue', { username: { kind: 'email', value: email } }, 'authorize_continue', `${new URL(authorization.authorizationUrl).origin}/log-in`), '邮箱验证');
    },
    async submitPassword(password) {
      return accept(await nativeClient._postWithSentinel('/api/accounts/password/verify', { password }, 'password_verify', `${new URL(authorization.authorizationUrl).origin}/log-in/password`), '密码验证');
    },
    async issueMfaChallenge(factorId) { checkAbort(); return nativeClient.issueMfaChallenge(factorId); },
    async verifyMfaTotp(factorId, code) { checkAbort(); const data = await nativeClient.verifyMfaTotp(factorId, code); checkAbort(); return data; },
    async submitEmailOtp(code) { checkAbort(); const data = await nativeClient.submitEmailOtp(code); checkAbort(); return data; },
    async getAuthorizationCode(data, { targetWorkspaceId, selectedWorkspaceId, expectedState } = {}) {
      checkAbort();
      const list = Array.isArray(nativeClient.lastSession?.workspaces) ? nativeClient.lastSession.workspaces : [];
      const selected = selectWorkspace(list, { targetWorkspaceId, selectedWorkspaceId, workspacePreference });
      if (selected?.id) {
        const response = await nativeClient._post('/api/accounts/workspace/select', { workspace_id: selected.id });
        const after = accept(response, '工作空间授权');
        const next = after.page?.payload?.url || after.continue_url;
        if (!next) throw fail('AUTHORIZATION_URL_REQUIRED', '工作空间授权没有返回继续地址');
        return followCallback(next, { expectedState, workspaceId: selected.id });
      }
      const next = data?.page?.payload?.url || data?.continue_url;
      if (!next) throw fail('WORKSPACE_UNVERIFIED', '授权尚未提供可确认的工作空间或回调地址');
      return followCallback(next, { expectedState });
    },
    async close() { nativeClient.log = () => {}; if (typeof nativeClient.close === 'function') await nativeClient.close(); },
  };
}
