'use strict';
// ── Sentinel 客户端（纯协议 RT 链路专用）──
//
// 两步：
//   ① POST https://sentinel.openai.com/backend-api/sentinel/req  拿 challenge
//      （body.p 由 sentinelFingerprint 生成；走 node-tls-client 以免被 CF 的 TLS 指纹识破）
//   ② 把 challenge 喂给 sentinel/sentinel-runner.js —— Node vm 里跑**真** sdk.js 算出最终 token
//      （含 PoW / Turnstile）。不在 JS 侧手写 PoW：OpenAI 改算法时不用跟着逆向。
//
// 产出：{ sentinelToken, soToken } 分别对应请求头
//   openai-sentinel-token / openai-sentinel-so-token
//
// ★指纹一致性铁律：deviceId、userAgent、时区/语言 三者必须与后续所有 HTTP 请求、
//   以及 authorize URL 上的 ext-oai-did 完全一致，否则风控会看出前后矛盾。
const path = require('path');
const { rtClientIdentifier } = require('./tlsProfile');
const fs = require('fs');
const os = require('os');
const crypto = require('crypto');
const { spawn } = require('child_process');
const { requireTlsClient } = require('./tlsClientBootstrap');
const fp = require('./sentinelFingerprint');
const { headerOrderFor } = require('./chromeHeaderOrder');
const { secFetchHeaders, priorityHeader } = require('./secFetch');

const SENTINEL_BASE = 'https://sentinel.openai.com';
const SENTINEL_REQ_PATH = '/backend-api/sentinel/req';
const SENTINEL_FRAME_PATH = '/backend-api/sentinel/frame.html';
const PROJECT_ROOT = path.resolve(__dirname, '..');
const SENTINEL_DIR = path.join(PROJECT_ROOT, 'sentinel');
const RUNNER_PATH = path.join(SENTINEL_DIR, 'sentinel-runner.js');
// ★ 2026-08-15：SDK 版本实验开关。
//   线上 08-15 12:41 起浏览器全量切到 20260810913b，我们还钉在 20260219f9f6。
//   换版必须先验证「新 sdk.js 在我们的假 DOM 里跑不跑得起来」，所以留一个
//   【环境变量】口子（不是配置项）—— 进程一重启就自动回到默认的 sentinel/sdk.js，
//   实验忘了关也影响不到正常注册。SENTINEL_SV 要和 SENTINEL_SDK_PATH 配对使用，
//   否则 p[5] 里报的版本号和实际跑的代码版本对不上，那比不换更可疑。
//
// ★★★ 2026-08-25：上面那条"必须配对"的纪律，现在由 src/sentinelSdkUpdater.js
//   的 activeSdk() 在【结构上】保证 —— 它一次同时返回 sv 与 path，
//   调用方拿不到只有其中一个的机会。三条优先级（env 成对 > 自动跟版 > 钉死默认）
//   都在那一个函数里，这里不再各算各的。
//   ⚠ 自动跟版默认【关】，关着时 activeSdk() 返回的就是钉死那一对，
//     与本行改动前逐字节等价。
//   ⚠ 【不要】把 SDK_PATH 改回模块级 const —— 跟版是运行时事件，
//     模块级常量会把整个进程钉在启动那一刻的版本上。
const { activeSdk } = require('./sentinelSdkUpdater');
const RUNNER_TIMEOUT_MS = 60_000;

// flow → 浏览器里对应的页面 URL（影响 sdk.js 的 location 指纹）
const FLOW_PAGE_URL = {
    authorize_continue: 'https://auth.openai.com/email-verification',
    oauth_create_account: 'https://auth.openai.com/about-you',
    username_password_create: 'https://auth.openai.com/create-account/password',
};

function codedError(code, message, cause) {
    const error = new Error(message);
    error.code = code;
    if (cause !== undefined) error.cause = cause;
    return error;
}

// ★ 识别「假 turnstile」：sdk 解密 dx 失败时，它的 catch 会把错误对象 base64 之后
//   当 token 交出来，长得跟真 token 一模一样（都是 base64 串、都非空）。
//   2026-08-12 之前线上发的全是这个 —— 因为日志只判了 `!!parsed.t`，而它恒为真。
//   典型内容：「0: SyntaxError: Expected ',' or ']' after array element in JSON ...」
//   真 token 解出来是二进制乱码（不是可读的 ASCII 报错文本），长度也在 900+。
const BROKEN_T_RE = /^\d+:\s*(SyntaxError|TypeError|ReferenceError|RangeError|Error)\b/;
function isBrokenTurnstile(t) {
    if (!t || typeof t !== 'string') return false;
    let decoded = '';
    try { decoded = Buffer.from(t, 'base64').toString('utf8'); } catch (_) { return false; }
    return BROKEN_T_RE.test(decoded);
}

function assertAssets() {
    if (!fs.existsSync(RUNNER_PATH)) {
        throw codedError('SENTINEL_ASSET_MISSING', `缺少 sentinel-runner.js: ${RUNNER_PATH}`);
    }
    const sdk = activeSdk();
    if (!fs.existsSync(sdk.path)) {
        throw codedError('SENTINEL_ASSET_MISSING', `缺少 sdk.js: ${sdk.path}（来源=${sdk.source}）`);
    }
}

