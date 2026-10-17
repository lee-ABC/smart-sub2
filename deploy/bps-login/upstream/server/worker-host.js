import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { createHash, randomBytes, timingSafeEqual } from 'node:crypto';
import { createRequire, Module } from 'node:module';
import { spawn } from 'node:child_process';
import { AsyncLocalStorage } from 'node:async_hooks';
import { fileURLToPath } from 'node:url';
import { readWorkerKey } from './worker-client.js';
import { ADDIN_AUTHORIZATION } from './login-engine.js';
import { createLegacyTransportBridge, getWorkspaceCandidates } from './legacy-transport-bridge.js';
import { LoginFlowError, sanitizeLoginError } from './login-state-machine.js';
import { createNativeWorkspaceInspector, sanitizeWorkspaceInspection, validateWorkspaceAccessToken } from './workspace-inspector.js';
import { createNativeBusinessUsageVerifier, sanitizeBusinessUsageVerification, validateBusinessUsageBinding } from './business-usage-verifier.js';

const WORKER_FILE = fileURLToPath(import.meta.url);
const METHODS = new Set(['bootstrapAuthorize', 'submitEmail', 'submitPassword', 'issueMfaChallenge', 'verifyMfaTotp', 'submitEmailOtp', 'getAuthorizationCode', 'close']);
const REVIEWED = {
  'src/sentinelClient.js': 'ef69236f447ddbcce92b64f61e6f6ecb7a8c65493444cd786fd4d8e9e3aac83d',
  'sentinel/sentinel-runner.js': '9a522b55eac0737475e770bb640a91657a7b889d4ee8a3b82c750914cd99ce56',
  'src/rtPureProtocolClient.js': 'b0c540acae489a420d7cd3f7b6faeefc373137346158ad57cf70e12f05ac6f08',
};
const fail = (code = 'LOGIN_REQUEST_REJECTED', options = {}) => new LoginFlowError(code, undefined, options);
const silentClose = async client => { try { await client?.close?.(); } catch {} };
const hash = value => createHash('sha256').update(value).digest('hex');
const blocked = reason => Object.assign(fail('LOGIN_TRANSPORT_REQUIRED'), { workerReason: reason });

export function sdkModeAdmission({ sdkContextMode = 'legacy', approvedSdkSha256, actualSdkSha256, workerKeyConfigured = false, environment = process.env } = {}) {
  if (!['legacy', 'paired'].includes(sdkContextMode)) return 'sdk_context_mode_invalid';
  if (sdkContextMode === 'legacy') return null;
  if (environment.CPA_ISOLATED_DIAGNOSTIC === '1' && !environment.CPA_WORKER_KEY_FILE && !workerKeyConfigured) return null;
  if (typeof approvedSdkSha256 !== 'string' || !/^[a-f0-9]{64}$/.test(approvedSdkSha256)) return 'sdk_approval_required';
  return actualSdkSha256 === approvedSdkSha256 ? null : 'sdk_version_unapproved';
}

function sdkSnapshot(filename, expectedSdkSha256) {
  try {
    if (typeof filename !== 'string' || !path.isAbsolute(filename) || !filename.endsWith('.js')) throw new Error();
    const size = fs.statSync(filename).size;
    if (size < 1 || size > 2097152) throw new Error();
    const bytes = fs.readFileSync(filename);
    if (bytes.length > 2097152 || hash(bytes) !== expectedSdkSha256) throw new Error();
    const text = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes);
    return { path: path.resolve(filename), sha256: expectedSdkSha256, text };
  } catch { throw fail('SDK_VERSION_DRIFT'); }
}

function compilePrivateCjs(filename, source, replacements = {}) {
  const module = new Module(filename);
  module.filename = filename;
  module.paths = Module._nodeModulePaths(path.dirname(filename));
  module.require = specifier => Object.hasOwn(replacements, specifier)
    ? replacements[specifier] : Module.prototype.require.call(module, specifier);
  module._compile(source, filename);
  module.loaded = true;
  return module;
}

/** Keeps secrets off argv and reuses the unchanged SDK. Paired mode also
 * changes context acquisition timing and enables the original observer envelope. */
