import test from 'node:test';
import assert from 'node:assert/strict';
import { createLoginEngine, ADDIN_AUTHORIZATION } from '../upstream/server/login-engine.js';

const record = { email: 'member@example.test', password: 'local-password' };
const item = (id, kind = 'business', extra = {}) => ({ id, name: id, kind, plan: kind === 'business' ? 'self_serve_business_prolite' : 'free', active: kind === 'business', expiresAt: null, deactivated: false, viewOnly: false, ...extra });
const personal = item('personal', 'personal');
const business = item('business');
const dto = (...workspaces) => ({ workspaces, order: workspaces.map(value => value.id), complete: true });
function fixture({ initial = 'personal', inspection = dto(personal, business), lookup, wrongTarget = false, wrongPlan = false, selectFailure = false } = {}) {
  const calls = [], closed = new Set();
  let leases = 0, issued = 0;
  const engine = createLoginEngine({
    waitImpl: async () => {},
    createClient: async context => {
      assert.equal(leases, 0, 'lookup and login must release capacity before the next operation');
      leases++;
      const number = ++issued, id = context.targetWorkspaceId || initial;
      calls.push(['login', context.targetWorkspaceId, context.workspacePreference]);
      return {
        bootstrapAuthorize: async () => {},
        submitEmail: async () => ({ page: { type: 'login_password' } }),
        submitPassword: async () => ({ page: { type: 'consent' } }),
        getAuthorizationCode: async () => {
          if (selectFailure && context.targetWorkspaceId) throw { code: 'WORKSPACE_NOT_FOUND' };
          return { code: `local-code-${id}`, state: context.expectedState, workspaceId: id };
        },
        close: async () => { assert.ok(!closed.has(number)); closed.add(number); leases--; calls.push(['close', id]); },
      };
    },
    exchangeCode: async ({ code }) => {
      let id = code.slice('local-code-'.length);
      if (wrongTarget && id === 'business') id = 'personal';
      const payload = { client_id: ADDIN_AUTHORIZATION.clientId, email: record.email, exp: Math.floor(Date.now() / 1000) + 3600, 'https://api.openai.com/auth': { chatgpt_account_id: id, chatgpt_user_id: 'member', chatgpt_plan_type: id === 'business' && !wrongPlan ? 'self_serve_business_prolite' : 'free' } };
      return { access_token: `local.${Buffer.from(JSON.stringify(payload)).toString('base64url')}.local`, refresh_token: 'invalid-local-refresh', expires_in: 3600 };
    },
    lookupWorkspaces: async (token, options) => {
      assert.equal(leases, 0, 'OAuth lease must be closed even with capacity one');
      calls.push(['lookup']);
      return lookup ? lookup(token, options, calls.filter(value => value[0] === 'lookup').length) : inspection;
    },
  });
  return { engine, calls, closed };
}

test('default discovers Business from complete lookup when login session initially selected Personal', async () => {
  const { engine, calls, closed } = fixture();
  const result = await engine.login(record);
  assert.equal(result.credential.account_id, 'business');
  assert.equal(result.credential.plan_type, 'self_serve_business_prolite');
  assert.deepEqual(result.workspaceSelection, { policy: 'team_first', kind: 'business', id: 'business', name: 'business', reason: 'active_business', reselected: true });
  assert.deepEqual(calls.map(value => value[0]), ['login', 'close', 'lookup', 'login', 'close', 'lookup']);
  assert.deepEqual(calls.filter(value => value[0] === 'login').map(value => value[1]), ['', 'business']);
  assert.equal(closed.size, 2);
});

test('current valid Business is retained with one login even when another precedes it', async () => {
  const { engine, calls } = fixture({ initial: 'business', inspection: dto(item('other-business'), business) });
  const result = await engine.login(record);
  assert.equal(result.workspaceSelection.reselected, false);
  assert.equal(calls.filter(value => value[0] === 'login').length, 1);
});