class SentinelClient {
    /**
     * @param {object} opts
     *   deviceId    必须与整条链路共用的 oai-did
     *   profile     指纹画像（见 sentinelFingerprint.DEFAULT_PROFILE），至少要跟出口地区一致
     *   proxyUrl    与业务请求同一出口；null=直连
     *   cookieHeader 可选，把 auth 域 cookie 带进 VM 的 document.cookie
     *   log         日志函数
     *   nodeExecutable  可覆盖 node 可执行文件路径（fnm 环境下常用）
     */
    constructor(opts = {}) {
        // ★ 节奏闸门的锚点：最近一次 /sentinel/req【发出】的时刻与往返耗时。
        //   由 _postWithRetry 写。null = 这一轮还没打过 sentinel/req。
        //   调用方（探针）拿它当 paceSince 的起点 —— 浏览器实测区间量的就是
        //   「sentinel/req 发出 → 消费请求发出」，锚点取错闸门就形同虚设。
        this.lastReqSentAt = null;
        this.lastReqMs = null;
        this.deviceId = String(opts.deviceId || fp.newDeviceId());
        this.profile = { ...fp.DEFAULT_PROFILE, ...(opts.profile || {}) };
        // ★★★ 版本号与 sdk.js 实体【必须成对】（见文件头）：报 A 版却跑 B 版比不换更可疑。
        //   2026-08-25 起两者都由 activeSdk() 一次性给出，这里把 profile 上那个
        //   可能来自钉死常量的 sentinelSv 覆盖成【当前真正会加载的那一版】。
        //   ⚠ 覆盖是对的：profile 由调用方拼装（iosProfileFactory / DEFAULT_PROFILE），
        //     它带的是编译期常量，不知道运行时有没有跟过版。
        //   ⚠ 自动跟版关着时 activeSdk().sv 就等于那个常量，此行等价于什么都没做。
        this.profile.sentinelSv = activeSdk().sv || this.profile.sentinelSv;
        this.proxyUrl = opts.proxyUrl || null;
        // ★ sdk.js 在 VM 里从 document.cookie 读 oai-did 当设备 ID；
        //   缺了它 runner 输出的 token 会没有 id 字段。这里统一兜底补齐。
        const rawCookie = String(opts.cookieHeader || '');
        this.cookieHeader = /(^|;\s*)oai-did=/.test(rawCookie)
            ? rawCookie
            : (rawCookie ? `oai-did=${this.deviceId}; ${rawCookie}` : `oai-did=${this.deviceId}`);
        this.log = typeof opts.log === 'function' ? opts.log : () => {};
        this.abortCheck = typeof opts.abortCheck === 'function' ? opts.abortCheck : () => false;
        this.nodeExecutable = opts.nodeExecutable
            || process.env.SENTINEL_NODE_EXECUTABLE
            || process.execPath;   // 默认复用当前 node，fnm 环境下天然正确
        // sdk 内部 sid：真 SDK 会另生成一个 UUID，与 oai-did 不是同一值
        this.sentinelSid = String(opts.sentinelSid || crypto.randomUUID());
        // ★★★ 2026-08-15：字符集从 hex 改成 base36。
        //   实测（p[11]/p[12] 抽到的注入键）：线上 9 份 0/9 是纯 hex，全是 base36（含 g~z）；
        //   我们 17/17 是纯 hex。Fisher p=3.2e-07 —— 单看一个键名就能判，不需要聚类。
        //   成因：React 用 Math.random().toString(36).slice(2) 生成后缀，永远可能含 g~z；
        //   crypto.randomBytes().toString('hex') 【永远不含】g~z。
        //   ⚠ 这里必须复用 sentinelFingerprint 的 randomSuffix：那条 RT 链路早就是 base36 了，
        //     两条链路各写各的等于把我们自己的号劈成两个可区分总体。
        this.reactListeningKey = '_reactListening' + fp.randomSuffix(11);
        this.reactContainerKey = '__reactContainer$' + fp.randomSuffix(11);
        this.reactResourcesKey = this.reactContainerKey.replace('__reactContainer$', '__reactResources$');
        // ★ IPC 握手换题（2026-08-12）。默认关，必须显式开。
        //   开了之后 turnstile 才是真 token；不开则一直是那串 base64 报错。
        //   之所以做成开关而不是直接改默认：生产 RT 链路已经用旧行为发了上千次，
        //   把它换成一个「以前从没发过的形状」是有风险的改动，要先观测再铺开。
        //   环境变量 SENTINEL_IPC_HANDSHAKE=1 可全局打开，便于灰度。
        this.ipcHandshake = opts.ipcHandshake != null
            ? opts.ipcHandshake === true
            : process.env.SENTINEL_IPC_HANDSHAKE === '1';
        // 页面（iframe）打开的绝对时刻，epoch 毫秒。整条注册流程传同一个值，
        // 这样多次 token() 报出的 p[17] timeOrigin 才是同一个 —— 线上实测同一会话的
        // 3 份 p 里 [14] sid 与 [17] timeOrigin 完全相同。不传则每次随机。
        this.pageOpenAt = Number(opts.pageOpenAt) > 0 ? Number(opts.pageOpenAt) : 0;
        // TLS 档案。不传则用生产默认（chrome_131）——不要在这里改默认值，
        // 那会把 RT / 检测链路一起改掉。协议链路显式传。
        this.clientIdentifier = String(opts.clientIdentifier || '') || null;
        // ★ 观测回调：把本客户端实际发出的请求交给调用方记录（2026-08-14）。
        //   sentinel 走的是它【自己】的 tls.Session，探针的 hop() 录不到 ——
        //   不接这个钩子，抓包里就会凭空少掉 6 跳（4 个 SDK GET + 2 次 POST /req），
        //   而那正好是我们最近在改的那一块。纯观测，不改变发出的字节。
        this.onWire = typeof opts.onWire === 'function' ? opts.onWire : null;
        this._sess = null;
    }

    _ck() { if (this.abortCheck()) throw codedError('ABORTED', 'ABORTED: 已急停/取消'); }

    /**
     * 这一 flow 该在页面上"停留"多久（毫秒）—— 直接决定 so token 里采集器的存活时长。
     *
     * ★ 区间取自浏览器实测的 so 槽0，不是拍脑袋：
     *     email_otp_validate  中位 7.4s（3.9~20.7s）
     *     oauth_create_account 中位 18.0s（13.2~19.0s）
     *   在区间内均匀取样，别取固定值 —— 固定值会让所有号的槽0 落在同一点上，
     *   那是比"太短"更容易聚类的特征。
     * ⚠ 这段时间是真实等待，会让整轮注册变慢约 25 秒。浏览器本来就更慢，这是对的代价。
     */
    _dwellMsFor(flow) {
        const f = String(flow || '');
        const [lo, hi] = /create_account/.test(f) ? [13000, 19000] : [5000, 14000];
        return Math.round(lo + Math.random() * (hi - lo));
    }