export function createPrivateRunnerTransport({ legacyRoot, storage, sdkContextMode = 'legacy', expectedSdkSha256, spawnImpl = spawn, workerFile = WORKER_FILE } = {}) {
  if (!['legacy', 'paired'].includes(sdkContextMode)) throw fail('LOGIN_TRANSPORT_REQUIRED');
  const runnerPath = path.resolve(legacyRoot, 'sentinel/sentinel-runner.js');
  const pending = new Map();
  const challengePath = value => typeof value === 'string' && path.dirname(path.resolve(value)) === path.resolve(os.tmpdir()) && /^sentinel-challenge-[A-Za-z0-9_-]+\.json$/.test(path.basename(value));
  const privateFs = {
    ...fs,
    writeFileSync(filename, value) {
      const owner = storage.getStore();
      if (!owner || owner.controller.signal.aborted || !challengePath(filename) || typeof value !== 'string' || Buffer.byteLength(value) > 1048576) throw fail();
      pending.set(path.resolve(filename), { value, owner });
    },
    unlinkSync(filename) {
      if (!challengePath(filename)) throw fail();
      pending.delete(path.resolve(filename));
    },
  };
  const privateSpawn = (_executable, args, options = {}) => {
    const owner = storage.getStore();
    if (!owner || owner.controller.signal.aborted || !Array.isArray(args) || args.some(value => typeof value !== 'string') || args[0] !== runnerPath) throw fail('LOGIN_TRANSPORT_REQUIRED');
    const paired = sdkContextMode === 'paired';
    const flags = args.filter(value => value === '--ipc' || value === '--emit-so');
    if (paired ? flags.join('|') !== '--ipc|--emit-so' || args.includes('--challenge-file') || args.includes('--challenge-url') : flags.length !== 0) throw fail('LOGIN_TRANSPORT_REQUIRED');
    const index = args.indexOf('--challenge-file');
    const filename = index >= 0 ? path.resolve(args[index + 1] || '') : '';
    const challenge = pending.get(filename);
    if (!paired && (!challenge || challenge.owner !== owner)) throw fail('LOGIN_TRANSPORT_REQUIRED');
    const sdkIndex = args.indexOf('--sdk');
    const sdkAsset = expectedSdkSha256 ? sdkSnapshot(sdkIndex >= 0 ? args[sdkIndex + 1] : undefined, expectedSdkSha256) : undefined;
    const envelope = { mode: sdkContextMode, args: args.slice(1), ...(sdkAsset ? { sdkAsset } : {}), ...(!paired ? { challengePath: filename, challengeText: challenge.value } : {}) };
    const encoded = JSON.stringify(envelope);
    if (Buffer.byteLength(encoded) > 2097152) throw fail('SDK_VERSION_DRIFT');
    const environment = { SENTINEL_CONFIG: '__none__' };
    for (const name of ['PATH', 'LANG', 'LC_ALL', 'TZ', 'LD_LIBRARY_PATH', 'NODE_ENV']) {
      if (typeof options.env?.[name] === 'string') environment[name] = options.env[name];
    }
    // The kernel argument list contains only paths and a fixed mode flag.
    const child = spawnImpl(process.execPath, [workerFile, '--sdk-stdin-runner', runnerPath, ...(paired ? ['--ipc', '--emit-so'] : [])], {
      cwd: legacyRoot, env: environment, stdio: ['pipe', 'pipe', 'pipe', ...(paired ? ['ipc'] : [])], windowsHide: true,
    });
    const abort = () => { try { child.kill('SIGKILL'); } catch {} };
    if (paired) {
      const flow = args[args.indexOf('--flow') + 1];
      const originalOn = child.on;
      child.on = function (event, listener) {
        if (event !== 'message') return originalOn.call(this, event, listener);
        return originalOn.call(this, event, function (message, ...rest) {
          if (owner.controller.signal.aborted || message?.type !== 'need-challenge' || message.flow !== flow || typeof message.p !== 'string' || !message.p || Buffer.byteLength(message.p) > 262144) { abort(); return; }
          return listener.call(this, message, ...rest);
        });
      };
    }
    owner.children.add(child);
    owner.controller.signal.addEventListener('abort', abort, { once: true });
    const release = () => {
      owner.children.delete(child);
      owner.controller.signal.removeEventListener('abort', abort);
    };
    child.once('close', release);
    child.once('error', () => { abort(); release(); });
    child.stdin.on('error', abort);
    if (owner.controller.signal.aborted) { abort(); throw fail('CANCELLED'); }
    child.stdin.end(encoded);
    return child;
  };
  return { fs: privateFs, spawn: privateSpawn, pending };
}

export function scopedSdkExports(original, sdkContextMode = 'legacy') {
  if (!['legacy', 'paired'].includes(sdkContextMode) || typeof original?.SentinelClient !== 'function') throw fail('LOGIN_TRANSPORT_REQUIRED');
  if (sdkContextMode === 'legacy') return original;
  const BaseClient = original.SentinelClient;
  return { ...original, SentinelClient: class ScopedSentinelClient extends BaseClient {
    constructor(options) { super({ ...options, ipcHandshake: true }); }
    _dwellMsFor(flow) {
      if (process.env.BPS_LOGIN_DWELL_PROFILE !== 'balanced') return super._dwellMsFor(flow);
      const [lo, hi] = /create_account/.test(String(flow || '')) ? [13250, 16000] : [5000, 9000];
      return Math.round(lo + Math.random() * (hi - lo));
    }
  } };
}

