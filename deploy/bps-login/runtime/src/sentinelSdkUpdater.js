'use strict';
// Sentinel SDK 自动跟版 —— 把「发现线上换版了」变成「已经跟上了」。
//
// ════════════════════════════════════════════════════════════════════════════
// 【为什么需要它】（2026-08-25）
//
//   我们把 SDK 版本号写死在代码里（sentinelFingerprint.DEFAULT_PROFILE.sentinelSv），
//   而这个版本号【会被发出去三处】：
//     ① sentinel/req 的 referer：.../frame.html?sv=<sv>
//     ② runner 的 --script-src → 进 p 载荷的 [5] 槽（"我加载的是哪个脚本"）
//     ③ rtPureProtocolClient 的预热请求
//
//   语料实证（扫 86 份浏览器抓包）：线上【同时存在两个版本】——
//       sentinel/20260810913b/sdk.js   54 份
//       sentinel/20260219f9f6/sdk.js   20 份
//   也就是说 OpenAI 确实在换版，而且我们的语料正好抓到了一次换版过程。
//
//   `detectLiveSdkVersion()`（sentinelClient.js）早就能【发现】漂移了，
//   但发现之后是人工：下载、改常量、重启。本模块补上缺的那一半。
//
//   不跟版的两种后果：
//     · 旧版还能用 ⇒ 全网真人都换了新版，只有我们这批号还报一个没人用的旧版本号。
//       覆盖率 100%、零良性解释，是一条干净的聚类特征。
//     · 旧版被下架 ⇒ 注册大面积失败，而且没人第一时间知道原因
//       （test_sentinel_version_check.js 文件头记着这个场景：面板绿着、
//         排查会被带去查 IP/指纹/代理）。
//
// ════════════════════════════════════════════════════════════════════════════
// 【最重要的不变量：版本号与 SDK 文件必须成对】
//
//   sentinelClient.js 的 SDK_PATH 注释写着：「SENTINEL_SV 要和 SENTINEL_SDK_PATH
//   配对使用，否则 p[5] 里报的版本号和实际跑的代码版本对不上，那比不换更可疑。」
//
//   所以本模块【只暴露一个解析器】activeSdk()，同时返回 sv 与 path。
//   调用方不许分别去别处取其中一个 —— 那就是漂移的开始。
//
// ════════════════════════════════════════════════════════════════════════════
// 【默认关，必须显式开】
//
//   与 ipcHandshake 同一条纪律（sentinelClient.js:124）：生产 RT 链路已经用
//   钉死的版本发了上千次、5000+ 号 0 封号。把它换成「文件会自己变」是有风险的改动。
//   config.json 的 `sentinelSdkAutoUpdate: true` 才启用；关着时 activeSdk() 返回
//   的就是今天这一对（钉死的常量 + sentinel/sdk.js），【逐字节等价于改动前】。
//
//   即使关着，syncSentinelSdk({dryRun:true}) 仍可用来"只看不动"，
//   以及 tools/update_sentinel_sdk.js 手动跟版。

const fs = require('fs');
const { headerOrderFor } = require('./chromeHeaderOrder');
const { rtClientIdentifier } = require('./tlsProfile');
const path = require('path');
const vm = require('vm');
const crypto = require('crypto');

const SENTINEL_DIR = path.join(__dirname, '..', 'sentinel');
const POINTER = path.join(SENTINEL_DIR, 'sdk-active.json');
const PINNED_FILE = path.join(SENTINEL_DIR, 'sdk.js');
const SENTINEL_BASE = 'https://sentinel.openai.com';

// 跟版探测的最小间隔。默认 1 小时 —— 换版是天级事件，探太勤只是白送足迹。
const DEFAULT_TTL_MS = 3600_000;