    async _ensureSession() {
        if (this._sess) return this._sess;
        const tls = requireTlsClient();
        if (!SentinelClient._tlsInit) SentinelClient._tlsInit = tls.initTLS();
        await SentinelClient._tlsInit;
        // ★ 默认仍是生产链路一直在用的 chrome_131（5000+ 号 0 封号背书）。
        //   协议注册链路会显式传 chrome_146 —— 那条链路的 UA 是 CriOS/150，
        //   131 与它差 19 个大版本，是 JA4 层面 100% 覆盖的不一致。见 src/tlsProfile.js。
        const clientId = this.clientIdentifier || rtClientIdentifier();
        const opts = { clientIdentifier: clientId, timeout: 30_000, followRedirects: false };
        if (this.proxyUrl) opts.proxy = this.proxyUrl;
        this._sess = new tls.Session(opts);
        return this._sess;
    }

    async close() {
        try { if (this._sess) await this._sess.close(); } catch (_) {}
        this._sess = null;
    }

    /**
     * 带重试的 POST（仅用于 sentinel/req —— 取 challenge，服务端无副作用，可安全重试）。
     * 识别口径与 rtPureProtocolClient 一致：连接被拒/重置/TLS/代理/EOF/超时 都算临时错误。
     */
    async _postWithRetry(sess, url, headers, body) {
        const TRANSIENT = /connection refused|connection reset|econnrefused|econnreset|tls|ssl|handshake|proxy|eof|timeout|timed out|socket hang up|no such host|dns/i;
        const maxAttempts = 3;
        let lastErr = null;
        for (let attempt = 1; attempt <= maxAttempts; attempt++) {
            this._ck();
            try {
                // ★★★ 2026-08-16：记下【请求发出】的时刻与往返耗时。
                //   这两个数是节奏闸门的锚点：浏览器实测的 sentinel→otpValidate /
                //   sentinel→createAccount 区间，量的就是「/sentinel/req 发出」到
                //   「消费请求发出」之间的墙钟时间。探针以前拿"spawn 子进程之前"当锚点，
                //   把 5~19 秒的 so-dwell 也算了进去 —— 而 dwell 跑在铸币【之前】、
                //   服务端根本读不到，于是 already 恒大于 target、闸门 17/17 全是「再等 0.0s」，
                //   等于从来没生效过。
                //   往返耗时此前【一行都没记】，所以连"锚点该取哪一刻"都无从判断。
                const _sentAt = Date.now();
                // 不设 headerOrder 时 tls-client 按字母序发头 —— 浏览器不会这样
                const r = await sess.post(url, {
                    headers, body, headerOrder: headerOrderFor(Object.keys(headers)),
                });
                this.lastReqSentAt = _sentAt;
                this.lastReqMs = Date.now() - _sentAt;
                if (this.onWire) this.onWire({
                    url, method: 'POST', headers, body,
                    resourceType: 'fetch', navigation: false,
                    status: r.status, responseHeaders: r.headers || {},
                });
                const text = await r.text().catch(() => '');
                // ★★★ 2026-08-16：node-tls-client 的传输层失败**不抛异常** —— 它 resolve 出一个
                //   status=0 的正常 Response，Go 侧的错误写在 body 里
                //   （`failed to do request: Post "...": EOF` / `... Client.Timeout exceeded ...`）。
                //   于是下面那个 catch 里的重试**一次都不会触发**，本函数顶上写的
                //   「sentinel/req 必须带网络重试」形同虚设：调用方 _fetchChallengeWithP 的
                //   `if (r.status !== 200) throw` 会把它当成一次业务失败直接判死。
                //   实测 2026-08-16 chumps.thrifty40@icloud.com：attempt 1 就是这么被
                //   一条 EOF 一击打死的（0 次重试），而救回它只需要一次 1.2 秒退避的重发。
                //   ⚠ 不要靠 body 文案去匹配 TRANSIENT —— body 可能为空。status===0 本身
                //     就是「传输没完成」的充分判据，直接打标记，让下面无条件当临时错误重试。
                if (r.status === 0) {
                    const err = new Error(`sentinel/req 传输失败(HTTP 0): ${String(text).slice(0, 200) || '(空响应体)'}`);
                    err.code = 'SENTINEL_REQ_TRANSPORT';
                    err.transient = true;
                    throw err;
                }
                return { r, text };
            } catch (e) {
                lastErr = e;
                const msg = String(e && e.message || e || '');
                // ★ 先短路"引擎没起来"：这不是网络问题，重试 3 次只是浪费 3.6s 并在日志里
                //   印三行误导性的"临时网络错误"。2026-08-16 排查协议并发全灭时被它坑惨 ——
                //   上面 TRANSIENT 里那个裸 `tls` 会命中 "Call ini(tTLS)() first."，
                //   于是一个永久性的致命错误被伪装成"网络抖一下"，15 行报错全长这个样子。
                //   ⚠ 不要改 TRANSIENT 里的 `tls` 为 \btls\b —— "tlsv1 alert internal error"
                //     这类真 TLS 错误会跟着不再匹配（s 和 v 之间没有词边界），那是收窄识别面。
                if (/not initialized/i.test(msg)) throw e;
                // e.transient 是上面 status===0 打的标记：传输层没走完，与 body 文案无关，一律可重试
                if ((!e || e.transient !== true) && !TRANSIENT.test(msg)) throw e;
                if (attempt >= maxAttempts) throw e;
                const backoff = 1200 * Math.pow(2, attempt - 1);
                this.log(`[sentinel] sentinel/req 临时网络错误(${msg.slice(0, 60)})，`
                    + `${backoff}ms 后重试 ${attempt + 1}/${maxAttempts}`);
                await new Promise(r => setTimeout(r, backoff));
            }
        }
        throw lastErr;
    }