export async function loadLegacyWorkerFactory(legacyRoot, storage, { sdkContextMode = 'legacy', approvedSdkSha256, workerKeyConfigured = false } = {}) {
  if (Number(process.versions.node.split('.')[0]) !== 22) throw blocked('node_version');
  if (!['legacy', 'paired'].includes(sdkContextMode)) throw blocked('sdk_context_mode_invalid');
  const admission = sdkModeAdmission({ sdkContextMode, approvedSdkSha256, workerKeyConfigured });
  // A valid configured hash is only preliminary approval. Its actual asset
  // comparison must happen below, before TLS initialization or factory creation.
  if (admission && admission !== 'sdk_version_unapproved') throw blocked(admission);
  const root = path.resolve(legacyRoot);
  const sources = {};
  for (const [relative, expected] of Object.entries(REVIEWED)) {
    const filename = path.join(root, relative);
    const source = fs.readFileSync(filename, 'utf8');
    if (hash(source) !== expected) throw blocked('legacy_source_changed');
    sources[relative] = source;
  }
  if (process.env.SENTINEL_IPC_HANDSHAKE === '1') throw blocked('legacy_ipc_unsupported');
  const requireLegacy = createRequire(path.join(root, 'package.json'));
  const sdk = requireLegacy('./src/sentinelSdkUpdater.js').activeSdk();
  let expectedSdkSha256;
  try {
    const bytes = fs.readFileSync(sdk.path);
    if (!fs.statSync(sdk.path).isFile() || bytes.length < 1 || bytes.length > 2097152) throw new Error();
    expectedSdkSha256 = hash(bytes);
  } catch { throw blocked('sdk_asset_missing'); }
  const versionAdmission = sdkModeAdmission({ sdkContextMode, approvedSdkSha256, workerKeyConfigured, actualSdkSha256: expectedSdkSha256 });
  if (versionAdmission) throw blocked(versionAdmission);
  const legacyTransport = createPrivateRunnerTransport({ legacyRoot: root, storage, expectedSdkSha256 });
  const sentinelPath = path.join(root, 'src/sentinelClient.js');
  const childProcess = requireLegacy('node:child_process');
  const legacySentinelModule = compilePrivateCjs(sentinelPath, sources['src/sentinelClient.js'], {
    fs: legacyTransport.fs, child_process: { ...childProcess, spawn: legacyTransport.spawn },
  });
  // Global dependency lookups always retain the default legacy constructor.
  requireLegacy.cache[sentinelPath] = legacySentinelModule;
  const transport = sdkContextMode === 'paired' ? createPrivateRunnerTransport({ legacyRoot: root, storage, sdkContextMode, expectedSdkSha256 }) : legacyTransport;
  const sentinelModule = sdkContextMode === 'paired' ? compilePrivateCjs(sentinelPath, sources['src/sentinelClient.js'], {
    fs: transport.fs, child_process: { ...childProcess, spawn: transport.spawn },
  }) : legacySentinelModule;
  const tlsBootstrap = requireLegacy('./src/tlsClientBootstrap.js');
  await validateTlsWorkerRuntime(tlsBootstrap, requireLegacy);
  const { RtPureProtocolClient } = compilePrivateCjs(path.join(root, 'src/rtPureProtocolClient.js'), sources['src/rtPureProtocolClient.js'], {
    './sentinelClient': scopedSdkExports(sentinelModule.exports, sdkContextMode),
  }).exports;
  if (typeof RtPureProtocolClient !== 'function') throw fail('LOGIN_TRANSPORT_REQUIRED');
  if (typeof requireLegacy('./sentinel/sentinel-runner.js').main !== 'function') throw blocked('runner_interface_missing');
  const factory = context => new RtPureProtocolClient({
    proxyUrl: context.proxyUrl || null, redirectUri: context.authorization.redirectUri,
    accountPassword: context.password, totpSecret: context.totpSecret,
    country: 'US', log: () => {}, abortCheck: () => context.signal.aborted,
    targetWorkspaceId: context.targetWorkspaceId,
  });
  Object.defineProperty(factory, 'sdkContextMode', { value: sdkContextMode });
  Object.defineProperty(factory, 'sdkSha256', { value: expectedSdkSha256 });
  Object.defineProperty(factory, 'workspaceLookup', { value: createNativeWorkspaceInspector({
    tls: tlsBootstrap.requireTlsClient(),
    rtClientIdentifier: requireLegacy('./src/tlsProfile.js').rtClientIdentifier,
    headerOrderFor: requireLegacy('./src/chromeHeaderOrder.js').headerOrderFor,
  }) });
  Object.defineProperty(factory, 'businessUsageLookup', { value: createNativeBusinessUsageVerifier({
    tls: tlsBootstrap.requireTlsClient(),
    rtClientIdentifier: requireLegacy('./src/tlsProfile.js').rtClientIdentifier,
    headerOrderFor: requireLegacy('./src/chromeHeaderOrder.js').headerOrderFor,
  }) });
  return factory;
}

