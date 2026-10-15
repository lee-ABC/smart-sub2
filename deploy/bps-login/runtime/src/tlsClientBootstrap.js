// ── node-tls-client 的 Go 引擎（tls-client-*.dll/.so/.dylib）本地固化 ────────────
// node-tls-client 把引擎放在 os.tmpdir()：文件不在就**运行时**去 api.github.com 拉 release。
// 系统清理临时目录 / GitHub 限流(403) / 网络不通 时它拿不到资产，而
// node_modules/node-tls-client/dist/utils/native.js 里 validateFile() 的失败分支是
// **process.exit(1)** —— 整条管线被静默带走（2026-07-27 启动即挂，日志末尾只留
// "Failed to fetch the latest version. Status code: 403" + "Failed to find required asset"）。
//
// 这里做两件事：
//   1) 引擎固化进 vendor/，进程起来先复制进 tmpdir，让 validateFile() 永远命中"文件已存在"分支
//      → 不再依赖启动时能连上 GitHub；tmpdir 被清也能自愈。
//   2) requireTlsClient() 把"引擎缺失"从 process.exit(1) 降级成**可捕获的异常**
//      → 调用方（账号检测/RT 协议模式）自己降级到浏览器路径，不再拖垮整个进程。
// vendor/ 里的文件从 https://github.com/bogdanfinn/tls-client/releases 取，
// 重命名成下表的 name（也可直接跑 `npm run fetch:tls`）。

// ═══════════════════════════════════════════════════════════════════════════
// ★★★ 【B6-1 定案】per-request 超时在本库【不存在入口】——
//      "单 Session + per-request 超时实现隔离"这条建议按字面【不成立】，别再提。
// ═══════════════════════════════════════════════════════════════════════════
//
// 【为什么把这段写在这里】这条建议在计划里已经出现过两次，其中 W8-S15 逐字写着
//   「用 per-request 超时 + maxTries:1 实现隔离」。它听起来完全合理，所以【不写下来
//   下一个人还会再提第三次】—— 而照它做会造出一个比现状更糟的系统：
//   今天一条卡死的可选遥测只占住"第二条连接"（辅助 session，8000ms 上限）；
//   删掉辅助 session 之后它继承主会话超时，**占住注册主连接 40 秒**
//   （主会话 timeout: 40000，见 probe_protocol_register.js:839 与 :1636）。
//   更阴的是：那份验收判据里没有任何一条会去看"超时到底生没生效"，
//   所以这个退化会【静默】通过验收。
//
// ── 复核口径（谁都能在 30 秒内重跑一遍）─────────────────────────────────
//   库：node-tls-client@2.1.0（node_modules/node-tls-client/package.json）
//   ① 逐字证据 —— node_modules/node-tls-client/dist/lib/Session.js:126：
//          timeoutMilliseconds: this.config.timeout || 0,
//      它在 `async execute(method, url, options = {})` 构造的 payload 里。
//      同一个 payload 从 `options` 里读了 followRedirects / headers / headerOrder /
//      proxy / body / byteResponse / hostOverride / cookies …… 唯独超时【不读
//      options】，只读 `this.config` —— 也就是 new Session(config) 那一刻的值。
//      ⇒ 你传 session.post(url, { timeout: 1500 }) 不会报错、不会警告，
//        **它就是被静默丢掉**。这是最坏的一种失败：看起来生效了。
//   ② 覆盖性证据（比 ① 更硬）—— 整个已发布 dist 里 /timeout/i 只出现【1 次】，
//      就是上面那一行：
//          grep -rni "timeout" node_modules/node-tls-client/dist/ --include=*.js | wc -l
//          → 1        （2026-09-05 实测 1/1）
//      ⇒ 不存在"别处还有个 per-request 超时入口被我漏看了"的可能。
//   ③ Go 侧（★ 本仓【读不到】源码，所以这一条按未证实处理，别当结论引用）：
//      bogdanfinn/tls-client 的共享库按 sessionId 缓存 http client，超时是在
//      client 创建那一刻绑上去的；同一 sessionId 的后续请求即使 payload 里
//      timeoutMilliseconds 变了也不重建 client。若哪天有人把 ① 打补丁改成
//      读 options.timeout，**必须先用下面 §复跑实验 验一遍 Go 侧认不认**，
//      不许因为"JS 传过去了"就认为它生效了 —— 那正是「别拿'我们没记到'当
//      '它没发生'」的反面。
//
// ── §复跑实验（会发真实网络，人工执行；test_tls_client_bootstrap.js 里有 skip 版）──
//   A) new Session({ timeout: 40000 }) 预热一发（让 Go 侧按该 sessionId 建好 client），
//      再对同一 session 发 { timeout: 1500 } 打一个 ≥5s 才响应的慢端点
//      → 观察：**不在 1.5s 超时**（≈5s 后正常返回）。⇒ per-request 值没生效。
//   B) 另起 new Session({ timeout: 1500 })，打同一个慢端点
//      → 观察：**约 1.5s 抛超时**。⇒ 超时只认 session 级配置。
//   A 与 B 的差只有"值放在哪儿"，所以它把结论钉死在"绑定时机"上。
//
// ── ⇒ 所以"隔离"只能靠这三件套，【没有一件是超时参数】──────────────────
//   1) 独立 session：可选遥测用自己的 tls.Session（自己的连接、自己的 timeout），
//      卡死时占住的是它自己那条连接，不碰注册链路。
//      （现状：src/protocolTelemetryLane.js 的 optionalTelemetrySessionFor()）
//   2) Promise.race：JS 侧自己掐表放行，不等 Go 侧。
//      ⚠ race 只让【调用方】往下走，底层请求仍在跑 —— 必须有 late-completion 记账，
//        否则就是"我们没记到"被当成"它没发"。
//   3) 信号量：限制同时在飞的可选遥测条数，让"卡住 N 条"不能累积成占满连接池。
//   B6 若要删辅助 session，删掉的是 (1)，那么 (2)(3) 必须先真的存在且被验过；
//   任何以"改用 per-request 超时"为由删 (1) 的改动，直接驳回。
// ═══════════════════════════════════════════════════════════════════════════