    /**
     * ★★★ SDK 加载序列 —— 必须在第一次 POST /req 【之前】跑一次（2026-08-13 补）。
     *
     * 【为什么】
     *   实测 16 份 complete 抓包，浏览器在 sentinel.openai.com 上的请求是：
     *     GET /backend-api/sentinel/sdk.js        16/16  ← 100%
     *     GET /backend-api/sentinel/frame.html    16/16  ← 100%
     *     GET /sentinel/<sv>/sdk.js               13/16
     *     POST /backend-api/sentinel/req          15/16
     *   序列 14/16 完全一致：sdk → verSdk → frame → verSdk → REQ[×2~4]，
     *   而且【加载只发生一次】，后面多次 POST 复用同一个 iframe。
     *
     *   我们此前只发 POST —— 而那个 POST 的覆盖率（15/16）比两个 GET（16/16）还低。
     *
     * 【为什么这是最硬的一条】
     *   我们在 POST 的 Referer 里逐字写着 ".../backend-api/sentinel/frame.html?sv=..."，
     *   在 p[5] 里报 currentScript = sentinel 的 sdk.js，在 document.scripts 里也只放它 ——
     *   三处都声明"我是从那些资源来的"，而那些资源我们【一次都没取过】。
     *   这不是"少发了个请求"，是**主动发出一个能被自己流量证伪的断言**。
     *
     *   同一个域、同一个服务：我们的 POST 能 200，就证明中间没有任何拦截器。
     *   而 oai-did 这个 cookie 两边都带 ⇒ 服务端按 did 做一次 anti-join
     *   （有 POST 无 GET 的 did）成本极低。
     *
     * 【为什么能解释 T+10 分钟】
     *   POST /req 发生时账号【还不存在】，判出来也没有可吊销的对象，只能打标；
     *   要把标签落到账号上，必须等 create_account 产生 did→account 的绑定 ——
     *   那一步天然是事后 join，判定时刻由批次节奏决定。
     *
     * @param {string} pageUrl 宿主页 URL（做 referer）；不传用 flow 对应的默认页
     */
    async warmupSdk(pageUrl) {
        this._ck();
        const sess = await this._ensureSession();
        const referer = pageUrl || FLOW_PAGE_URL.authorize_continue;
        const sv = this.profile.sentinelSv;
        const frameUrl = `${SENTINEL_BASE}${SENTINEL_FRAME_PATH}?sv=${sv}`;
        const verSdkUrl = `${SENTINEL_BASE}/sentinel/${sv}/sdk.js`;

        // 抓包实测的必带头：script 类带 sec-ch-ua 三件套，document(iframe) 类带 uir 且不带 sec-ch-ua
        const base = {
            'user-agent': this.profile.userAgent,
            'accept-language': this.profile.acceptLanguage || 'ja-JP,ja;q=0.9',
            'accept-encoding': 'gzip, deflate, br, zstd',
        };
        // ★ 兜底值必须与人格一致。人格现在只有一个：Windows 桌面 Chrome。
        //   ⚠ 这里的兜底以前是 iPhone 的（?1 / "iPhone"）—— 那是 A 臂时代的残留。
        //     人格删到只剩 C 之后，一旦 profile 漏配就会发出
        //     「Windows Chrome UA + sec-ch-ua-mobile: ?1」这种自相矛盾的组合，
        //     所以兜底一并改成桌面值。
        const chUa = {
            'sec-ch-ua': this.profile.secChUa
                || '"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"',
            'sec-ch-ua-mobile': this.profile.secChUaMobile || '?0',
            'sec-ch-ua-platform': this.profile.secChUaPlatform || '"Windows"',
        };
        const cookie = this.cookieHeader ? { cookie: this.cookieHeader } : {};

        const scriptHeaders = ref => ({
            ...base, ...chUa, ...cookie, accept: '*/*', referer: ref,
            ...secFetchHeaders({ url: verSdkUrl, initiator: ref }),
            'sec-fetch-dest': 'script',     // 这几跳是 <script>，不是 XHR
            'sec-fetch-mode': 'no-cors',
            ...priorityHeader({ dest: 'script' }),   // script 是 u=1，不是 fetch 的 u=1, i
        });
        const frameHeaders = {
            ...base, ...cookie,
            accept: 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
            'upgrade-insecure-requests': '1', referer,
            ...secFetchHeaders({ url: frameUrl, initiator: referer, navigation: true }),
            'sec-fetch-dest': 'iframe',     // 它是 iframe 不是顶层 document
        };

        // 序列与抓包一致：sdk → verSdk → frame → verSdk
        const steps = [
            { url: `${SENTINEL_BASE}/backend-api/sentinel/sdk.js`, h: scriptHeaders(referer), gapMs: 0 },
            { url: verSdkUrl, h: scriptHeaders(referer), gapMs: 600 },
            { url: frameUrl, h: frameHeaders, gapMs: 500 },
            { url: verSdkUrl, h: scriptHeaders(frameUrl), gapMs: 300 },
        ];
        const got = [];
        for (const st of steps) {
            if (st.gapMs) await new Promise(r => setTimeout(r, st.gapMs));
            try {
                const r = await sess.get(st.url, {
                    headers: st.h,
                    headerOrder: headerOrderFor(Object.keys(st.h), { navigation: st.h['sec-fetch-dest'] === 'iframe' }),
                });
                // 内容我们不用（sdk.js 本地就有），要的是这条访问日志存在
                await r.text().catch(() => '');
                got.push(`${r.status}`);
                if (this.onWire) this.onWire({
                    url: st.url, method: 'GET', headers: st.h,
                    resourceType: st.h['sec-fetch-dest'] === 'iframe' ? 'document' : 'script',
                    navigation: false, status: r.status, responseHeaders: r.headers || {},
                });
            } catch (e) {
                // 取不到不该让整轮失败 —— 但要留痕，否则又变成静默缺失。
                // ★ 必须带 message：这一行是【最早的金丝雀】。2026-08-16 协议并发全灭那次，
                //   "SDK 加载序列 200/200/200/ERR" 在第一次 destroyTLS 之后 1.1 秒就出现了，
                //   比第一条明文报错早 89 秒 —— 而光一个 'ERR' 什么都说明不了，白白浪费了它。
                //   返回值两个调用方都丢弃，全仓也没有断言字面量 'ERR' 的测试，加字段是安全的。
                got.push('ERR:' + String((e && e.message) || e || '?').slice(0, 60));
            }
        }
        this.log(`[sentinel] SDK 加载序列 ${got.join('/')} （抓包 16/16 必有，我们此前 0 次）`);
        // 抓包实测 frame.html → 首个 POST /req 中位 777ms
        await new Promise(r => setTimeout(r, 600 + Math.random() * 500));
        return got;
    }