// Vendor initTLS only checks the file and starts an asynchronous pool. Probe
// dlopen first, then await a local native operation before advertising readiness.
// Neither operation sends a network request or contains account credentials.
export async function validateTlsWorkerRuntime(tlsBootstrap, requireLegacy) {
  if (tlsBootstrap.ensureTlsLibrary().ok !== true) throw blocked('tls_library_missing');
  try {
    const { LibraryHandler } = requireLegacy('./node_modules/node-tls-client/dist/utils/native.js');
    LibraryHandler.retrieveLibrary();
  } catch { throw blocked('tls_library_unloadable'); }
  try {
    const tls = tlsBootstrap.requireTlsClient();
    await tls.initTLS();
    const { Client } = requireLegacy('./node_modules/node-tls-client/dist/lib/Client.js');
    await Client.getInstance().pool.run({}, { name: 'destroyAll' });
  } catch { throw blocked('tls_worker_initialization_failed'); }
}

function inputContext(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw fail();
  const limits = { email: 320, password: 1000, totpSecret: 512, targetWorkspaceId: 256, expectedState: 256 };
  const result = {};
  for (const [key, max] of Object.entries(limits)) {
    const candidate = value[key] ?? '';
    if (typeof candidate !== 'string' || candidate.length > max) throw fail();
    result[key] = candidate;
  }
  result.email = result.email.trim();
  if (!/^[^\s@]+@[^\s@]+$/.test(result.email) || !result.expectedState) throw fail();
  if (value.workspacePreference !== undefined && value.workspacePreference !== '' && !['first', 'team', 'personal', 'team_first'].includes(value.workspacePreference)) throw fail();
  result.workspacePreference = value.workspacePreference || 'team_first';
  return result;
}

function safeStep(data) {
  const known = new Set(['login_password', 'password', 'password_verification', 'mfa', 'mfa_challenge', 'totp', 'totp_verification', 'two_factor', 'two_factor_verification', 'two-factor', 'email_otp', 'email_otp_verification', 'email_verification', 'otp_verification', 'consent', 'codex', 'workspace', 'workspace_selection', 'workspace_select', 'select_workspace', 'oauth', 'oauth_consent', 'external_url', 'authorization_code', 'oauth_callback', 'add_phone', 'phone_verification', 'phone_otp_verification', 'reset_password_new_password', 'password_reset_required']);
  const rawType = typeof data?.page?.type === 'string' ? data.page.type.trim().toLowerCase() : '';
  const payload = data?.page?.payload || {};
  const factors = Array.isArray(payload.factors) ? payload.factors.slice(0, 10).filter(factor => typeof factor?.id === 'string' && factor.id.length <= 256).map(factor => ({ id: factor.id, factor_type: factor.factor_type === 'totp' ? 'totp' : 'unsupported' })) : [];
  return { page: { type: known.has(rawType) ? rawType : 'unsupported', payload: {
    ...(typeof payload.factor_id === 'string' && payload.factor_id.length <= 256 ? { factor_id: payload.factor_id } : {}),
    ...(factors.length ? { factors } : {}),
    ...(payload.factor_type ? { factor_type: payload.factor_type === 'totp' ? 'totp' : 'unsupported' } : {}),
  } } };
}

function authorizeUrl(value, session) {
  let url;
  try { url = new URL(value); } catch { throw fail(); }
  const base = new URL(session.authorization.authorizationUrl);
  const allowed = new Set(['client_id', 'response_type', 'redirect_uri', 'scope', 'state', 'code_challenge', 'code_challenge_method', 'audience']);
  if (url.origin !== base.origin || url.pathname !== base.pathname || url.username || url.password || url.hash) throw fail();
  for (const key of url.searchParams.keys()) if (!allowed.has(key) || url.searchParams.getAll(key).length !== 1) throw fail();
  for (const [key, expected] of Object.entries({ client_id: session.authorization.clientId, response_type: 'code', redirect_uri: session.authorization.redirectUri, scope: session.authorization.scope, state: session.context.expectedState, code_challenge_method: 'S256' })) {
    if (url.searchParams.get(key) !== expected) throw fail();
  }
  if (!/^[A-Za-z0-9_-]{43}$/.test(url.searchParams.get('code_challenge') || '')) throw fail();
  if (session.authorization.audience && url.searchParams.get('audience') !== session.authorization.audience) throw fail();
  return url.toString();
}