const fs = require('fs');
const os = require('os');
const path = require('path');

// 与 node-tls-client/dist/utils/native.js 的 retrieveFileInfo() 保持一致（改版本时对一下）
const FILE_MAP = {
    darwin: {
        arm64: { name: 'tls-client-arm64.dylib', downloadName: 'tls-client-darwin-arm64-{version}.dylib' },
        x64: { name: 'tls-client-x86.dylib', downloadName: 'tls-client-darwin-amd64-{version}.dylib' },
    },
    win32: {
        x64: { name: 'tls-client-64.dll', downloadName: 'tls-client-windows-64-{version}.dll' },
        ia32: { name: 'tls-client-32.dll', downloadName: 'tls-client-windows-32-{version}.dll' },
    },
    linux: {
        arm64: { name: 'tls-client-arm64.so', downloadName: 'tls-client-linux-arm64-{version}.so' },
        x64: { name: 'tls-client-x64.so', downloadName: 'tls-client-linux-ubuntu-amd64-{version}.so' },
        default: { name: 'tls-client-amd64.so', downloadName: 'tls-client-linux-ubuntu-amd64-{version}.so' },
    },
};

const RELEASE_PAGE = 'https://github.com/bogdanfinn/tls-client/releases';
const VENDOR_DIR = path.join(__dirname, '..', 'vendor');

function fileInfo() {
    const byPlatform = FILE_MAP[process.platform];
    return (byPlatform && byPlatform[process.arch]) || FILE_MAP.linux.default;
}
function vendorPath() { return path.join(VENDOR_DIR, fileInfo().name); }
function runtimePath() { return path.join(os.tmpdir(), fileInfo().name); }

function sizeOf(p) { try { return fs.statSync(p).size; } catch { return -1; } }

let _cached = null;
let _responseMetricsPatched = false;

/**
 * 确保 os.tmpdir() 里有可用引擎；返回 { ok, path, source, copied?, reason? }，不抛异常。
 * source: 'tmpdir'（本来就在）| 'vendor'（本次从 vendor 复制）
 */
function ensureTlsLibrary({ force = false } = {}) {
    if (_cached && !force) return _cached;
    const target = runtimePath();
    const source = vendorPath();
    const srcSize = sizeOf(source);
    const dstSize = sizeOf(target);

    // tmpdir 里已有一份，且 vendor 没有或大小一致 → 直接用（避免无谓覆盖：Windows 上别的进程可能正加载着它）
    if (dstSize > 0 && (srcSize <= 0 || dstSize === srcSize)) {
        return (_cached = { ok: true, path: target, source: 'tmpdir' });
    }
    if (srcSize <= 0) {
        return (_cached = {
            ok: false,
            path: target,
            reason: `TLS 引擎缺失：${target} 不存在，且 ${source} 也没有。`
                + ` 跑 \`npm run fetch:tls\`，或从 ${RELEASE_PAGE} 下载 ${fileInfo().downloadName.replace('{version}', '<版本>')} 重命名为 ${fileInfo().name} 放进 vendor/。`,
        });
    }

    // 原子落盘：先写 .tmp-<pid> 再 rename，避免多进程/半份文件被 koffi 加载
    const staging = `${target}.${process.pid}.tmp`;
    try {
        fs.mkdirSync(path.dirname(target), { recursive: true });
        fs.copyFileSync(source, staging);
        fs.renameSync(staging, target);
    } catch (e) {
        try { fs.unlinkSync(staging); } catch {}
        // Windows 上目标被别的进程加载着会 EBUSY/EPERM：只要 tmpdir 那份还在就照用（版本可能旧一档，功能一致）
        if (sizeOf(target) > 0) return (_cached = { ok: true, path: target, source: 'tmpdir' });
        return (_cached = { ok: false, path: target, reason: `复制 TLS 引擎失败(${source} → ${target}): ${e && e.message}` });
    }
    return (_cached = { ok: true, path: target, source: 'vendor', copied: true });
}