    /** ① 拿 challenge */
    async requestChallenge(flow) {
        this._ck();
        assertAssets();
        const sess = await this._ensureSession();
        const p = fp.generateRequirementsToken(this.sentinelSid, {
            ...this.profile,
            reactListeningKey: this.reactListeningKey,
            reactContainerKey: this.reactContainerKey,
            reactResourcesKey: this.reactResourcesKey,
        });
        return this._fetchChallengeWithP(p, flow);
    }

    /**
     * 用【指定的 p】去换 challenge。
     *
     * ★ 为什么要把 p 变成参数（2026-08-12）：
     *   服务端返回的三块 dx（turnstile.dx / so.collector_dx / so.snapshot_dx）是用
     *   【本次请求发上去的那个 p】做 XOR 密钥加密的。谁解密就必须拿同一个 p。
     *   IPC 模式下解密方是子进程里的真 SDK，它自己会算一个 p —— 所以换题必须用
     *   子进程那个 p，而不是这里现造的。见 sentinel/sentinel-runner.js 的 ipcMode。
     */
    /**
     * sentinel/req 的请求头。抽成纯函数是为了能单测 —— 这组头的形状是按 73 份
     * 真实抓包逐个校正过的，双向都不能漂（少发和多发一样是特征）。
     */
    _reqHeaders() {
        return {
            'user-agent': this.profile.userAgent,
            'accept': '*/*',
            'accept-language': this.profile.acceptLanguage || 'ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7',
            'content-type': 'text/plain;charset=UTF-8',
            'origin': SENTINEL_BASE,
            'referer': `${SENTINEL_BASE}/backend-api/sentinel/frame.html?sv=${this.profile.sentinelSv}`,
            // sentinel SDK 跑在 sentinel.openai.com 自己的 iframe 里，对 /sentinel/req
            // 是同源 XHR。走公共模块算，避免与探针那边的口径漂移。
            ...secFetchHeaders({
                url: SENTINEL_BASE + SENTINEL_REQ_PATH,
                initiator: `${SENTINEL_BASE}/backend-api/sentinel/frame.html`,
            }),
            // ★ 2026-08-12 按 73/73 份抓包校正 sentinel/req 的头（双向都错过）：
            //   · 浏览器【必带】sec-ch-ua 三件套 —— 我们此前一个都不发
            //   · 浏览器【从不】发 oai-device-id / oai-language —— 我们此前每次都发
            //     （deviceId 本来就在 body 里，这两个头纯属多余，且是 0/73 的信号）
            //   sec-ch-ua 的版本必须与 UA 的大版本一致，所以取自画像；
            //   兜底值同样是桌面 Chrome 的（?0 / "Windows"），见上一处同样的理由。
            'sec-ch-ua': this.profile.secChUa
                || '"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"',
            'sec-ch-ua-mobile': this.profile.secChUaMobile || '?0',
            'sec-ch-ua-platform': this.profile.secChUaPlatform || '"Windows"',
            ...(this.cookieHeader ? { cookie: this.cookieHeader } : {}),
        };
    }

    async _fetchChallengeWithP(p, flow) {
        this._ck();
        const sess = await this._ensureSession();
        const body = fp.buildSentinelRequestBody(p, this.deviceId, flow);
        const headers = this._reqHeaders();

        const url = SENTINEL_BASE + SENTINEL_REQ_PATH;
        // ★sentinel/req 必须带网络重试：
        //   它在一次授权里至少打两次（提交邮箱一次、验证邮箱码一次），是高频调用点，
        //   而 `HTTP 0 ... EOF` 这类瞬时断连一击不中就会让整个任务判死
        //   （实测 firebug_raspy.2y@icloud.com 就是这么在第 1 次尝试就 blocked 的）。
        //   这里是**纯取 challenge**、服务端无副作用 ⇒ 可以放心重试任何临时错误。
        const { r, text } = await this._postWithRetry(sess, url, headers, body);
        if (r.status !== 200) {
            throw codedError('SENTINEL_REQ_FAILED',
                `sentinel/req HTTP ${r.status}: ${String(text).slice(0, 300)}`);
        }
        let challenge;
        try { challenge = JSON.parse(text); } catch (e) {
            throw codedError('SENTINEL_REQ_BAD_JSON', `sentinel/req 返回非 JSON: ${String(text).slice(0, 200)}`);
        }
        const requires = [];
        if (challenge.turnstile && challenge.turnstile.required) requires.push('turnstile');
        if (challenge.so && challenge.so.required) requires.push('so');
        if (challenge.proofofwork && challenge.proofofwork.required) requires.push('pow');
        this.log(`[sentinel] challenge 就绪 flow=${flow} persona=${challenge.persona || '?'} 要求=${requires.join(',') || '无'}`
            + (challenge.proofofwork && challenge.proofofwork.required
                ? ` difficulty=${challenge.proofofwork.difficulty}` : '')
            // ★ 往返耗时必须打出来：节奏闸门的锚点是「请求发出」那一刻，而这行日志
            //   以前打在【响应到达之后】，两者之间差整整一个往返却没有任何记录 ——
            //   于是"真实陈旧度到底多少"在日志上永远算不出来。
            + ` req=${this.lastReqMs != null ? this.lastReqMs + 'ms' : '?'}`);
        return challenge;
    }