export async function readMemoryJson(stream, maxBytes) {
  const chunks = [];
  let size = 0;
  for await (const chunk of stream) {
    const bytes = typeof chunk === 'string' ? Buffer.from(chunk, 'utf8') : Buffer.from(chunk);
    size += bytes.length;
    if (size > maxBytes) throw fail();
    chunks.push(bytes);
  }
  try { return JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(Buffer.concat(chunks, size))); }
  catch { throw fail(); }
}

async function readBody(req) {
  if (!String(req.headers['content-type'] || '').startsWith('application/json')) throw fail();
  return readMemoryJson(req, 32768);
}

export async function createWorkerHost({ keyFile, enabled = false, legacyRoot = '/app', nativeFactory, workspaceLookup, lookupBusinessUsage, factoryLoader = loadLegacyWorkerFactory, sdkContextMode = 'legacy', approvedSdkSha256, authorization = ADDIN_AUTHORIZATION, maxSessions = 1, ttlMs = 300000, host = '127.0.0.1', port = 19418 } = {}) {
  if (!['127.0.0.1', '::1'].includes(host) || !Number.isSafeInteger(maxSessions) || maxSessions < 1 || !Number.isInteger(ttlMs) || ttlMs < 1 || ttlMs > 300000) throw fail();
  const key = Buffer.from(readWorkerKey(keyFile));
  const storage = new AsyncLocalStorage();
  let ready = false;
  let blockedReason = 'disabled';
  if (enabled) {
    try { nativeFactory ||= await factoryLoader(legacyRoot, storage, { sdkContextMode, approvedSdkSha256, workerKeyConfigured: true }); ready = true; }
    catch (error) { ready = false; blockedReason = ['node_version', 'legacy_source_changed', 'legacy_ipc_unsupported', 'tls_library_missing', 'tls_library_unloadable', 'tls_worker_initialization_failed', 'sdk_asset_missing', 'runner_interface_missing', 'sdk_context_mode_invalid', 'sdk_approval_required', 'sdk_version_unapproved'].includes(error?.workerReason) ? error.workerReason : 'legacy_initialization_failed'; }
  }
  workspaceLookup ||= nativeFactory?.workspaceLookup;
  lookupBusinessUsage ||= nativeFactory?.businessUsageLookup;
  const sessions = new Map();
  const inspections = new Set();
  let stopping = false;
  const dispose = async session => {
    if (session.disposing) return session.disposing;
    session.closed = true;
    session.controller.abort();
    clearTimeout(session.timer);
    session.disposing = Promise.resolve().then(async () => {
      for (const child of session.children) { try { child.kill('SIGKILL'); } catch {} }
      let timer;
      try { await Promise.race([silentClose(session.client), new Promise(resolve => { timer = setTimeout(resolve, 2000); })]); }
      finally {
        clearTimeout(timer);
        // A closing in-flight call still owns its capacity slot until it settles.
        if (!session.busy) { sessions.delete(session.id); session.lastData = null; session.native = null; session.context = null; session.workspaceOfferedIds = null; }
      }
    });
    return session.disposing;
  };
  const send = (res, status, body) => {
    if (res.destroyed || res.writableEnded) return;
    res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' });
    res.end(JSON.stringify(body));
  };
  const authenticate = req => {
    const value = req.headers.authorization;
    if (typeof value !== 'string' || !value.startsWith('Bearer ')) return false;
    const received = Buffer.from(value.slice(7));
    return received.length === key.length && timingSafeEqual(received, key);
  };
  const health = () => ({ ready: ready && !stopping, enabled: enabled === true, mode: ['legacy', 'paired'].includes(sdkContextMode) ? sdkContextMode : 'invalid', transport: ready ? 'legacy-stdin-private' : 'blocked', reason: stopping ? 'stopping' : ready ? 'ready' : blockedReason, version: 1, maxSessions, ttlMs });
  const server = http.createServer(async (req, res) => {
    let session;
    let inspection;
    let workspaceAccessToken;
    const lost = () => {
      if (res.writableEnded) return;
      if (session) void dispose(session);
      inspection?.controller.abort();
    };
    req.once('aborted', lost);
    res.once('close', lost);
    try {
      const pathname = new URL(req.url, 'http://worker.invalid').pathname;
      if (req.method === 'GET' && pathname === '/health') return send(res, 200, health());
      if (!authenticate(req)) return send(res, 401, { error: { code: 'LOGIN_TRANSPORT_REQUIRED', message: 'Worker 请求未获授权', retryable: false } });
      if (req.method === 'POST' && pathname === '/capacity') {
        const body = await readBody(req);
        if (!body || typeof body !== 'object' || Array.isArray(body) || Object.keys(body).length !== 1 || !Number.isSafeInteger(body.maxSessions) || body.maxSessions < 1) throw fail();
        maxSessions = body.maxSessions;
        return send(res, 200, { maxSessions });
      }
      if (req.method === 'POST' && ['/workspaces/check', '/workspaces/verify-business'].includes(pathname)) {
        const businessUsage = pathname === '/workspaces/verify-business';
        if (req.headers.origin !== undefined) return send(res, 403, { error: { code: 'LOGIN_REQUEST_REJECTED', message: 'Worker 请求来源未获授权', retryable: false } });
        if (!ready || stopping || typeof (businessUsage ? lookupBusinessUsage : workspaceLookup) !== 'function') return send(res, 503, { error: { code: 'LOGIN_TRANSPORT_REQUIRED', message: 'Worker 工作空间查询尚未就绪', retryable: false } });
        const body = await readBody(req);
        if (!body || typeof body !== 'object' || Array.isArray(body) || Object.keys(body).length !== (businessUsage ? 2 : 1) || !Object.hasOwn(body, 'accessToken') || businessUsage && !Object.hasOwn(body, 'accountId')) throw fail();
        workspaceAccessToken = validateWorkspaceAccessToken(body.accessToken);
        if (businessUsage) validateBusinessUsageBinding(workspaceAccessToken, body.accountId);
        if (sessions.size + inspections.size >= maxSessions) return send(res, 409, { error: { code: 'UPSTREAM_UNAVAILABLE', message: 'Worker 当前忙碌，请稍后重试', retryable: true } });
        inspection = { controller: new AbortController() };
        inspections.add(inspection);
        const timer = setTimeout(() => inspection.controller.abort(), 35000);
        timer.unref();
        try {
          const result = businessUsage
            ? await lookupBusinessUsage(workspaceAccessToken, { accountId: body.accountId, signal: inspection.controller.signal })
            : await workspaceLookup(workspaceAccessToken, { signal: inspection.controller.signal });
          if (inspection.controller.signal.aborted || stopping) throw fail('CANCELLED');
          // Revalidate aliases/completeness at the RPC boundary; never return the native snapshot.
          return send(res, 200, { result: businessUsage
            ? sanitizeBusinessUsageVerification(result, { accountId: body.accountId, privateValues: [workspaceAccessToken, key.toString()] })
            : sanitizeWorkspaceInspection(result, [workspaceAccessToken, key.toString()]) });
        } finally {
          clearTimeout(timer);
          inspections.delete(inspection);
          body.accessToken = '';
        }
      }
      if (req.method === 'POST' && pathname === '/sessions') {
        if (!ready || stopping) return send(res, 503, { error: { code: 'LOGIN_TRANSPORT_REQUIRED', message: 'Worker 传输尚未就绪', retryable: false } });
        const body = await readBody(req);
        if (body?.provider !== authorization.provider) throw fail('TOKEN_CLIENT_MISMATCH');
        if (body.requestId !== undefined && (typeof body.requestId !== 'string' || !/^[A-Za-z0-9_-]{32,80}$/.test(body.requestId))) throw fail();
        if (body.requestId && [...sessions.values()].some(item => item.requestId === body.requestId)) throw fail();
        const context = inputContext(body.context);
        if (sessions.size + inspections.size >= maxSessions) return send(res, 409, { error: { code: 'UPSTREAM_UNAVAILABLE', message: 'Worker 当前忙碌，请稍后重试', retryable: true } });
        session = { id: randomBytes(24).toString('base64url'), requestId: body.requestId, context, authorization, controller: new AbortController(), children: new Set(), busy: false, closed: false, lastData: null };
        sessions.set(session.id, session);
        session.timer = setTimeout(() => { void dispose(session); }, ttlMs);
        session.timer.unref();
        let native;
        try {
          native = await nativeFactory({ ...context, authorization, signal: session.controller.signal });
          if (session.closed || res.destroyed || stopping) { await silentClose(native); throw fail('CANCELLED'); }
          session.client = createLegacyTransportBridge(native, { authorization, signal: session.controller.signal, workspacePreference: session.context.workspacePreference });
          session.native = native;
        } catch (error) { await silentClose(native); await dispose(session); throw error; }
        return send(res, 201, { sessionId: session.id });
      }
      const match = /^\/sessions\/([A-Za-z0-9_-]{32,80})(\/call)?$/.exec(pathname);
      if (!match) return send(res, 404, { error: { code: 'LOGIN_REQUEST_REJECTED', message: 'Worker 接口不存在', retryable: false } });
      session = sessions.get(match[1]) || (req.method === 'DELETE' && !match[2] ? [...sessions.values()].find(item => item.requestId === match[1]) : undefined);
      if (!session || session.closed) return send(res, 404, { error: { code: 'LOGIN_TIMEOUT', message: 'Worker 会话已结束，请重新提交', retryable: true } });
      if (req.method === 'DELETE' && !match[2]) { await dispose(session); return send(res, 200, { result: null }); }
      if (req.method !== 'POST' || !match[2]) throw fail();
      const body = await readBody(req);
      if (!METHODS.has(body?.method) || !Array.isArray(body.args)) throw fail();
      if (body.method === 'close') { await dispose(session); return send(res, 200, { result: null }); }
      if (session.busy) return send(res, 409, { error: { code: 'UPSTREAM_UNAVAILABLE', message: 'Worker 会话步骤尚未结束', retryable: false } });
      session.busy = true;
      try {
        const args = body.args;
        const method = body.method;
        let callArgs;
        if (method === 'bootstrapAuthorize') callArgs = [authorizeUrl(args[0], session)];
        else if (method === 'submitEmail') { if (args[0] !== session.context.email) throw fail(); callArgs = [args[0]]; }
        else if (method === 'submitPassword') { if (args[0] !== session.context.password) throw fail(); callArgs = [args[0]]; }
        else if (method === 'getAuthorizationCode') {
          const requested = args[1] || {};
          if (requested.expectedState !== session.context.expectedState || (requested.targetWorkspaceId || '') !== session.context.targetWorkspaceId) throw fail('OAUTH_STATE_MISMATCH');
          const selectedWorkspaceId = requested.selectedWorkspaceId;
          if (selectedWorkspaceId !== undefined && (typeof selectedWorkspaceId !== 'string' || !session.workspaceOfferedIds?.has(selectedWorkspaceId))) throw fail('WORKSPACE_NOT_FOUND');
          if (!session.lastData) throw fail('INVALID_LOGIN_RESPONSE');
          callArgs = [session.lastData, { targetWorkspaceId: session.context.targetWorkspaceId, selectedWorkspaceId, expectedState: session.context.expectedState }];
        } else {
          if (typeof args[0] !== 'string' || args[0].length > 256) throw fail();
          if (method === 'submitEmailOtp') { if (!/^\d{6,8}$/.test(args[0])) throw fail(); callArgs = [args[0]]; }
          else {
            const payload = session.lastData?.page?.payload || {};
            const factorIds = [payload.factor_id, ...(Array.isArray(payload.factors) ? payload.factors.filter(item => item?.factor_type === 'totp').map(item => item.id) : [])];
            if (!factorIds.includes(args[0])) throw fail('MFA_FACTOR_MISMATCH');
            if (method === 'verifyMfaTotp' && !/^\d{6,8}$/.test(args[1] || '')) throw fail();
            callArgs = method === 'verifyMfaTotp' ? [args[0], args[1]] : [args[0]];
          }
        }
        let result;
        try { result = await storage.run(session, () => session.client[method](...callArgs)); }
        catch (error) {
          if (method !== 'getAuthorizationCode' || error?.code !== 'WORKSPACE_SELECTION_REQUIRED' || session.closed) throw error;
          const requestedIds = new Set((Array.isArray(error.workspaceCandidates) ? error.workspaceCandidates : []).map(choice => choice?.id));
          const choices = getWorkspaceCandidates(session.native?.lastSession?.workspaces).filter(choice => requestedIds.has(choice.id)).slice(0, 100);
          if (!choices.length || session.context.targetWorkspaceId) throw error;
          session.workspaceOfferedIds = new Set(choices.map(choice => choice.id));
          return send(res, 200, { result: { workspaceSelectionRequired: true, choices } });
        }
        if (session.closed) throw fail('CANCELLED');
        if (['submitEmail', 'submitPassword', 'verifyMfaTotp', 'submitEmailOtp'].includes(method)) {
          session.lastData = result;
          session.workspaceOfferedIds = null;
          return send(res, 200, { result: safeStep(result) });
        }
        if (method === 'getAuthorizationCode') { session.workspaceOfferedIds = null; return send(res, 200, { result: { code: result.code, state: result.state, workspaceId: result.workspaceId } }); }
        return send(res, 200, { result: null });
      } finally {
        session.busy = false;
        if (session.closed) {
          await silentClose(session.client);
          sessions.delete(session.id); session.lastData = null; session.native = null; session.context = null; session.workspaceOfferedIds = null;
        }
      }
    } catch (error) {
      const context = session?.context || {};
      const safe = sanitizeLoginError(error, [key.toString(), workspaceAccessToken, context.email, context.password, context.totpSecret, context.expectedState].filter(Boolean));
      send(res, 502, { error: { code: safe.code, message: safe.message, causeCode: safe.causeCode, httpStatus: safe.httpStatus, retryable: safe.retryable } });
    }
  });
  server.requestTimeout = 180000;
  return {
    server, health,
    async listen() { await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, host, resolve); }); return server.address(); },
    async close() {
      stopping = true;
      for (const inspection of inspections) inspection.controller.abort();
      await Promise.all([...sessions.values()].map(dispose));
      server.closeAllConnections();
      if (server.listening) await new Promise(resolve => server.close(resolve));
    },
  };
}