/**
 * require('node-tls-client') 的安全版：引擎不在就抛（code=TLS_LIB_MISSING），
 * 而不是等 initTLS() 内部 process.exit(1) 把进程干掉。必须在 initTLS() 之前调用。
 */
function requireTlsClient() {
    const r = ensureTlsLibrary();
    if (!r.ok) {
        const err = new Error(r.reason);
        err.code = 'TLS_LIB_MISSING';
        throw err;
    }
    const tls = require('node-tls-client');
    patchTlsResponseMetrics();
    patchTlsBinaryBody();
    return tls;
}

/* ── 不靠请求头声明的二进制请求体 ─────────────────────────────────────────
 *
 * 【为什么需要】Statsig 的 chatgpt.com 实例把 rgstr 请求体 gzip 后发送，
 *   靠查询串 `gz=1` 声明 —— 实测 506/506 都这样，而且【没有 content-encoding，
 *   也没有 content-type】。
 *   但 node-tls-client 只按【请求头】判断要不要走二进制：
 *   `isByteRequest(headers)` 看 content-encoding / content-type /
 *   content-transfer-encoding。也就是说要发得像就不能加那两个头，
 *   不加那两个头就发不了二进制 —— 死结。
 *
 * 【怎么解】给 headers 对象挂一个 Symbol 键的标记。
 *   · Symbol 键不会被 JSON.stringify 序列化，所以【不会上到线上】；
 *   · isByteRequest 拿到的就是同一个 headers 对象，读得到它；
 *   · headers 是每请求一个对象，天然没有并发串台问题
 *     （不违反"绝不能退回模块级单槽"那条）。
 *
 * 【已验证】2026-09-03 本地 echo 实测：
 *   base64(gzip) + isByteRequest=true → 服务端收到 49 字节、头 1f8b0800、解压正确；
 *   把原始 gzip 当字符串直接发 → 收到 65 字节、头 1fc28b08（UTF-8 把 0x8b
 *   撑成了 c2 8b），确认那条路是死的。
 *
 * ⚠ 用它的时候 body 必须是【base64 字符串】，不是 Buffer 也不是原始字节。
 */
const BINARY_BODY = Symbol.for('xm.tls.binaryBody');
let _binaryBodyPatched = false;

/** 给一份请求头打上"这次走二进制"的标记；返回同一个对象，方便链式写。 */
function markBinaryBody(headers) {
    if (headers && typeof headers === 'object') headers[BINARY_BODY] = true;
    return headers;
}

function patchTlsBinaryBody() {
    if (_binaryBodyPatched) return true;
    const requestUtils = require('node-tls-client/dist/utils/request');
    const original = requestUtils.isByteRequest;
    if (typeof original !== 'function' || original.__xmBinaryBody) return false;
    const patched = headers => {
        if (headers && typeof headers === 'object' && headers[BINARY_BODY] === true) return true;
        return original(headers);
    };
    patched.__xmBinaryBody = true;
    requestUtils.isByteRequest = patched;
    _binaryBodyPatched = true;
    return true;
}

/**
 * node-tls-client 的 Response 包装器只复制固定字段，会丢掉补丁引擎返回的
 * encodedBodySize。Session 持有的是 Response 模块对象引用，因此替换导出类即可
 * 在不改 node_modules 的前提下保留该字段。
 */
function patchTlsResponseMetrics() {
    if (_responseMetricsPatched) return true;
    const responseModule = require('node-tls-client/dist/lib/Response');
    const BaseResponse = responseModule.Response;
    if (!BaseResponse || BaseResponse.__xmEncodedBodySize) return false;
    class MeasuredResponse extends BaseResponse {
        constructor(response) {
            super(response);
            this.encodedBodySize = Math.max(0, Number(response && response.encodedBodySize) || 0);
            this.usedProtocol = String(response && response.usedProtocol || '');
        }
    }
    MeasuredResponse.__xmEncodedBodySize = true;
    responseModule.Response = MeasuredResponse;
    _responseMetricsPatched = true;
    return true;
}