    /** ② 交给 Node vm 里的真 sdk.js 算最终 token */
    async solveChallenge(challenge, flow) {
        this._ck();
        assertAssets();
        // ★ IPC 模式（this.ipcHandshake）：不预取题、不落临时文件。
        //   子进程里的真 SDK 现算 p，通过 IPC 交回来，由这里用【同一个带代理和
        //   TLS 指纹的会话】去换题 —— 保证加密 dx 的 p 与解密 dx 的 p 是同一个。
        //   非 IPC 模式保持原样：父进程预取的题 + --challenge-file。
        const useIpc = this.ipcHandshake === true;
        // IPC 模式下题是子进程触发拿回来的，这里留个引用给后面的 c 字段兜底与返回值用
        let ipcChallenge = null;
        const tmpFile = useIpc ? '' : path.join(os.tmpdir(),
            `sentinel-challenge-${flow}-${crypto.randomUUID()}.json`);
        if (!useIpc) fs.writeFileSync(tmpFile, JSON.stringify(challenge), 'utf8');

        const p = this.profile;
        // ★ 一次取、两处用（--sdk 与 --script-src）。见下面那两行的注释。
        const _sdk = activeSdk();
        const args = [
            RUNNER_PATH,
            ...(useIpc ? ['--ipc'] : ['--challenge-file', tmpFile]),
            // ★ so token 只在 IPC 模式下有意义：它的两块料同样用请求时那个 p 加密，
            //   p 不配对时 sdk 根本不激活 collector，要来也是 null。
            ...(useIpc ? ['--emit-so'] : []),
            '--flow', flow,
            '--device-id', this.deviceId,
            '--sentinel-sid', this.sentinelSid,
            '--react-listening-key', this.reactListeningKey,
            '--react-container-key', this.reactContainerKey,
            '--react-resources-key', this.reactResourcesKey,
            '--page-url', FLOW_PAGE_URL[flow] || FLOW_PAGE_URL.authorize_continue,
            '--user-agent', p.userAgent,
            '--browser-family', p.browserFamily || 'chrome',
            '--navigator-platform', p.navigatorPlatform || 'MacIntel',
            '--navigator-vendor', p.navigatorVendor || 'Google Inc.',
            '--user-agent-data-platform', p.userAgentDataPlatform || 'macOS',
            '--request-idle-callback', '0',
            // ★★★ 这两行【必须来自同一次 activeSdk() 调用】——
            //   --sdk 是实际跑的代码，--script-src 是报给服务端的 p[5]。
            //   分别去两处取 = 迟早报 A 版跑 B 版。见文件头 2026-08-25 那段。
            '--sdk', _sdk.path,
            '--script-src', `https://sentinel.openai.com/sentinel/${_sdk.sv}/sdk.js`,
            // auth 页面的 documentElement 通常没有 data-build，保持空串
            '--build-id', p.buildId || '',
            // ★ 同一次注册的多次调用必须共用一个"页面打开时刻"，否则两份 p 会报出
            //   两个不同的 timeOrigin —— 线上实测同一会话的 3 份 p 是同一个值。
            ...(this.pageOpenAt ? ['--page-open-at', String(this.pageOpenAt)] : []),
            // ★★ 2026-08-15：会话停留时长。不传的话 runner 默认 0，
            //   采集器只活 11 毫秒、so 的 7 个行为槽全是 0 ——
            //   服务端单看一个 so token 就知道背后没有真实页面会话。
            //   取值按【浏览器实测的 so 槽0（采集器存活毫秒数）】分 flow 采样：
            //     email-otp/validate  n=11  中位 7359   区间 3873~20733
            //     create_account      n= 7  中位 17990  区间 13222~18981
            //   两者差异是有道理的：验证码页只要粘一下码，about-you 页要打姓名。
            '--so-dwell-ms', String(this._dwellMsFor(flow)),
            '--width', String(p.screenWidth),
            '--height', String(p.screenHeight),
            '--cores', String(p.hardwareConcurrency),
            '--language', p.navigatorLanguage,
            '--languages', p.navigatorLanguages.join(','),
            '--time-zone', p.timezoneIana,
            '--timezone-name', p.timezoneName,
            '--timezone-offset-minutes', String(p.timezoneOffsetMinutes),
            // ★ 值为 null 就【整个参数不传】，不要传 String(null)="null" ——
            //   那会让 runner 解析出 NaN，比不传更糟。
            //   （这条判据是 2026-08-16 为当时的 iOS Safari 人格加的；那个人格已删，
            //     但"宁可不传也不要传 null 字符串"这条本身仍然成立，保留。）
            ...(p.jsHeapSizeLimit == null ? [] : ['--js-heap-size-limit', String(p.jsHeapSizeLimit)]),
            ...(p.deviceMemory == null ? [] : ['--device-memory', String(p.deviceMemory)]),
            '--device-pixel-ratio', String(p.devicePixelRatio),
            // ⚠ 兜底值必须与人格一致（Windows 桌面 Chrome）。原来是 149 / "macOS"，
            //   那是旧人格的残留 —— 漏配时会发出"Chrome 150 UA + macOS 平台"的矛盾。
            '--chrome-major', p.chromeMajor || '150',
            '--chrome-full-version', p.chromeFullVersion || '150.0.7871.187',
            '--sec-ch-ua', p.secChUa || '"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"',
            '--sec-ch-ua-platform', p.secChUaPlatform || '"Windows"',
            '--cookie', this.cookieHeader || `oai-did=${this.deviceId}`,
        ];

        const env = { ...process.env };
        delete env.SENTINEL_CONFIG;
        env.SENTINEL_CONFIG = '__none__';   // 故意指向不存在的文件，跳过自动发现
        env.TZ = p.timezoneIana;            // 让 VM 里的 Date.toString() 与 p[1] 时区一致

        const rawOut = await new Promise((resolve, reject) => {
            const child = spawn(this.nodeExecutable, args, {
                cwd: PROJECT_ROOT, env, windowsHide: true,
                // ★ ipc 模式要第四个 fd 当消息通道；stdio 前三个保持默认的 pipe 行为
                ...(useIpc ? { stdio: ['ignore', 'pipe', 'pipe', 'ipc'] } : {}),
            });
            if (useIpc) {
                child.on('message', async msg => {
                    if (!msg || msg.type !== 'need-challenge') return;
                    try {
                        const ch = await this._fetchChallengeWithP(msg.p, msg.flow || flow);
                        ipcChallenge = ch;
                        child.send({ type: 'challenge', challenge: ch });
                    } catch (e) {
                        // 不在这里 reject —— 让子进程自己超时退出，错误信息统一从 stderr 出，
                        // 避免 promise 双重 settle。
                        try { child.send({ type: 'challenge', error: String(e && e.message || e) }); } catch (_) {}
                    }
                });
            }
            let stdout = '';
            let stderr = '';
            const timer = setTimeout(() => {
                try { child.kill('SIGKILL'); } catch (_) {}
                reject(codedError('SENTINEL_RUNNER_TIMEOUT',
                    `sentinel-runner.js 超时(>${RUNNER_TIMEOUT_MS}ms) flow=${flow}`));
            }, RUNNER_TIMEOUT_MS);
            child.stdout.on('data', d => { stdout += d.toString(); });
            child.stderr.on('data', d => {
                const s = d.toString();
                stderr += s;
                // --debug-so 的槽位明文只在 stderr，而 stderr 平时只在出错时才露出来。
                // 只有显式开了调试才转发，正常注册一行都不多打。
                if (process.env.SENTINEL_DEBUG_SO === '1') {
                    for (const line of s.split('\n')) {
                        if (line.startsWith('[so-plain]')) process.stderr.write(`${line}\n`);
                    }
                }
                // --debug-fp 同理：只在显式开启时转发「sdk 读了哪些指纹面」
                if (process.env.SENTINEL_DEBUG_FP === '1') {
                    for (const line of s.split('\n')) {
                        if (line.startsWith('[fp]')) process.stderr.write(`${line}\n`);
                    }
                }
                // --dump-surface 的枚举结果同理。runner 是【孙进程】——它的 stderr 到这里就
                // 被收进变量了，不转发的话外层什么都看不到（第一次跑 fp_diff 就栽在这）。
                if (process.env.SENTINEL_DUMP_SURFACE) {
                    for (const line of s.split('\n')) {
                        if (line.startsWith('[surface]')) process.stderr.write(`${line}\n`);
                    }
                }
                // --trace-reads 同理（runner 是孙进程，不转发外层看不到）
                if (process.env.SENTINEL_TRACE_READS === '1') {
                    for (const line of s.split('\n')) {
                        if (line.startsWith('[read]')) process.stderr.write(`${line}\n`);
                    }
                }
            });
            child.on('error', err => {
                clearTimeout(timer);
                reject(codedError('SENTINEL_RUNNER_SPAWN_FAILED',
                    `无法启动 node（${this.nodeExecutable}）：${err.message}`, err));
            });
            child.on('close', code => {
                clearTimeout(timer);
                if (code !== 0) {
                    reject(codedError('SENTINEL_RUNNER_FAILED',
                        `sentinel-runner.js 退出码 ${code}\nstderr: ${stderr.slice(0, 800)}`));
                    return;
                }
                resolve(stdout.trim());
            });
        }).finally(() => {
            if (tmpFile) { try { fs.unlinkSync(tmpFile); } catch (_) {} }
        });

        if (!rawOut) {
            throw codedError('SENTINEL_RUNNER_EMPTY', 'sentinel-runner.js 输出为空');
        }
        // ★ --emit-so 模式下 runner 输出的是信封 {sentinelToken, soToken}；
        //   否则就是裸 token 文本。这里统一拆成 tokenText + sdkSoToken。
        let tokenText = rawOut;
        let sdkSoToken = null;
        if (useIpc) {
            let envelope;
            try { envelope = JSON.parse(rawOut); } catch (e) {
                throw codedError('SENTINEL_RUNNER_BAD_JSON',
                    `runner 信封不是合法 JSON: ${rawOut.slice(0, 200)}`);
            }
            if (!envelope || typeof envelope.sentinelToken !== 'string') {
                throw codedError('SENTINEL_RUNNER_BAD_JSON',
                    `runner 信封缺 sentinelToken: ${rawOut.slice(0, 200)}`);
            }
            tokenText = envelope.sentinelToken;
            sdkSoToken = typeof envelope.soToken === 'string' && envelope.soToken
                ? envelope.soToken : null;
        }
        let parsed;
        try { parsed = JSON.parse(tokenText); } catch (e) {
            throw codedError('SENTINEL_RUNNER_BAD_JSON',
                `runner 输出不是合法 JSON: ${tokenText.slice(0, 200)}`);
        }
        for (const key of ['p', 'c', 'id', 'flow']) {
            if (!(key in parsed)) {
                throw codedError('SENTINEL_RUNNER_INCOMPLETE',
                    `runner 输出缺字段 ${key}: ${tokenText.slice(0, 200)}`);
            }
        }
        // so 字段。优先用 sdk 的 sessionObserverToken() 原样输出 —— 它内部的 ce(t,n)
        // 已经拼好 {so, c, id, flow}，形状与真浏览器逐字一致，不需要我们再造一遍。
        // 退路（parsed.so）保留给非 IPC 模式：那条路 sdk 不会产 so，几乎恒为空。
        // 缺失时不发 so-token 头 —— 与浏览器在 so 未激活时的行为一致。
        let soToken = sdkSoToken;
        if (!soToken && parsed.so) {
            soToken = JSON.stringify({
                so: parsed.so,
                c: parsed.c || ((challenge || ipcChallenge) || {}).token || '',
                id: this.deviceId,
                flow,
            });
        }
        const summary = Object.fromEntries(Object.entries(parsed)
            .map(([k, v]) => [k, typeof v === 'string' ? v.length : typeof v]));
        // ⚠ 不要退回 `!!parsed.t` —— 假 token 也是非空字符串，那个判断恒为真，
        //   正是它让「发了三个月报错串」这件事一直没被发现。
        const turnstileBroken = isBrokenTurnstile(parsed.t);
        const turnstileState = !parsed.t ? '无' : (turnstileBroken ? '假(sdk报错串)' : '真');
        this.log(`[sentinel] token 生成 flow=${flow} 字段=${JSON.stringify(summary)} `
            + `turnstile=${turnstileState} so=${!!soToken}`);
        if (turnstileBroken) {
            this.log('[sentinel] ⚠ turnstile 是 sdk 内部报错的 base64，不是解题结果。'
                + '成因是换题的 p 与解题的 p 不配对 —— 开 ipcHandshake（或 SENTINEL_IPC_HANDSHAKE=1）可修。');
        }
        return { sentinelToken: tokenText, soToken, parsed, challenge: challenge || ipcChallenge };
    }