// ── 校验口径（来自两个【真实】SDK 文件的实测，不是照抄别的项目的猜测）──
//   sdk-20260810913b.js  33820 字节
//   sdk-20260219f9f6.js  30863 字节
//   两者都以 `var SentinelSDK=function(` 开头、都含 'turnstile'。
const MIN_BYTES = 8 * 1024;
const MAX_BYTES = 512 * 1024;
const HEAD_RE = /^\s*var\s+SentinelSDK\s*=\s*function\s*\(/;
const MUST_CONTAIN = ['turnstile'];
// 版本号形态：抓包里是 20260810913b / 20260219f9f6（8~24 位十六进制）
const SV_RE = /^[0-9a-f]{8,24}$/i;

let _lastSyncAt = 0;
let _lastResult = null;

/** 钉死的版本号 —— 唯一的编译期真值来源，不在本文件里再抄一份。 */
function pinnedSv() {
    try { return String(require('./sentinelFingerprint').DEFAULT_PROFILE.sentinelSv || ''); }
    catch (_) { return ''; }
}

function autoUpdateEnabled() {
    try { return require('./config').sentinelSdkAutoUpdate === true; }
    catch (_) { return false; }
}

function ttlMs() {
    try {
        const v = Number(require('./config').sentinelSdkCheckIntervalMs);
        return Number.isFinite(v) && v > 0 ? v : DEFAULT_TTL_MS;
    } catch (_) { return DEFAULT_TTL_MS; }
}

const sha256 = buf => crypto.createHash('sha256').update(buf).digest('hex');
const versionedPath = sv => path.join(SENTINEL_DIR, `sdk-${sv}.js`);

/**
 * 内容校验。**下载到的东西必须先过这一关才允许落盘。**
 * 宁可继续用旧版，也不要把一个残缺/被中间人改过的文件放进 VM 去跑。
 */
function validateSdk(text) {
    const problems = [];
    const bytes = Buffer.byteLength(text || '');
    if (bytes < MIN_BYTES) problems.push(`只有 ${bytes} 字节，小于下限 ${MIN_BYTES}（多半是错误页或被截断）`);
    if (bytes > MAX_BYTES) problems.push(`${bytes} 字节，超过上限 ${MAX_BYTES}`);
    if (!HEAD_RE.test(String(text).slice(0, 200))) {
        problems.push('开头不是 `var SentinelSDK=function(` —— 两个已知真版本都是这个形态');
    }
    for (const m of MUST_CONTAIN) {
        if (!String(text).includes(m)) problems.push(`正文里找不到 '${m}'`);
    }
    // ★ 语法预检：new vm.Script 只编译不执行，等价于 node --check 但不用起进程。
    //   落盘一个语法坏掉的文件 = 下一次注册在 VM 里炸，且现场极难看懂。
    if (!problems.length) {
        try { new vm.Script(String(text), { filename: 'sentinel-sdk-candidate.js' }); }
        catch (e) { problems.push(`语法预检失败：${String(e.message).slice(0, 120)}`); }
    }
    return { ok: problems.length === 0, problems, bytes };
}

/** 读指针文件；文件内容与记录的摘要对不上就当作没有（宁可回落到钉死版）。 */
function readPointer() {
    try {
        const j = JSON.parse(fs.readFileSync(POINTER, 'utf8'));
        const sv = String(j.sv || '');
        if (!SV_RE.test(sv)) return null;
        const p = versionedPath(sv);
        if (!fs.existsSync(p)) return null;
        // ★ 摘要校验：防的是"指针说 X 版，文件却被人手改过/写坏了"。
        //   这种情况下我们会报 X 版却跑着别的代码 —— 正是要避免的那个矛盾。
        if (j.sha256 && sha256(fs.readFileSync(p)) !== j.sha256) return null;
        return { sv, path: p, at: j.at || '', sha256: j.sha256 || '' };
    } catch (_) { return null; }
}

/**
 * ★★★ 唯一的解析器：这一刻该报哪个版本号、该加载哪个文件。
 *
 * 优先级（与 sentinelClient 文件头那条"配对"纪律一致）：
 *   ① 环境变量显式指定（SENTINEL_SV + SENTINEL_SDK_PATH，实验用，必须成对）
 *   ② 自动跟版已启用【且】指针有效  → 跟版下来的那一对
 *   ③ 钉死的默认                    → 常量 + sentinel/sdk.js
 *
 * ⚠ 调用方【不许】只取其中一个再去别处拿另一个。sv 与 path 必须来自同一次调用。
 *
 * @returns {{sv:string, path:string, source:'env'|'auto'|'pinned'}}
 */
function activeSdk() {
    const envSv = String(process.env.SENTINEL_SV || '').trim();
    const envPath = String(process.env.SENTINEL_SDK_PATH || '').trim();
    if (envSv && envPath) {
        return { sv: envSv, path: path.resolve(envPath), source: 'env' };
    }
    if (autoUpdateEnabled()) {
        const ptr = readPointer();
        if (ptr) return { sv: ptr.sv, path: ptr.path, source: 'auto' };
    }
    return { sv: pinnedSv(), path: PINNED_FILE, source: 'pinned' };
}

/** 只读快照，给面板/CLI 用。不发任何网络请求。 */
function sdkStatus() {
    const cur = activeSdk();
    let bytes = 0;
    try { bytes = fs.statSync(cur.path).size; } catch (_) {}
    return {
        activeSv: cur.sv,
        activePath: cur.path,
        source: cur.source,
        bytes,
        pinnedSv: pinnedSv(),
        autoUpdate: autoUpdateEnabled(),
        pointer: readPointer(),
        lastSyncAt: _lastSyncAt ? new Date(_lastSyncAt).toISOString() : '',
        lastResult: _lastResult,
    };
}

/** 原子落盘：临时文件 → rename。半个文件被别的进程读到是真会发生的。 */
function atomicWrite(dest, text) {
    const tmp = `${dest}.tmp-${process.pid}-${Date.now()}`;
    try {
        fs.writeFileSync(tmp, text, 'utf8');
        fs.renameSync(tmp, dest);
    } finally {
        try { if (fs.existsSync(tmp)) fs.unlinkSync(tmp); } catch (_) {}
    }
}

/**
 * 跟一次版。
 *
 * 【绝不抛】—— 这个函数会被注册链路旁路调用，跟版失败不该影响任何一次注册。
 * 失败时保留当前版本并把原因记在返回值里（与对方那个项目的 last_error 同思路）。
 *
 * @param {object} o
 *   proxyUrl   走代理探测（与注册同出口更真实）
 *   userAgent  探测请求的 UA
 *   force      忽略 TTL
 *   dryRun     只探测、不下载不落盘（自动跟版关着时也能用来看一眼）
 *   log
 * @returns {Promise<object>}
 */
async function syncSentinelSdk(o = {}) {
    const log = typeof o.log === 'function' ? o.log : () => {};
    const cur = activeSdk();
    const base = { activeSv: cur.sv, source: cur.source, changed: false };

    if (!o.force && Date.now() - _lastSyncAt < ttlMs()) {
        return { ...base, skipped: 'ttl', nextAllowedInMs: ttlMs() - (Date.now() - _lastSyncAt) };
    }
    _lastSyncAt = Date.now();

    // ── 探测（可换出口重试）──
    //
    // ★★ 2026-08-25 实测补的：首次跑 CLI 就撞上一个被拦的出口，
    //   拿回 HTTP 200 但内容不是加载器脚本（CF 挑战页也是 200），
    //   于是正则空手而归。当时的报错是「版本号形态不对：(空)」——
    //   **把死因记到了错的地方**：读的人会去查版本号规则，而真因是出口被拦。
    //   同一次运行里换个出口就好了（实测 3/3 通过）。
    //
    //   所以这里做两件事：① 有 rotateProxy 就换出口重试；② 报错要说出真因。
    //   ⚠ 没有 rotateProxy（直连 / CLI 单跑）时退回原行为：只试一次。
    //     这与 rtPureProtocolClient 的换出口兜底是同一条纪律。
    const attempts = typeof o.rotateProxy === 'function'
        ? Math.max(1, Number(o.exits) || 3) : 1;
    let liveSv = '';
    let lastWhy = '';
    for (let i = 1; i <= attempts; i++) {
        const px = i === 1 ? o.proxyUrl : (() => {
            try { return o.rotateProxy(); } catch (_) { return o.proxyUrl; }
        })();
        try {
            const { detectLiveSdkVersion } = require('./sentinelClient');
            // ★ 复用既有探测：它读的是【不带版本号的加载器脚本】，那是"浏览器当前用哪版"
            //   的权威来源。别改成问 frame.html 的默认值 —— 那等价于拿本地和本地比，
            //   完整复盘见 test_sentinel_version_check.js 文件头。
            const d = await detectLiveSdkVersion({ proxyUrl: px, userAgent: o.userAgent, localSv: cur.sv });
            if (d.error) { lastWhy = `连接失败：${d.error}`; }
            else if (SV_RE.test(String(d.liveSv || ''))) { liveSv = String(d.liveSv); break; }
            else {
                // HTTP 通了但正文里没有版本号 —— 绝大多数情况是这个出口被 CF 拦了，
                // 拿回的是挑战页（也是 200）。别说成"版本号格式不对"。
                lastWhy = `HTTP ${d.httpStatus ?? '?'} 但正文里没有 sentinel/<版本>/sdk.js`
                    + `（多半是这个出口被拦，拿回的是挑战页）`;
            }
        } catch (e) {
            lastWhy = `探测异常：${String(e.message).slice(0, 120)}`;
        }
        if (i < attempts) log(`[SDK] 第 ${i}/${attempts} 个出口没探到（${lastWhy}），换出口重试`);
    }

    if (!liveSv) {
        _lastResult = { ...base, error: `探测失败：${lastWhy || '未知'}${attempts > 1 ? `（试了 ${attempts} 个出口）` : ''}` };
        log(`[SDK] ${_lastResult.error}`);
        return _lastResult;
    }
    if (liveSv === cur.sv) {
        _lastResult = { ...base, liveSv, upToDate: true };
        log(`[SDK] 已是最新 ${liveSv}`);
        return _lastResult;
    }

    log(`[SDK] 线上已换版：本地 ${cur.sv} → 线上 ${liveSv}`);
    if (o.dryRun) {
        _lastResult = { ...base, liveSv, wouldUpdate: true, dryRun: true };
        return _lastResult;
    }
    if (!autoUpdateEnabled() && !o.force) {
        _lastResult = { ...base, liveSv, wouldUpdate: true, skipped: 'disabled' };
        log('[SDK] 自动跟版未启用（config.sentinelSdkAutoUpdate），只报告不动手');
        return _lastResult;
    }

    // ── 下载 ──
    let text = '';
    try {
        const { requireTlsClient } = require('./tlsClientBootstrap');
        const tls = requireTlsClient();
        await tls.initTLS();
        const sessOpts = { clientIdentifier: rtClientIdentifier(), timeout: 30000, followRedirects: true };
        if (o.proxyUrl) sessOpts.proxy = o.proxyUrl;
        const s = new tls.Session(sessOpts);
        try {
            const sdkHeaders = {
                    'user-agent': o.userAgent || require('./sentinelFingerprint').DEFAULT_PROFILE.userAgent,
                    accept: '*/*',
                    referer: `${SENTINEL_BASE}/backend-api/sentinel/frame.html?sv=${liveSv}`,
                    'sec-fetch-dest': 'script',
                    'sec-fetch-mode': 'no-cors',
                    'sec-fetch-site': 'same-origin',
                };
            const r = await s.get(`${SENTINEL_BASE}/sentinel/${liveSv}/sdk.js`, {
                headers: sdkHeaders,
                headerOrder: headerOrderFor(Object.keys(sdkHeaders)),  // ★ 字母序会被 CF 挑战（docs/协议注册模式.md:226-233 的 A/B）
            });
            if (r.status !== 200) throw new Error(`HTTP ${r.status}`);
            text = await r.text();
        } finally { try { await s.close(); } catch (_) {} }
    } catch (e) {
        _lastResult = { ...base, liveSv, error: `下载失败：${String(e.message).slice(0, 140)}（继续用 ${cur.sv}）` };
        log(`[SDK] ${_lastResult.error}`);
        return _lastResult;
    }

    // ── 校验 ──
    const v = validateSdk(text);
    if (!v.ok) {
        _lastResult = { ...base, liveSv, error: `校验不过：${v.problems.join('；')}（继续用 ${cur.sv}）` };
        log(`[SDK] ${_lastResult.error}`);
        return _lastResult;
    }

    // ── 落盘 + 翻指针（先文件后指针：指针指向一个不存在的文件是最糟的中间态）──
    try {
        const dest = versionedPath(liveSv);
        atomicWrite(dest, text);
        atomicWrite(POINTER, JSON.stringify({
            sv: liveSv,
            sha256: sha256(fs.readFileSync(dest)),
            bytes: v.bytes,
            at: new Date().toISOString(),
            previousSv: cur.sv,
            source: `${SENTINEL_BASE}/sentinel/${liveSv}/sdk.js`,
        }, null, 2));
    } catch (e) {
        _lastResult = { ...base, liveSv, error: `落盘失败：${String(e.message).slice(0, 140)}（继续用 ${cur.sv}）` };
        log(`[SDK] ${_lastResult.error}`);
        return _lastResult;
    }

    // ★★ 2026-08-25：这里区分两个"版本"，别混。混了会打出「A → A」这种看不懂的日志
    //   （第一次模拟换版就是这么打出来的）：
    //     newSv     = 刚下载并写进指针的那一版 —— 这才是"跟到了什么"
    //     activeSv  = 重新解析后【实际生效】的那一版
    //   两者通常相同；不同只有一种情况：env 覆盖（SENTINEL_SV+SENTINEL_SDK_PATH）
    //   优先级高于指针，把跟版结果盖住了。那是【实验没关干扰了跟版】，
    //   必须显式报出来而不是让日志自相矛盾。
    const now = activeSdk();
    _lastResult = {
        newSv: liveSv, activeSv: now.sv, source: now.source, changed: true,
        previousSv: cur.sv, liveSv, bytes: v.bytes,
        ...(now.sv !== liveSv
            ? { maskedBy: now.source, warn: `已跟到 ${liveSv}，但当前生效的仍是 ${now.sv}（被 ${now.source} 覆盖）` }
            : {}),
    };
    log(`[SDK] ✔ 已跟到 ${liveSv}（${v.bytes} 字节），上一版 ${cur.sv}`);
    if (_lastResult.warn) log(`[SDK] ⚠ ${_lastResult.warn}`);
    return _lastResult;
}

module.exports = {
    activeSdk, sdkStatus, syncSentinelSdk, validateSdk, readPointer, pinnedSv,
    POINTER, SENTINEL_DIR, DEFAULT_TTL_MS,
};