test('Personal-only and expired or disabled Business membership fail after bounded inspection retries', async () => {
  for (const inspection of [dto(personal), dto(personal, item('business', 'business', { expiresAt: '2020-01-01T00:00:00Z' })), dto(personal, item('business', 'business', { deactivated: true })), dto(personal, item('business', 'business', { active: false }))]) {
    const { engine, calls } = fixture({ inspection });
    await assert.rejects(engine.login(record), error => error.code === 'BUSINESS_WORKSPACE_NOT_FOUND' && error.workspaceDiagnostics.reason === 'no_active_business' && error.workspaceDiagnostics.lookupAttempt === 3);
    assert.equal(calls.filter(value => value[0] === 'lookup').length, 3);
    assert.equal(calls.filter(value => value[0] === 'login').length, 1);
  }
});

test('malformed or unavailable complete lookup never returns the provisional Personal token', async () => {
  for (const lookup of [async () => ({}), async () => { throw { code: 'UPSTREAM_UNAVAILABLE', retryable: true, message: 'private upstream details' }; }]) {
    const { engine } = fixture({ lookup });
    await assert.rejects(engine.login(record), error => ['INVALID_LOGIN_RESPONSE', 'UPSTREAM_UNAVAILABLE'].includes(error.code) && !error.message.includes('private upstream'));
  }
});

test('selection failure, mismatched token and non-Business plan stop before import', async () => {
  for (const [extra, code] of [[{ selectFailure: true }, 'WORKSPACE_NOT_FOUND'], [{ wrongTarget: true }, 'TOKEN_WORKSPACE_MISMATCH'], [{ wrongPlan: true }, 'BUSINESS_WORKSPACE_UNVERIFIED']]) {
    const { engine, closed } = fixture(extra);
    await assert.rejects(engine.login(record), { code });
    assert.equal(closed.size, 2);
  }
});

test('subscription is checked again after reauthorization and losing eligibility aborts', async () => {
  const { engine } = fixture({ lookup: async (_token, _options, number) => dto(personal, item('business', 'business', { active: number === 1 })) });
  await assert.rejects(engine.login(record), { code: 'BUSINESS_WORKSPACE_NOT_FOUND' });
});

test('inactive Business cannot be mislabeled as a Personal fallback by a Free JWT claim', async () => {
  const { engine } = fixture({ initial: 'business', wrongPlan: true, inspection: dto(personal, item('business', 'business', { active: false })) });
  await assert.rejects(engine.login(record), { code: 'BUSINESS_WORKSPACE_NOT_FOUND' });
});

test('explicit workspace and explicit Personal or first preferences remain authoritative', async () => {
  for (const extra of [{ workspaceId: 'chosen' }, { workspacePreference: 'personal' }, { workspacePreference: 'first' }]) {
    const { engine, calls } = fixture({ lookup: async () => assert.fail('explicit policy must not trigger automatic reselection') });
    const result = await engine.login({ ...record, ...extra });
    assert.equal(result.workspaceSelection, undefined);
    assert.equal(result.credential.account_id, extra.workspaceId || 'personal');
    assert.equal(calls.filter(value => value[0] === 'login').length, 1);
  }
});

test('cancellation during lookup closes OAuth and never returns a credential', async () => {
  const outer = new AbortController();
  const { engine, closed } = fixture({ lookup: async (_token, { signal }) => {
    queueMicrotask(() => outer.abort());
    return new Promise((resolve, reject) => signal.addEventListener('abort', () => reject({ code: 'CANCELLED' }), { once: true }));
  } });
  await assert.rejects(engine.login(record, { signal: outer.signal }), { code: 'CANCELLED' });
  assert.equal(closed.size, 1);
});

test('default rule cannot silently run without a workspace inspector', async () => {
  const engine = createLoginEngine({ createClient: async () => assert.fail('no login expected'), exchangeCode: async () => {} });
  await assert.rejects(engine.login(record), error => error.code === 'WORKSPACE_LOOKUP_REQUIRED' && error.workspaceDiagnostics.reason === 'lookup_failed');
});