    /** 一步到位：拿 challenge + 算 token */
    async token(flow) {
        // ★ IPC 模式下【不能】在这里预取题：那会多打一次 sentinel/req（多一次可观测的
        //   网络请求），而且取回来的题用的是一个即将被丢弃的 p，毫无用处。
        //   真正的换题在 solveChallenge 里、由子进程的 p 触发。
        const challenge = this.ipcHandshake ? null : await this.requestChallenge(flow);
        const result = await this.solveChallenge(challenge, flow);
        return { ...result, challenge: result.challenge || challenge };
    }
}

SentinelClient._tlsInit = null;

/**
 * 探测线上当前的 Sentinel SDK 版本。
 *
 * ★★★ 2026-08-15 重写。旧实现是【永远报绿灯的假自检】，实测确认：
 *
 *   旧做法：GET frame.html **不带 sv**，读回填的版本，和本地比。
 *   旧注释断言「sv 留空 → 服务端回填线上当版」。这句话是错的 ——
 *   实测 sv 是【调用方点的菜】，服务端你要哪版给哪版：
 *       sv=(不传)         -> 回 20260219f9f6      ← 只是服务端的默认值
 *       sv=20260219f9f6   -> 回 20260219f9f6
 *       sv=20260810913b   -> 回 20260810913b      ← 新版活得好好的
 *   而服务端默认值恰好 == 我们本地钉的版本，于是这段代码等价于
 *   「拿本地和本地比」，match 恒为 true。真出事时它一声不吭。
 *
 *   新做法：GET /backend-api/sentinel/sdk.js（**不带版本号的那个**）。
 *   它返回 923 字节的【加载器】脚本，浏览器就是靠它知道该去取哪一版的 ——
 *   抓包实证（registration-flow-ea8f7f18…）：
 *       seq634 request   /backend-api/sentinel/sdk.js      ← 先要加载器
 *       seq691 response  HTTP 200
 *       seq693 request   /sentinel/20260810913b/sdk.js     ← 再按它说的取版本
 *   版本号就明文写在加载器里，这才是「浏览器当前用哪版」的权威来源。
 *
 *   ⚠ 这个 URL 我们在 warmSentinelAssets() 里【本来就在请求】，只是把返回体
 *     扔了（当时注释写「内容我们不用」）。现在同一条请求顺带把版本读出来。
 *
 * @returns {{liveSv:string, localSv:string, match:boolean, error?:string}}
 */