export function validatePrivateRunnerEnvelope(envelope, { kernelFlags = [], ipcConnected = false } = {}) {
  if (!envelope || !Array.isArray(envelope.args) || envelope.args.some(value => typeof value !== 'string') || !['legacy', 'paired'].includes(envelope.mode)) throw fail('LOGIN_TRANSPORT_REQUIRED');
  const flags = envelope.args.filter(value => value === '--ipc' || value === '--emit-so');
  if (envelope.mode === 'paired') {
    if (kernelFlags.join('|') !== '--ipc|--emit-so' || flags.join('|') !== '--ipc|--emit-so' || !ipcConnected || envelope.args.includes('--challenge-file') || envelope.args.includes('--challenge-url') || envelope.challengePath !== undefined || envelope.challengeText !== undefined) throw fail('LOGIN_TRANSPORT_REQUIRED');
  } else {
    const index = envelope.args.indexOf('--challenge-file');
    if (kernelFlags.length || flags.length || ipcConnected || index < 0 || typeof envelope.challengePath !== 'string' || path.resolve(envelope.args[index + 1] || '') !== envelope.challengePath || typeof envelope.challengeText !== 'string') throw fail('LOGIN_TRANSPORT_REQUIRED');
  }
  if (envelope.sdkAsset !== undefined) {
    const asset = envelope.sdkAsset;
    const sdkIndex = envelope.args.indexOf('--sdk');
    if (!asset || typeof asset.path !== 'string' || sdkIndex < 0 || !path.isAbsolute(asset.path) || path.resolve(envelope.args[sdkIndex + 1] || '') !== asset.path || typeof asset.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(asset.sha256) || typeof asset.text !== 'string' || Buffer.byteLength(asset.text) > 2097152 || hash(asset.text) !== asset.sha256) throw fail('SDK_VERSION_DRIFT');
  }
  return envelope.mode;
}