/**
 * ★★ 绕开 node-tls-client 的一个真 bug：响应里带 __Secure- / __Host- 前缀的 cookie 时，
 *    **整个请求会抛 TypeError，连响应都拿不到**。
 *
 * 成因（dist/lib/Cookie.js:31-38）：Session.execute() 拿到响应后调 jar.syncCookies()，
 * 后者把服务端 cookie **重建成裸 `name=value`** —— 原本的 Secure / Path 属性全丢了 ——
 * 然后无条件读 `cookie.key`。而 tough-cookie 对这两个前缀强制要求那些属性
 * （__Secure- 要 Secure；__Host- 还要 Path=/ 且无 Domain），缺了就拒收返回 undefined，
 * 于是 `undefined.key` 抛异常。
 *
 * 实测受害面：chatgpt.com/api/auth/session 必然下发 __Secure-next-auth.session-token，
 * 所以那条路 100% 必炸。rtPureProtocolClient.preflight() 里那句「对 chatgpt.com 重复请求会抛
 * Cannot read properties of undefined (reading 'key')」也是同一个根因（当时按噪音日志绕过去了）。
 * accountDetector.detectViaTls 至今没踩到，是因为 backend-api 走 Bearer，响应里只有
 * __cf_bm 这类无前缀 cookie。
 *
 * 修法：把属性补回去再存，且**单个 cookie 存不进去绝不让整个请求失败** —— jar 里存成什么
 * 其实无关紧要，需要 set-cookie 值的调用方都是直接读 response.headers 的。
 *
 * ★ 必须在 new Session() 之后、发第一个请求之前调用。
 */
function hardenCookieJar(session) {
    const jar = session && session.jar;
    if (!jar || typeof jar.setCookie !== 'function') return session;
    const cookieAttributes = key => {
        const name = String(key || '');
        // tough-cookie enforces the prefix rules even when a cookie is being
        // supplied for an outbound request.  node-tls-client's default
        // mergeCookies() used `name=value` only, so __Secure-/__Host- values
        // returned undefined and the caller then crashed on `cookie.key`.
        if (/^__Host-/i.test(name)) return '; Path=/; Secure';
        if (/^__Secure-/i.test(name)) return '; Path=/; Secure';
        return '';
    };
    const setCookieSafe = async (key, value, url) => {
        try {
            return await jar.setCookie(
                `${key}=${value == null ? '' : value}${cookieAttributes(key)}`,
                url,
                { ignoreError: true },
            );
        } catch (_) {
            return null;
        }
    };
    jar.syncCookies = async (cookies, url) => {
        if (!cookies) return {};
        const result = {};
        for (const [key, value] of Object.entries(cookies)) {
            const cookie = await setCookieSafe(key, value, url);
            if (cookie && cookie.key) result[cookie.key] = cookie.value;
        }
        return result;
    };
    // Session.execute() uses mergeCookies() for the explicit cookie object on
    // each outbound request.  Patching only syncCookies() fixes response
    // reconciliation but leaves the next request vulnerable to the same
    // prefix exception.  Return the library's documented [{name,value}]
    // shape, silently omitting only the individual cookie that cannot be
    // stored; one malformed cookie must not abort telemetry or registration.
    jar.mergeCookies = async (cookies, url) => {
        if (!cookies || typeof cookies !== 'object') return [];
        const result = [];
        for (const [key, value] of Object.entries(cookies)) {
            const cookie = await setCookieSafe(key, value, url);
            if (cookie && cookie.key) result.push({ name: cookie.key, value: cookie.value });
        }
        return result;
    };
    return session;
}

/** 启动期自检：把结果打一行日志，缺引擎只告警不退出（账号检测/协议模式会自己回退浏览器） */
function reportTlsLibrary(log = console.log) {
    const r = ensureTlsLibrary();
    if (r.ok) {
        if (r.copied) log(`[TLS引擎] 已从 vendor/ 装载到 ${r.path}`);
    } else {
        log(`[TLS引擎] ⚠️ ${r.reason} → 账号检测/RT协议模式将回退浏览器路径`);
    }
    return r;
}

module.exports = {
    ensureTlsLibrary, requireTlsClient, reportTlsLibrary, hardenCookieJar,
    patchTlsResponseMetrics, fileInfo, vendorPath, runtimePath, RELEASE_PAGE, VENDOR_DIR,
    BINARY_BODY, markBinaryBody, patchTlsBinaryBody,
};