async function detectLiveSdkVersion(opts = {}) {
    const localSv = String(opts.localSv || fp.DEFAULT_PROFILE.sentinelSv);
    try {
        const tls = requireTlsClient();
        if (!SentinelClient._tlsInit) SentinelClient._tlsInit = tls.initTLS();
        await SentinelClient._tlsInit;
        const sessOpts = { clientIdentifier: rtClientIdentifier(), timeout: 20000, followRedirects: true };
        if (opts.proxyUrl) sessOpts.proxy = opts.proxyUrl;
        const s = new tls.Session(sessOpts);
        try {
            const probeHeaders = {
                'user-agent': opts.userAgent || fp.DEFAULT_PROFILE.userAgent,
                accept: '*/*',
                referer: 'https://auth.openai.com/',
                'sec-fetch-dest': 'script',
                'sec-fetch-mode': 'no-cors',
                'sec-fetch-site': 'cross-site',
            };
            const r = await s.get(`${SENTINEL_BASE}/backend-api/sentinel/sdk.js`, {
                headers: probeHeaders,
                // ★ 字母序会被 CF 挑战（docs/协议注册模式.md:226-233 的 A/B）
                headerOrder: headerOrderFor(Object.keys(probeHeaders)),
            });
            const body = await r.text();
            // 加载器里出现的 sentinel/<sv>/sdk.js；没有这个形态时退回裸版本串
            const m = /sentinel\/([0-9a-f]{8,24})\/sdk\.js/i.exec(body)
                || /\b(20\d{6}[0-9a-f]{4})\b/.exec(body);
            const liveSv = m ? m[1] : '';
            return { liveSv, localSv, match: !!liveSv && liveSv === localSv, httpStatus: r.status };
        } finally {
            try { await s.close(); } catch (_) {}
        }
    } catch (e) {
        return { liveSv: '', localSv, match: false, error: String(e.message).slice(0, 160) };
    }
}

module.exports = {
    isBrokenTurnstile,
    detectLiveSdkVersion,
    FLOW_PAGE_URL,
    RUNNER_PATH,
    // ★ 2026-08-25：SDK_PATH 从模块级常量改成 getter —— 跟版是运行时事件，
    //   常量会把整个进程钉在启动那一刻的版本上。保留这个导出名是为了不破坏既有引用。
    get SDK_PATH() { return activeSdk().path; },
    SENTINEL_DIR,
    SentinelClient,
    assertAssets,
};