async function runPrivateRunner(runnerPath) {
  const envelope = await readMemoryJson(process.stdin, 2097152);
  const filename = path.resolve(runnerPath);
  const source = fs.readFileSync(filename, 'utf8');
  if (hash(source) !== REVIEWED['sentinel/sentinel-runner.js']) throw fail();
  validatePrivateRunnerEnvelope(envelope, { kernelFlags: process.argv.slice(4), ipcConnected: typeof process.send === 'function' && process.connected === true });
  const privateFs = { ...fs, readFileSync(target, options) {
    if (typeof target === 'string' && path.resolve(target) === envelope.challengePath) return options ? envelope.challengeText : Buffer.from(envelope.challengeText);
    if (typeof target === 'string' && path.resolve(target) === envelope.sdkAsset?.path) return options ? envelope.sdkAsset.text : Buffer.from(envelope.sdkAsset.text);
    return fs.readFileSync(target, options);
  } };
  const module = compilePrivateCjs(filename, source, { 'node:fs': privateFs });
  await module.exports.main(envelope.args, true);
}

export function workerEnvironmentConfig(environment = process.env) {
  return {
    keyFile: environment.CPA_WORKER_KEY_FILE, enabled: environment.CPA_WORKER_ENABLE_LEGACY === '1',
    legacyRoot: environment.CPA_WORKER_LEGACY_ROOT || '/app', port: Number(environment.CPA_WORKER_PORT || 19418),
    maxSessions: Number(environment.CPA_WORKER_MAX_SESSIONS || 1),
    sdkContextMode: environment.CPA_WORKER_SDK_CONTEXT_MODE || 'legacy', approvedSdkSha256: environment.CPA_WORKER_APPROVED_SDK_SHA256,
  };
}

if (process.argv[1] && path.resolve(process.argv[1]) === WORKER_FILE) {
  if (process.argv[2] === '--sdk-stdin-runner') {
    runPrivateRunner(process.argv[3]).catch(() => { process.stderr.write('SDK_RUNNER_FAILED\n'); process.exitCode = 1; });
  } else {
    createWorkerHost(workerEnvironmentConfig()).then(async worker => {
      await worker.listen();
      process.stdout.write(`CPA worker listening on loopback; transport=${worker.health().transport}\n`);
      const stop = () => { void worker.close().then(() => process.exit(0)); };
      process.once('SIGTERM', stop); process.once('SIGINT', stop);
    }).catch(() => { process.stderr.write('CPA worker could not start\n'); process.exitCode = 1; });
  }
}
