// ── 纯协议 RT 客户端(B2 架构)──
// 浏览器只 bootstrap 到 add_phone(过 sentinel + 登录)→交 cookie+UA；之后"绑手机→手机OTP→选workspace→跟external_url拿code"全走 HTTP。
// ★HTTP 传输用 node-tls-client 的 JA3 指纹——纯 axios 会被 Cloudflare「Just a moment」403 挡下(TLS 指纹被识破)；
//   装哪个画像见 tlsProfile.js（2026-09-26 实测：chrome_131 已经过不去了，0/6 全被挑战）。cookie 用 tough-cookie 手管(set Cookie 头 + 吸 set-cookie)。maxRedirects=0 手动跟 302/303 截 code。
// 端点/字段来自 2026-07-19/20 真流量抓取。
const { CookieJar } = require('tough-cookie');
const { rtClientIdentifier } = require('./tlsProfile');
const { requireTlsClient, hardenCookieJar } = require('./tlsClientBootstrap');   // Go 引擎本地固化 + 缺失时抛异常（而非 process.exit）
// ★ 2026-08-14：把协议注册那一轮验证过的两件事接到【生产 RT】上。
//   ① headerOrder —— 在这之前本文件一处都没设，于是 tls-client 按【字母序】发头。
//      字母序是 Go/Python HTTP 库的典型特征（无序 map 排序输出），浏览器从不这样；
//      当年本地起 h2 服务器读线序、发现字母序会被 Cloudflare 403 的，就是这个。
//      生产日志实测：纯协议 RT 783 次建会话里 123 次 CLOUDFLARE_BLOCK 撞在 authorize，
//      另有 135 次 authorize 拿不到 oai-client-auth-session —— 合计约 33% 要换出口重来。
//   ② sec-fetch 三件套 + priority —— 抓包里 2005/2005 = 100% 覆盖，且由浏览器网络栈
//      生成、扩展和拦截器都改不掉，缺了没有任何良性解释。本文件此前一个都不发。
//   ⚠ 刻意【不】动的两样，理由写在这里免得日后有人"顺手补齐"：
//      · TLS 仍 chrome_131 —— 本文件的 UA 也是 Chrome/131，两者一致；131 有 5000+ 号 0 封号背书。
//      · sentinel 仍不开 ipcHandshake —— 生产 RT 已用旧行为过了 390 次 sentinel 门且账号存活，
//        把一个在working的反爬信号换成"从没发过的形状"，风险实打实、收益未证实。
//        要灰度用环境变量 SENTINEL_IPC_HANDSHAKE=1，不要改这里的默认值。
const { headerOrderFor } = require('./chromeHeaderOrder');
const { secFetchHeaders, priorityHeader } = require('./secFetch');

const AUTH_BASE = 'https://auth.openai.com';
let _tlsInit = null;
async function ensureTls(tls) { if (!_tlsInit) _tlsInit = tls.initTLS(); return _tlsInit; }

// ★每个 /api/accounts/* 端点在真浏览器里都有**专属 referer**（就是发起它的那个页面）。
//   2026-08-03 抓包实录（logs/authflow/）：
//     authorize/continue → /log-in          password/verify    → /log-in/password
//     email-otp/validate → /email-verification
//     add-phone/send     → /add-phone       phone-otp/validate → /phone-verification
//     workspace/select   → /sign-in-with-chatgpt/codex/consent
//   我们原来一律发通用的 AUTH_BASE + '/'。其中 add-phone/send 是**唯一**走裸 _post 的端点
//   （其余四个走 _postWithSentinel，会显式传 referer）—— 而它恰好就是回 fraud_guard 的那个。
const API_REFERER = [
    [/\/api\/accounts\/authorize\/continue/, AUTH_BASE + '/log-in'],
    [/\/api\/accounts\/password\/verify/, AUTH_BASE + '/log-in/password'],
    [/\/api\/accounts\/email-otp\//, AUTH_BASE + '/email-verification'],
    [/\/api\/accounts\/add-phone\//, AUTH_BASE + '/add-phone'],
    [/\/api\/accounts\/phone-otp\//, AUTH_BASE + '/phone-verification'],
    [/\/api\/accounts\/workspace\/select/, AUTH_BASE + '/sign-in-with-chatgpt/codex/consent'],
    // 2026-09-16 全流程抓包（真人「注册→开2FA→加密码」）补登记：
    //   mfa/issue_challenge → /email-verification（就是刚验完邮箱码的那个页面发起的）
    //   password/add        → /reset-password/new-password
    // ★ mfa/verify 的 referer 是 /mfa-challenge/<factor_id>，**带动态段**，登记不进这张静态表 ——
    //   由调用方显式传（见 rtPureProtocolClient.verifyMfaTotp）。这里**故意不登记**它的静态前缀：
    //   登记了反而会在调用方忘记传 referer 时，悄悄发出一个少了 factor_id 的、真浏览器发不出的 referer。
    [/\/api\/accounts\/mfa\/issue_challenge/, AUTH_BASE + '/email-verification'],
    [/\/api\/accounts\/password\/add/, AUTH_BASE + '/reset-password/new-password'],
];

// OpenAI 自家 web app 发往 /api/accounts/* 的每个请求都带 x-access-flow-invocation-id，
// 每次一个全新 UUID（抓包里 6 个请求 6 个不同值，不是会话级常量）。缺这个头 = 明摆着不是从他们页面发出来的。
function apiCallHeaders(path) {
    const hit = API_REFERER.find(([re]) => re.test(path));
    return {
        'x-access-flow-invocation-id': require('crypto').randomUUID(),
        ...(hit ? { referer: hit[1] } : {}),
    };
}

// 号码国家标签：**只为日志可读性服务**，不参与任何判断。
// 用途很具体：面板上一眼看出「我配的是印度，实际买到的是不是印度号」——
// 2026-08-10 那次就是配置从印度改回美国之后号才通的，而日志里只有一串数字，人得自己心算前缀。
// ★只对**已登记**的前缀做最长匹配，认不出就如实标「未登记」，绝不猜：
//   +1 是整个北美编号计划（除美加外还有 +1246 巴巴多斯、+1876 牙买加等二十多个国家/地区），
//   所以它只敢标成「美加/NANP」而不是「美国」。宁可少说，也不要给人一个错的国家去排查。
const DIAL_LABELS = [
    ['1', '美加/NANP'], ['7', '俄/哈'], ['33', '法国'], ['44', '英国'], ['49', '德国'],
    ['55', '巴西'], ['57', '哥伦比亚'], ['60', '马来'], ['62', '印尼'], ['63', '菲律宾'],
    ['66', '泰国'], ['81', '日本'], ['82', '韩国'], ['84', '越南'], ['91', '印度'],
    ['92', '巴基斯坦'], ['234', '尼日利亚'], ['880', '孟加拉'], ['998', '乌兹别克'],
];
function dialLabel(phone) {
    const digits = String(phone || '').replace(/[^0-9]/g, '');
    if (!digits) return '未知号码';
    for (let len = 3; len >= 1; len--) {
        const hit = DIAL_LABELS.find(([code]) => code === digits.slice(0, len));
        if (hit) return hit[1];
    }
    return `+${digits.slice(0, 3)} 未登记`;
}

class RtProtocolClient {
    constructor(opts = {}) {
        this.jar = new CookieJar();
        this.log = typeof opts.log === 'function' ? opts.log : (() => {});
        // 兜底 UA 必须与实际装的 clientIdentifier 同版本（见 tlsProfile.js）。
        // 2026-09-26：TLS 从 chrome_131 换成 chrome_117（131 已经 0/6 全被 Cloudflare 挑战），
        //   这里的兜底 UA 跟着落到 117 —— 我们自己控制得了的那一层要说同一件事。
        // ★ 纯协议 RT 那条路的 UA 不走这里：rtPureProtocolTask 建 client 时不传 userAgent，
        //   取的是 sentinelFingerprint.js 里那个**写死的 macOS Chrome/131** 串（全库所有账号逐字相同）。
        //   实测 TLS117 + UA131 与 TLS117 + UA117 都是 6/6，UA 不影响过不过，所以没动它。
        //   ⚠ 但那条路因此有「TLS 说 117、UA 说 131、平台说 macOS 而这里的兜底说 Windows」三重不一致，
        //     属于**已知未补**，不是已经对齐了 —— 别看到这行就以为那边也处理过了。
        // B2 链路正常会被 importBrowserCookies(cookies, userAgent) 覆盖成真浏览器的 UA，这里只是兜底。
        this.userAgent = opts.userAgent || 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36';
        this.redirectUri = opts.redirectUri || 'http://localhost:1455/auth/callback';
        this.abortCheck = typeof opts.abortCheck === 'function' ? opts.abortCheck : () => false;
        // 想把 RT 绑到【指定】工作空间时传它（Team 空间的 account_id）。
        // 不传＝保持原行为：取服务端返回的 workspaces[0]。
        // 为什么需要这个：一个号同时属于个人空间和 Team 空间时，workspaces[] 的顺序
        // 由服务端决定，写死 [0] 就没法把令牌绑到我们要的那个空间。
        // 协议本来就吃显式 workspace_id（见 selectWorkspaceAndGetCode 里的 POST body），
        // 缺的只是把目标送进来的入口。
        this.targetWorkspaceId = String(opts.targetWorkspaceId || '').trim();
        this.lastSession = null;
        // node-tls-client 代理走 URL 串；socks5 对象 → socks5://；本机直连=null
        this.proxyUrl = null;
        if (opts.proxyUrl) this.proxyUrl = opts.proxyUrl;
        else if (opts.socks5 && opts.socks5.host) {
            const p = opts.socks5;
            const auth = p.username ? `${encodeURIComponent(p.username)}:${encodeURIComponent(p.password || '')}@` : '';
            this.proxyUrl = `socks5://${auth}${p.host}:${p.port}`;
        }
        this._sess = null;
    }

    _ck() { if (this.abortCheck()) throw new Error('ABORTED: 已急停/取消'); }

    async _ensureSession() {
        if (this._sess) return this._sess;
        const tls = requireTlsClient();   // ★不用裸 require：引擎缺失时 initTLS() 内部会 process.exit(1)
        await ensureTls(tls);
        const opts = { clientIdentifier: rtClientIdentifier(), timeout: 30000, followRedirects: false };
        if (this.proxyUrl) opts.proxy = this.proxyUrl;
        // Apply the existing prefix-cookie guard before the first login request.
        this._sess = hardenCookieJar(new tls.Session(opts));
        // ★ 日志要说出**实际**装的是哪个画像：写死一个名字的话，将来换了画像这行还在说旧的，
        //   而排查时最先看的就是它（2026-09-26 那次就是靠它对上号的）。
        this.log(`[proto] TLS 会话就绪(${rtClientIdentifier()}${this.proxyUrl ? ' via proxy' : ' 本机直连'})`);
        return this._sess;
    }

    async close() { try { if (this._sess) await this._sess.close(); } catch (e) {} this._sess = null; }

    async importBrowserCookies(cookies, userAgent) {
        if (userAgent) this.userAgent = userAgent;
        let n = 0;
        for (const c of cookies || []) {
            const domain = String(c.domain || '').replace(/^\./, '');
            if (!domain) continue;
            const url = `https://${domain}${c.path || '/'}`;
            const parts = [`${c.name}=${c.value}`, `Path=${c.path || '/'}`, `Domain=${c.domain}`];
            if (c.secure) parts.push('Secure');
            try { await this.jar.setCookie(parts.join('; '), url, { ignoreError: true }); n++; } catch (e) {}
        }
        this.log(`[proto] 已导入 ${n} 个 cookie，UA=${this.userAgent.slice(0, 42)}…`);
    }

    // ★sec-ch-ua 三件套：Chrome 89 起**每个请求都带**。只发 user-agent 却一个 client hint 都不发，
    //   是最容易被识别的自动化特征之一。版本号从 UA 里取，保证 UA / client hints / TLS 指纹三层说同一件事。
    //   （2026-08-03 抓包对照：真浏览器发往 /api/accounts/* 的 6 个请求全都带这三个头。）
    _clientHints() {
        const m = /Chrome\/(\d+)/.exec(this.userAgent || '');
        const v = m ? m[1] : '131';
        const ua = String(this.userAgent || '');
        const platform = /Windows/i.test(ua) ? '"Windows"'
            : /Macintosh|Mac OS X/i.test(ua) ? '"macOS"'
                : /Linux|X11/i.test(ua) ? '"Linux"' : '"Windows"';
        return {
            'sec-ch-ua': `"Google Chrome";v="${v}", "Chromium";v="${v}", "Not_A Brand";v="24"`,
            'sec-ch-ua-mobile': '?0',
            'sec-ch-ua-platform': platform,
        };
    }

    async _headers(extra) {
        const cookie = await this.jar.getCookieString(AUTH_BASE);
        return {
            'user-agent': this.userAgent,
            'accept': 'application/json, text/plain, */*',
            // ★ 必须显式发（2026-08-14 起 h2 服务器读线上字节实测）：
            //   不发的话 Go 引擎会自己补一个，代价有两处 ——
            //   ① 值是 `gzip, deflate, br`，**少了 zstd**；Chrome 从 123 起一直发 zstd，
            //      我们的 UA 报 131，却发一个 Chrome 122 时代的 accept-encoding。
            //   ② 引擎补的头一律排在【最末】，而 Chrome 把它放在 referer 之后、
            //      accept-language 之前 —— headerOrder 管不到我们没发的头。
            //   已实测 tls-client 对 gzip / br / zstd 三种响应都能正常解压，显式发不会收到乱码。
            'accept-encoding': 'gzip, deflate, br, zstd',
            'accept-language': this.acceptLanguage || 'ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7',
            ...this._clientHints(),
            'origin': AUTH_BASE,
            'referer': AUTH_BASE + '/',
            ...(cookie ? { cookie } : {}),
            ...(extra || {}),
        };
    }

    async _absorb(r, url) {
        const sc = r && r.headers && (r.headers['set-cookie'] || r.headers['Set-Cookie']);
        const arr = Array.isArray(sc) ? sc : (sc ? [sc] : []);
        for (const c of arr) { try { await this.jar.setCookie(c, url, { ignoreError: true }); } catch (e) {} }
    }

    _isCf(status, body) {
        return (status === 403 || status === 503 || status === 429 || status === 401)
            && /just a moment|challenge-platform|cf-chl|cloudflare|enable javascript|verifying you are human|attention required/i.test(String(body || ''));
    }

    /**
     * @param {object} [extraHeaders] 追加/覆盖头。展开在 apiCallHeaders 之**后**，
     *   所以调用方能用它覆盖 API_REFERER 表里的 referer —— 给那些 referer 带动态段、
     *   登记不进静态表的端点用（例如 mfa/verify 的 /mfa-challenge/<factor_id>）。
     *   不传时行为与原来逐字相同。
     */
    async _post(path, bodyObj, extraHeaders) {
        this._ck();
        const sess = await this._ensureSession();
        const url = AUTH_BASE + path;
        const headers = await this._headers({
            'content-type': 'application/json',
            'accept': 'application/json',
            ...apiCallHeaders(path),
            ...(extraHeaders || {}),
            // auth.openai.com 的 JSON 接口，发起方是 auth.openai.com 自己的页面 ⇒ 同源 XHR
            ...secFetchHeaders({ url, initiator: AUTH_BASE + '/' }),
            ...priorityHeader({}),
        });
        const r = await sess.post(url, { headers, body: JSON.stringify(bodyObj), headerOrder: headerOrderFor(Object.keys(headers)) });
        const text = await r.text().catch(() => '');
        await this._absorb(r, url);
        if (this._isCf(r.status, text)) throw new Error(`CLOUDFLARE_BLOCK: ${path} 被 Cloudflare 挑战(HTTP ${r.status})——TLS指纹/IP 需调`);
        let data = null; try { data = JSON.parse(text); } catch (e) {}
        if (data && data['oai-client-auth-session']) this.lastSession = data['oai-client-auth-session'];
        return { status: r.status, data, text };
    }

    async _getRaw(url) {
        this._ck();
        const sess = await this._ensureSession();
        const headers = await this._headers({
            'accept': 'text/html,application/json;q=0.9,*/*;q=0.8',
            // ★ 这一跳在语义上是【跟 302 链】，浏览器里是导航；但我们这里刻意保留
            //   origin + 收 json 的 accept（见 _followRedirectsForCode 的注释：改了会让
            //   本该 302 的响应变成 200+JSON，auth session 卡在第 0 步）。
            //   既然发的是 fetch 形状的头，sec-fetch 就必须跟着报 fetch ——
            //   报 navigate 会造出「带 origin 的顶层导航」这种真浏览器发不出的组合。
            ...secFetchHeaders({ url, initiator: AUTH_BASE + '/' }),
            ...priorityHeader({}),
        });
        const r = await sess.get(url, { headers, headerOrder: headerOrderFor(Object.keys(headers)) });
        const text = await r.text().catch(() => '');
        await this._absorb(r, url);
        if (this._isCf(r.status, text)) throw new Error(`CLOUDFLARE_BLOCK: GET 被 Cloudflare 挑战(HTTP ${r.status})`);
        const loc = r.headers && (r.headers['location'] || r.headers['Location']);
        return { status: r.status, location: Array.isArray(loc) ? loc[0] : loc, text };
    }

    // ── 尾巴状态机：从 add_phone 开始（浏览器 bootstrap 已过登录）──
    // onPhoneRejected(reason) 可选：在**不再取下一个号**就直接抛错的路径上，把当前这个坏号立刻判死+退款。
    //   没有它的话，最后一个号只会走常规 cancel → release 回池 → 几秒后被另一个账号取走再被拒一次
    //   （日志里 +639756703446 连着喂给两个账号就是这么来的）。中间那些号靠下一轮
    //   onPhoneNeeded({reason}) 判死即可，不用它，免得同一个号 cancel 两次。
    // ── fraud_guard 的换号预算（maxFraudStreak / maxFraudTotal）──
    // 2026-08-10 修订。原值 maxFraudStreak=1 ⇒ 撞第一次就中止换号、整条会话作废重建。
    //   它的依据是 2026-08-03 那句「fraud_guard 是**环境级**风控…实测一轮 219 次拒发、80 个号全废，
    //   先修环境再谈放宽」。★但那份数据采于**修复之前**：它点名的四条请求形态缺陷
    //   （无 referer、无 client hints、无 x-access-flow-invocation-id、UA/TLS 版本对不上）
    //   是在同一个 commit(5134a02) 里一并修掉的，修完之后从未重新测过这个阈值。
    //   顺带更正两处以讹传讹的注释：git 里 maxFraudStreak **从来没有过 5**，5134a02 引入即为 1
    //   （rtAutomation.js 与 gui.js 里那两句「连续 5 个号」「已从 5 改为 1」都是无中生有）。
    // 修复后的实测（2026-08-08~10 三天、11 次 fraud_guard，全部走中止路径）：
    //   · 7 个受害账号最终 **100% 都拿到了 RT** —— 没有一次是"本来就该放弃"的；
    //   · 最干净的对照 wands_origami_8k：同会话换号 **1.0s**，丢弃重建 **88.4s**（88 倍）；
    //   · subnet-solver.73 在同一个会话里连试 5 个号、前 4 个被拒、第 5 个通过并拿到 code
    //     —— 会话在多次拒绝后仍然可用，这是"原地换号"成立的硬证据；
    //   · 保留窗口内 SESSION_INVALID 命中 0 次。
    //   代价上限：一个账号最多多烧 maxFraudTotal 个号（约 ¥0.4）。不满意就把 config.json 的
    //   rtProtoMaxFraudStreak 改回 1 存盘，行为立刻退回原样。
    // ★为什么要两个计数器：fraudStreak 会被下面 isPhoneRateLimited/isNumberBad 两条分支清零，
    //   「fraud→坏号→fraud→坏号」交替时 streak 永远够不到上限，能一路烧到 maxPhoneTries=8。
    //   fraudTotal 不清零，专门兜这种交替型故障。
    // ★两者都必须 < maxPhoneTries(8)：用满 8 次抛的 PHONE_ALL_REJECTED 在 classifyRtError 里
    //   **无人接管**，会掉进 review 兜底直接转人工；而 PHONE_FRAUD_GUARD_STREAK 是 retryable。
    //   宁可在 fraud 预算上先熔断，也别滑到那个悬崖。
    // maxOtpSwaps：号**已经绑上**、但在等码阶段翻车（收不到码 / 码不对）时，允许换几次号。
    //   见下面等码阶段那段注释：这一段 2026-08-10 之前在换号循环外面，翻车即整条会话作废。
    async bindPhoneAndGetCode({ onPhoneNeeded, onSmsNeeded, onPhoneRejected, maxPhoneTries = 8, maxFraudStreak = 3, maxFraudTotal = 4, maxOtpSwaps = 2 }) {
        let fraudStreak = 0;   // **连续**被 fraud_guard 拒发的号数；被其它类型的拒绝清零
        let fraudTotal = 0;    // 本次会话内被 fraud_guard 拒发的号**总数**；永不清零
        let otpSwaps = 0;      // 因"绑上了但拿不到/对不上验证码"而换掉的号数
        let lastRejectReason = 'number_rejected';   // 透传给下一轮 onPhoneNeeded，决定上个号判死时的日志措辞
        const rejectCurrent = async reason => {
            if (!onPhoneRejected) return;
            try { await onPhoneRejected(reason); } catch (e) { this.log(`[proto] 判死当前号失败(忽略): ${String(e && e.message || e).slice(0, 80)}`); }
        };
        // ★只认「这个号收不到码」这一种异常才换号。急停 / 账号已废 / Cloudflare / 会话失效 一律原样抛出——
        //   拿它们当"坏号"去换号，只会在一个必然失败的会话里把号一个个烧光。白名单而不是黑名单，
        //   是因为 onSmsNeeded 是上层传进来的闭包，它可能抛出任何东西。
        const isPhoneGotNoCode = e => {
            const m = String((e && e.message) || e || '');
            if (/ABORTED|ACCOUNT_UNUSABLE|CLOUDFLARE_BLOCK|SESSION_INVALID|account_deactivated/i.test(m)) return false;
            return String((e && e.code) || '').toLowerCase() === 'sms_timeout'
                || /sms.{0,8}timeout|短信验证码超时|验证码.{0,4}超时|未收到.{0,6}(短信|验证码)/i.test(m);
        };
        for (let pt = 1; pt <= maxPhoneTries; pt++) {
            this._ck();
            const phone = await onPhoneNeeded(pt > 1 ? { reason: lastRejectReason } : undefined);
            if (!phone) throw new Error('未取到手机号');
            // ★channel 字段必须看会话脸色，不能恒发。2026-08-03 诊断日志实测，完美分离、零反例：
            //     multi_channel_allowed=true  → 19/19 绑号成功
            //     multi_channel_allowed=false → 0/7 成功，7/7 回 409 invalid_state
            //   服务端对 mca=false 的会话不接受 channel 字段（它连多通道选择都没开），
            //   带上去就是非法请求体 —— 响应里的 redirect_uri:"/log-in" 就是"请重新登录"的意思。
            //   之前那句「会话失效」的错误文案完全是被这个误导出来的：会话是好的，是请求体不对。
            //   只在明确 false 时省略；字段缺失(undefined)时保持原行为发 sms，别动没验证过的路径。
            const mca = this.lastSession ? this.lastSession.multi_channel_allowed : undefined;
            const body = { phone_number: phone };
            if (mca !== false) body.channel = 'sms';
            this.log(`[proto] add-phone/send 尝试 ${pt}: ${phone}`
                + ` (multi_channel_allowed=${mca === undefined ? '未知' : mca}${mca === false ? ' → 省略 channel 字段' : ''})`);
            const res = await this._post('/api/accounts/add-phone/send', body);
            if (res.status === 200 && res.data && res.data.page && res.data.page.type === 'phone_otp_verification') {
                // ────────── 号绑上了，进等码阶段 ──────────
                // ★2026-08-10 之前这一整段在换号循环**外面**：等码超时或码不对时异常直接穿透绑号循环，
                //   ⇒ 整条会话作废重建（新代理会话 + 2 次 sentinel PoW + 一封新的邮箱验证码，p50 ~170s）。
                //   实测这比 fraud_guard 频繁得多：living_methane8y@icloud.com 在 2026-08-10 一天内
                //   被「120s 收不到码」打掉 4 次会话，最后打成 blocked 转人工。
                //   浏览器链路本来就是对的（browserService.js:4889-4938：码连错 / 被强制 WhatsApp
                //   → 退号换新号、回填号页重来，全程不重建会话）。这里把同样的语义补给协议链路。
                //
                // ★这条「已受理」日志是 2026-08-10 补的，此前**成功分支一条日志都没有**（只有失败才打），
                //   于是号绑上之后 RT 面板要静默整整一轮等码（~115s，等码换号开了之后最坏 ~345s）才有下一行，
                //   看起来完全像卡死 —— 实测有人因此按了强制暂停，白废一个已付费的号 + 一次 attempt。
                // ★channel 是**服务端**告诉我们这个码将从哪条通道发出（真流量抓包里 add-phone/send 的
                //   响应带 phone_verification_channel）。它对排查「号绑上了但码永远不来」是决定性的：
                //   接码平台只收 SMS，服务端若判定走 whatsapp，那这个号无论等多久都不会有码 ——
                //   那不是号源质量问题，是这个国家/号段在本链路上根本不可用，该换国家而不是换供应商。
                //   之前我们从不读这个字段，导致印度号 0/3 收码的真因至今无法判定。
                const vchan = String((res.data && (res.data.phone_verification_channel
                    || (res.data.page && res.data.page.payload && res.data.page.payload.phone_verification_channel))) || '');
                const isWa = /whats/i.test(vchan);
                this.log(`[proto] ✅ add-phone/send 已受理 → 验证码页（${phone} · ${dialLabel(phone)}）`
                    + ` · 发码通道=${vchan || '(响应未带)'} · 开始等码`
                    + (isWa ? ' ⚠️ 接码平台只收 SMS，这个号大概率等不到码，别干等——考虑换国家/号源' : ''));
                this._ck();
                let smsCode = null;
                try {
                    smsCode = await onSmsNeeded(undefined);
                } catch (e) {
                    if (!isPhoneGotNoCode(e)) throw e;
                    // 预算用尽：原样抛出。该错误自带 code='sms_timeout'，在 RT_DEAD_REASONS 白名单里，
                    // 上层的兜底 cancel 会照旧 markDead + 退款，不需要在这里再 rejectCurrent 一次。
                    if (otpSwaps >= maxOtpSwaps) throw e;
                    otpSwaps++; fraudStreak = 0; lastRejectReason = 'sms_timeout';
                    this.log(`[proto] → 该号收不到验证码（等码换号 ${otpSwaps}/${maxOtpSwaps}），判死退款换下一个号，会话保留`);
                    continue;
                }
                if (!smsCode) throw new Error('未取到 SMS 码');
                const vres = await this._post('/api/accounts/phone-otp/validate', { code: smsCode });
                if (vres.status === 200 && vres.data && vres.data.page) {
                    this.log(`[proto] phone-otp 通过 → page.type=${vres.data.page.type}`);
                    return vres.data;
                }
                this.log(`[proto] phone-otp 被拒(HTTP ${vres.status}): ${JSON.stringify(vres.data || vres.text).slice(0, 140)}`);
                // ★原来这里是 `for (ct=1..3) { const code = await onSmsNeeded(undefined) ... }` ——
                //   三轮拿到的是**同一个码**（rejectedCode 被硬编码成 undefined，markCodeRejected 从没被调用过），
                //   而服务端刚判过这个码不对 ⇒ 第 2、3 轮的成功率恒为 0，纯属白跑两个来回。
                //   协议链路又没有"重发短信"的入口（能用的只有 add-phone/send），在同一个号上重取也拿不到新码。
                //   所以正确解法是换号：新号 = 新码。日后若抓到重发端点，可以再把原地重试加回来。
                if (otpSwaps >= maxOtpSwaps) {
                    throw new Error(`PHONE_OTP_INCORRECT: 连续 ${otpSwaps + 1} 个号的手机验证码都不对`
                        + '——注意已废账号(deactivated)在纯协议下也表现为"码一直不对"，不一定是号池的问题');
                }
                otpSwaps++; fraudStreak = 0;
                lastRejectReason = '';   // 码不对≠号坏：走常规退号(退款、不 markDead)，与 browserService.js:4930 同口径
                this.log(`[proto] → 验证码不对（等码换号 ${otpSwaps}/${maxOtpSwaps}），换个号重新取码，会话保留`);
                continue;
            }
            // ★分类：只有确认"号的问题"(VoIP/无效号/已绑满)才换号+判死；会话失效/限流/未知 → 快速失败整账号，绝不烧号。
            //   血泪教训：add-phone/send 返回 409 invalid_state(会话失效)时把每个好号都误判成坏号，10 秒连烧 8 个好号。
            const d = res.data || {};
            const err = d.error || {};
            const code = String(err.code || d.code || err.type || '').toLowerCase();
            const msg = String(err.message || d.message || d.detail || (typeof res.text === 'string' ? res.text : '') || '').slice(0, 200);
            const hay = (code + ' ' + msg).toLowerCase();
            const isNumberBad = /invalid_phone|phone_number_invalid|phone.{0,8}invalid|invalid.{0,8}phone|voip|virtual|non-?virtual|already.{0,12}(associated|registered|in ?use|used)|maximum.{0,12}account|unsupported.{0,8}phone|carrier|undeliverable|cannot.{0,8}(send|deliver)/i.test(hay);
            // ★2026-08-03 新增 fraud_guard：OpenAI 对"相似号码"的风控拒发。
            //   以前它三条判定都不命中 → 掉进最后的 ADD_PHONE_UNKNOWN → **尝试 1 次就整个账号失败**。
            //   实测这是"号"的问题（同一个号换个账号照样被拒），换号是有效的，
            //   所以归入坏号分支：换号继续，并让下轮 onPhoneNeeded(number_rejected) 把它判死+退款。
            const isFraudGuard = /fraud_guard|suspicious behavior from phone/i.test(hay);
            // ★2026-08-03 拆分 invalid_state，并去掉裸 409。两处都被实测坐实：
            //   ① 原来 `res.status === 409` 单独成条件 ⇒ **任何** 409 都被改写成「会话失效」。
            //      但 409 也可能带号级语义（already associated with another account / 号已绑满），
            //      那种情况下号是真坏的，却被当"无辜"放回池里反复喂。删掉裸 409：
            //      带号语义的 409 会落到下面 isNumberBad，带 invalid_state 的仍由文案命中。
            //   ② invalid_state ≠ 会话过期。浏览器链路对照实验（6/6 全过）证明账号侧完全正常，
            //      是纯协议这条链路自己的缺陷。原来那句"auth 会话没接上"是**我们自己的猜测**，
            //      它让整个排查往会话/超时方向跑偏了几个小时。改成中性描述，别再断言原因。
            const isAddPhoneRejected = /invalid_state/i.test(hay);
            const isSession = res.status === 401 || /session.{0,8}(no longer|not).{0,8}valid|sign-?in.{0,8}session|start over|log-?in/i.test(hay);
            // ★「验证请求次数达上限」是**号级**的，不是账号/IP 级 —— 2026-08-03 实测坐实：
            //     +17624194434 / +18393221046 / +17624570337 各自在 **3 个不同账号**上都被限，
            //     而每个任务都换了代理出口、换了账号，同号照样必中；被限号 bindCount=0，成功号平均 1.86。
            //   浏览器链路早就是这么判的（browserService.js:3445 isRateLimitText 归进 isBadNumberText
            //   → markDead+退款换号，注释写着"这个号请求验证码次数用光=号废，与IP/账号无关"）。
            //   而协议链路原来把它归进 isRate → 整个账号快速失败、号还留在池里，
            //   于是同一个坏号被反复喂给下一个账号（上面那三个号就是这么各毒害 3 个账号的）。
            //   现在：HTTP 429（传输层限流，可能真是 IP/账号级）仍走 RATE_LIMITED_PROTO；
            //         HTTP 4xx + rate_limit_exceeded 这种业务码 → 判坏号，换号并判死退款。
            const isPhoneRateLimited = res.status !== 429
                && /rate.?limit|too many (verification |phone |sms )?(requests|attempts|codes)|too many phone|请求.{0,4}上限|リクエスト回数/i.test(hay);
            const isRate = res.status === 429 || (!isPhoneRateLimited
                && /rate.?limit|too many|请求.{0,4}上限|リクエスト回数|しばらく/i.test(hay));
            this.log(`[proto] ⚠️ add-phone/send 未进OTP页 | HTTP ${res.status} | code=${code || '(无)'} | msg=${msg}`);
            if (res.status !== 200) this.log(`[proto] ⚠️ add-phone/send 完整响应: ${JSON.stringify(res.data == null ? String(res.text || '').slice(0, 600) : res.data).slice(0, 900)}`);
            if (isAddPhoneRejected) {
                throw new Error(`ADD_PHONE_REJECTED_STATE: add-phone/send 被服务端拒绝(invalid_state, HTTP ${res.status})`
                    + `——号无辜不判死。已知成因：会话 multi_channel_allowed=${mca === undefined ? '未知' : mca} 时请求体不合法`
                    + `（本次已${mca === false ? '省略' : '发送'} channel 字段）。若仍复现，说明还有别的成因，看 [diag] 行`);
            }
            if (isSession) throw new Error(`SESSION_INVALID: add-phone/send 会话失效(${code || res.status})——auth 会话没接上，账号回队列重试；号无辜不判死`);
            if (isRate) throw new Error(`RATE_LIMITED_PROTO: add-phone/send 限流(${code || res.status})——快速失败，号不判死`);
            if (isFraudGuard) {
                fraudStreak++; fraudTotal++; lastRejectReason = 'fraud_guard';
                // ★错误码字符串保持 PHONE_FRAUD_GUARD_STREAK 不变：rtAutomation.js 的 classifyRtError
                //   与 gui.js 的错误文案映射都按它匹配，改名会让分类静默退化成 review 兜底。
                const capped = fraudStreak >= maxFraudStreak || fraudTotal >= maxFraudTotal;
                const why = fraudTotal >= maxFraudTotal ? '累计' : '连续';
                this.log(`[proto] → fraud_guard 拒发（连续 ${fraudStreak}/${maxFraudStreak}，累计 ${fraudTotal}/${maxFraudTotal}）`
                    + `，${capped ? `${why}预算用尽，判死此号并中止换号` : '判坏号换下一个（同一会话内继续）'}`);
                if (capped) {
                    await rejectCurrent('fraud_guard');
                    throw new Error(`PHONE_FRAUD_GUARD_STREAK: fraud_guard 换号预算用尽`
                        + `（连续 ${fraudStreak}/${maxFraudStreak}、累计 ${fraudTotal}/${maxFraudTotal}）`
                        + `——本次会话不再换号，账号回队列重试；被拒的号已逐个判死退款`);
                }
                continue;   // 下轮 onPhoneNeeded({reason:'fraud_guard'}) 会 markDead + /sms/cancel 退款
            }
            if (isPhoneRateLimited) {
                fraudStreak = 0; lastRejectReason = 'number_rejected';
                this.log('[proto] → 判定坏号（该号验证请求次数已用尽，换账号也没用），换号并判死退款');
                continue;   // 下轮 onPhoneNeeded(number_rejected) 会 markDead + /sms/cancel 退款
            }
            if (isNumberBad) { fraudStreak = 0; lastRejectReason = 'number_rejected'; this.log('[proto] → 判定坏号(VoIP/无效/已绑满)，换号'); continue; }   // 下轮 onPhoneNeeded(number_rejected) 才 markDead
            throw new Error(`ADD_PHONE_UNKNOWN: add-phone/send 未知响应 HTTP ${res.status} ${code} ${msg.slice(0, 80)}——保守快速失败，号不判死`);
        }
        // 走到这里 = maxPhoneTries 用满、仍然一个号都没走通「绑号 + 验证码」全程。
        // 最后一个号同样算坏号，判死退款（这条路径不再取下一个号，没有下一轮 onPhoneNeeded 帮忙判死）。
        await rejectCurrent(lastRejectReason || 'number_rejected');
        throw new Error(`PHONE_ALL_REJECTED: ${maxPhoneTries} 个号都没能走通绑号+验证码`
            + `（其中 fraud_guard ${fraudTotal} 次、绑上后等码换号 ${otpSwaps} 次）`);
    }

    async selectWorkspaceAndGetCode() {
        this._ck();
        const list = this.lastSession && Array.isArray(this.lastSession.workspaces)
            ? this.lastSession.workspaces : [];
        const want = this.targetWorkspaceId;
        // 指定了目标就按 id 挑，没指定还是老样子取第 0 个（不传目标时行为一字不变）。
        const ws = want ? (list.find(w => w && w.id === want) || null) : (list[0] || null);
        if (want && !ws) {
            // ★必须判 permanent：指定的空间不在候选里，重试多少次都还是不在。
            //   判可重试的话会像 MFA 那次一样连跑 8 次、白烧 8 封验证码邮件。
            //   错误信息里把整个候选列表打出来 —— 排查时唯一有用的就是这个。
            const e = new Error(`RT_WORKSPACE_NOT_FOUND: 会话里没有目标工作空间 ${want}；`
                + `候选: ${list.map(w => `${w && w.id}/${w && w.kind}/${w && w.name}`).join(' | ') || '(空)'}`);
            e.code = 'RT_WORKSPACE_NOT_FOUND';
            e.permanent = true;
            throw e;
        }
        if (!ws || !ws.id) {
            // ★兜底：走到这里而拿不到 workspace，最常见的真因是**账号开了两步验证**——
            //   服务端把会话停在 mfa_challenge，根本没推进到 workspace 那一步。
            //   会话对象里的 mfa_factors / mfa_challenge_factors 是同一件事的旁证。
            //   不识别的话只会报「会话里没有 workspace」，一个跟真因毫不相干、还会被反复重试的错。
            const s = this.lastSession || {};
            const looksMfa = s.mfa_factors || s.mfa_challenge_factors
                || /mfa|two[-_]?factor|totp/i.test(String((this.lastOtpResponse
                    && this.lastOtpResponse.page && this.lastOtpResponse.page.type) || ''));
            if (looksMfa) {
                const e = new Error('ACCOUNT_MFA_REQUIRED: 账号开启了两步验证，会话停在 MFA 挑战，'
                    + '纯协议无法输入 2FA 码——请改用浏览器链路，或先关掉账号的 MFA');
                e.code = 'ACCOUNT_MFA_REQUIRED';
                throw e;
            }
            throw new Error('会话里没有 workspace（拿不到 workspace_id）——'
                + `会话字段: ${Object.keys(s).filter(k => !/token|email|name|session_id|logging_id/i.test(k)).join(',') || '(空)'}`);
        }
        const res = await this._post('/api/accounts/workspace/select', { workspace_id: ws.id });
        if (res.status !== 200 || !res.data || !res.data.page) throw new Error(`workspace/select 失败 HTTP ${res.status}: ${JSON.stringify(res.data || res.text).slice(0, 140)}`);
        const nextUrl = (res.data.page.payload && res.data.page.payload.url) || res.data.continue_url;
        if (!nextUrl) throw new Error('workspace/select 未返回 external_url');
        this.log('[proto] 跟随 external_url 链取 code…');
        return await this._followRedirectsForCode(nextUrl);
    }

    async _followRedirectsForCode(startUrl, maxHops = 10) {
        let url = startUrl;
        const redirectBase = new URL(this.redirectUri);
        for (let hop = 0; hop < maxHops; hop++) {
            this._ck();
            try {
                const u = new URL(url);
                if (u.origin === redirectBase.origin && u.pathname === redirectBase.pathname && (u.searchParams.has('code') || u.searchParams.has('error'))) {
                    const code = u.searchParams.get('code');
                    if (!code) throw new Error('回调无 code: ' + (u.searchParams.get('error_description') || u.searchParams.get('error')));
                    this.log(`[proto] ✅ 截获 authorization code (${code.slice(0, 8)}…)`);
                    return code;
                }
            } catch (e) { if (/回调无 code/.test(e.message)) throw e; }
            const r = await this._getRaw(url);
            this.log(`[proto]   hop${hop}: ${r.status} ${String(url).replace(AUTH_BASE, '').slice(0, 56)}${r.location ? ' → ' + r.location.slice(0, 56) : ''}`);
            if (r.status >= 300 && r.status < 400 && r.location) {
                url = r.location.startsWith('http') ? r.location : new URL(r.location, url).toString();
                continue;
            }
            // 200：这步可能返回 JSON 状态机响应(继续跟 continue_url / page.payload.url)——浏览器里是 302，HTTP 里改成 200+JSON
            if (r.status === 200) {
                let j = null; try { j = JSON.parse(r.text); } catch (e) {}
                const next = j && ((j.page && j.page.payload && j.page.payload.url) || j.continue_url);
                if (next) { this.log(`[proto]   hop${hop}: 200 → 跟 JSON continue_url (page.type=${j.page && j.page.type})`); url = next.startsWith('http') ? next : new URL(next, url).toString(); continue; }
                this.log(`[proto]   hop${hop}: 200 无 Location/continue_url。body(前300): ${String(r.text).slice(0, 300)}`);
                throw new Error('跟随重定向中断 HTTP 200（consent 步无 Location/continue_url，body 见日志）');
            }
            throw new Error(`跟随重定向中断 HTTP ${r.status}（无 Location）`);
        }
        throw new Error('跟随重定向超过 ' + maxHops + ' 跳仍未拿到 code');
    }
}

module.exports = { RtProtocolClient, apiCallHeaders, API_REFERER, dialLabel };
