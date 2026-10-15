'use strict';
// ── 纯协议 RT 客户端（C 架构：无浏览器）──
//
// 与既有两条链路的关系：
//   A  全程浏览器            rtBrowserTask.runEmailAccount
//   B  浏览器 bootstrap + 协议尾巴  ★2026-08-14 已删除（「🚀协议模式(快)」/settings.useProtocol）
//   C  本文件：全程协议，Sentinel 由 Node vm 跑真 sdk.js 算（sentinelClient）
//
// ★继承 RtProtocolClient：_post/_getRaw/_absorb/_isCf/_headers/bindPhoneAndGetCode/
//   selectWorkspaceAndGetCode/_followRedirectsForCode 全部复用——尤其是 bindPhoneAndGetCode
//   里那套「坏号 vs 会话失效 vs 限流」分类（血泪教训：误判会 10 秒连烧 8 个好号），
//   绝不在这里重写一份。
//
// 本文件只补 B 架构缺的**前半段**：authorize → 提交邮箱 → 邮箱 OTP → 落到 consent/phone。
//
// ★出口一致性铁律：sentinel 请求、协议请求、换 token 三处必须同一出口。
//   B 架构靠 browserService 轮换后的 effectiveProxy；C 架构没有浏览器，
//   由调用方（rtPureProtocolTask）解析一次 proxyUrl，然后贯穿始终。
const { RtProtocolClient, apiCallHeaders } = require('./rtProtocolClient');
const { SentinelClient } = require('./sentinelClient');
const fp = require('./sentinelFingerprint');
const { headerOrderFor } = require('./chromeHeaderOrder');   // 见父类顶部注释：此前全链路按字母序发头
const { secFetchHeaders, priorityHeader } = require('./secFetch');
const { totp: computeTotp, msUntilNextStep } = require('./totp');

const AUTH_BASE = 'https://auth.openai.com';

// statusCode 是可选的：给它的**唯一**理由是让 rtAutomation.classifyRtError 能按 HTTP 语义分类
// （errorStatusCode 只读对象字段，不解析文案里的 "HTTP 401"）。
// ★别图省事去让分类器解析文案：CLOUDFLARE_BLOCK 的文案里就写着 "(HTTP 403)"，
//   一旦被解析成 statusCode=403 就会命中「permanent ⇒ account_invalid」，
//   把一批只是被 CF 挑战的好号判死。所以只在**确实是账号级判据**的地方显式传。
function codedError(code, message, statusCode = 0) {
    const error = new Error(message);
    error.code = code;
    if (statusCode) error.statusCode = Number(statusCode);
    return error;
}

// ── 临时性网络错误识别（口径移植自 turb core/openai_auth.py:_is_transient_network_error）──
// 分两档，因为「能不能重试」取决于请求有没有可能已经到达服务端：
//   连接阶段失败（拒绝/重置/TLS/代理/解析）→ 服务端肯定没收到 ⇒ POST 也能安全重试
//   传输中断（unexpected EOF、读超时）      → 服务端可能已处理 ⇒ 只有幂等请求(GET)才重试
const CONNECT_PHASE = /connection refused|connection reset|econnrefused|econnreset|tls|ssl|handshake|proxy|no such host|dns|socks/i;
const MID_TRANSFER = /unexpected eof|timeout|timed out|client\.timeout|context deadline|eof|socket hang up|aborted by peer/i;

function classifyNetError(err) {
    // ★ 显式标记优先于文案匹配。调用方明确知道自己是哪一档时（例如把 node-tls-client 的
    //   status=0 转成异常的那些地方），不该再让正则去猜 —— Go 把错误原因放在字符串**末尾**，
    //   而消息为了可读几乎一定会被截断，一截就再也匹配不上（2026-08-20 就是这么复发的）。
    if (err && (err.netKind === 'connect' || err.netKind === 'transfer')) return err.netKind;
    const msg = String((err && err.message) || err || '').toLowerCase();
    // ★ "Client not initialized. Call initTLS() first." 不是网络错误，是 TLS 引擎没起来
    //   （或者被谁 destroyTLS 拔掉了）。CONNECT_PHASE 里那个裸 `tls` 会命中它里面的
    //   "ini(tTLS)()"，把永久性故障伪装成可重试的连接抖动 —— 2026-08-16 协议区块并发全灭时
    //   sentinelClient 就是这么被坑的（那边同一处已加短路，两边口径必须一致，见本文件上方注释）。
    //   ⚠ 不要改 CONNECT_PHASE 里的 `tls` 为 \btls\b，会漏掉 "tlsv1 alert internal error"。
    if (/not initialized/.test(msg)) return '';
    if (CONNECT_PHASE.test(msg)) return 'connect';
    if (MID_TRANSFER.test(msg)) return 'transfer';
    return '';
}

// 已经越过「邮箱验证码」这一步的页面 —— 落到这些说明服务端不打算再要邮箱码了，
// 直接交给 classifyNextStep 走绑号/consent，别再傻等 200 秒的邮件。
const ADVANCED_PAGE = /add[-_]?phone|phone[-_]?otp|phone_verification|consent|codex|workspace|oauth/i;

// email-otp/validate 之后可能落到的几种状态
const NEXT_PHONE = 'phone';        // 需要绑手机
const NEXT_CONSENT = 'consent';    // 直接到 consent，可以选 workspace
const NEXT_FOLLOW = 'follow';      // 已经给了可跟随的 URL
const NEXT_MFA = 'mfa';            // ★账号开了两步验证，纯协议做不了

class RtPureProtocolClient extends RtProtocolClient {
    /**
     * @param {object} opts  继承 RtProtocolClient 的全部选项，另加：
     *   deviceId   整条链路共用的 oai-did（不传则自动生成）
     *   country    出口国家码，用来选指纹画像（JP/US/SG/...）
     *   profile    指纹画像覆盖项
     *   nodeExecutable  可选，spawn sentinel-runner 用的 node
     */
    constructor(opts = {}) {
        super(opts);
        this.deviceId = String(opts.deviceId || fp.newDeviceId());
        const locale = fp.localeProfileForCountry(opts.country || 'JP');
        this.profile = { ...fp.DEFAULT_PROFILE, ...locale, ...(opts.profile || {}) };
        // UA 必须三处一致：HTTP 头、sentinel 指纹、runner VM
        this.userAgent = opts.userAgent || this.profile.userAgent;
        this.profile.userAgent = this.userAgent;
        // ★accept-language 也必须跟画像一致。原来父类 _headers 里硬编码 ja-JP，
        //   而出口选 US 时 sentinel token 里的 navigatorLanguage 是 en-US ——
        //   「浏览器语言 en-US、HTTP 只接受日语」这种矛盾同样是很干净的自动化特征。
        this.acceptLanguage = this.profile.acceptLanguage
            || `${this.profile.navigatorLanguage},${String(this.profile.navigatorLanguage).split('-')[0]};q=0.9`;
        this.nodeExecutable = opts.nodeExecutable || null;
        this._sentinelSid = null;   // 同一账号一轮内保持稳定
        this.lastOtpResponse = null;
        // 宽容验证：号被 OpenAI 强制走 WhatsApp 时也照常提交、不退号换新
        // （前提是接码平台能收 WhatsApp）。语义对齐 browserService 的 tolerantVerify。
        this.tolerantVerify = opts.tolerantVerify === true;
        // 账号登录密码：服务端把我们带到密码页时要用（POST password/verify）。
        // 来源是 accounts.extra_json.password，由 prepareIsolatedRtTask 放进 task.account.password。
        this.accountPassword = String(opts.accountPassword || '').trim();
        // 2FA：号硬化后开了 TOTP，RT 登录中段会撞 mfa_challenge，用这俩应答。
        //   来源同 accountPassword —— prepareIsolatedRtTask 从 accounts.extra_json 放进 task.account。
        this.totpSecret = String(opts.totpSecret || '').trim();
        this.totpFactorId = String(opts.totpFactorId || '').trim();
        // TOTP 码计算函数（测试可注入假实现，默认用 src/totp）
        this._totpFn = typeof opts.totp === 'function' ? opts.totp : computeTotp;
        // 代理会话轮换器：CF 挑战时用它换一个出口再重试（见 bootstrapAuthorize）
        this.rotateProxy = typeof opts.rotateProxy === 'function' ? opts.rotateProxy : null;
        // 出口切换成功后的通知（见 _switchProxy 末尾）：上层用它把换 token 的出口一并同步过去
        this.onProxySwitched = typeof opts.onProxySwitched === 'function' ? opts.onProxySwitched : null;
        // CF 挑战时「换出口重试」的次数上限。★2026-08-11 之前这是**死代码**：
        //   bootstrapAuthorize:464 是全仓唯一的读点，而构造函数从不赋值、config 里也没有这个键，
        //   于是 `Number(this.cfMaxAttempts || 4)` 恒等于 4，想调都无从调起。
        //   实测逐次通过率 84%→54%→29%→42%，坏窗口里 4 次常常不够（08-11 有 4 个账号打穿），
        //   所以把它接出来，坏窗口时可以临时调大硬扛。范围与默认值见 config.js 的 rtProtoCfMaxAttempts。
        this.cfMaxAttempts = Number(opts.cfMaxAttempts) > 0 ? Number(opts.cfMaxAttempts) : 0;
    }

    /**
     * 覆写父类的手机绑定：仅在 tolerantVerify=true 时，把「被强制 WhatsApp」从坏号里摘出来。
     * ★父类 bindPhoneAndGetCode 的 isNumberBad 正则里含 whatsapp ⇒ 默认一律换号。
     *   浏览器链路有 tolerantVerify 开关能豁免，纯协议原来没接 ⇒ 你在 GUI 勾了也不生效。
     *   这里不重写父类逻辑（那套坏号/会话失效/限流分类是血泪换来的），
     *   只在宽容模式下把 whatsapp 关键词从判定里"挡"掉，其余行为完全不变。
     */
    /**
     * 手机阶段的「账号已废」嗅探。
     *
     * ★为什么必须在这一步查：实测已废账号（deactivated）在纯协议下**不会**在邮箱 OTP 就被拒，
     *   OpenAI 会让你走完 add-phone/send → phone-otp/validate 全程，
     *   期间任何短信码都不会通过，最后才在 consent 步暴露 account_deactivated。
     *   于是表面症状是「连续 3 次手机验证码都不对」(PHONE_OTP_INCORRECT)，
     *   看起来像号池问题 —— 实际是账号已死。
     *   2026-08-03 那批 4 个 icloud 号全是这个模式，重试一次才露出真因。
     *
     * 检测到就立刻抛 ACCOUNT_UNUSABLE：省下后续 2 个短信码 + 让上层正确判 permanent。
     */
    _sniffDeadAccountInPhoneStage(res, stage) {
        if (!res) return;
        const d = res.data || {};
        const err = d.error || {};
        const msg = String(err.message || d.message || d.detail || '').trim();
        const code = String(err.code || d.code || '').toLowerCase();
        const hay = `${code} ${msg} ${String(res.text || '').slice(0, 400)}`.toLowerCase();
        if (code === 'account_deactivated'
            || /deactiv|deleted or deactivated|account.{0,24}(deleted|suspend|banned)/i.test(hay)) {
            throw codedError('ACCOUNT_UNUSABLE',
                `账号已废（${code || 'account_deactivated'}）—— 在${stage}阶段暴露: ${msg || String(res.text || '').slice(0, 160)}`);
        }
    }

    async bindPhoneAndGetCode(opts) {
        // 无论宽容与否，都要在手机阶段嗅探"账号已废"
        const origPostForSniff = this._post.bind(this);
        this._post = async (path, body) => {
            const res = await origPostForSniff(path, body);
            if (/phone-otp\/validate/.test(path)) this._sniffDeadAccountInPhoneStage(res, '手机验证码');
            else if (/add-phone\/send/.test(path)) this._sniffDeadAccountInPhoneStage(res, '绑手机');
            return res;
        };
        try {
            return await this._bindPhoneInner(opts);
        } catch (e) {
            // 连续 3 次码不对：极可能是已废账号（见上方注释），给上层一个明确的复验提示
            if (/PHONE_OTP_INCORRECT/.test(String(e.message || ''))) {
                e.message = String(e.message)
                    + '——注意：已废账号(deactivated)在纯协议下也表现为"码一直不对"，'
                    + '不一定是号池问题；本次会自动重试一轮以确认真因';
                e.code = e.code || 'PHONE_OTP_INCORRECT';
            }
            throw e;
        } finally {
            this._post = origPostForSniff;
        }
    }

    async _bindPhoneInner(opts) {
        if (!this.tolerantVerify) return super.bindPhoneAndGetCode(opts);
        const origResponseText = this._responseTextForPhone;
        this.log('[pure] 宽容验证已开启：被强制 WhatsApp 的号也照常提交，不退号');
        // 用一层包装：把响应里的 whatsapp 字样替换成中性词，避免命中 isNumberBad 的 whatsapp 分支。
        // 只影响"要不要判这个号是坏号"，不影响 HTTP 语义与其它分类。
        const origPost = this._post.bind(this);
        this._post = async (path, body) => {
            const res = await origPost(path, body);
            if (this.tolerantVerify && /add-phone\/send/.test(path) && res && typeof res.text === 'string'
                && /whats\s?app/i.test(res.text)) {
                this.log('[pure] 宽容验证：检测到 WhatsApp 通道提示，按可用处理');
                res.text = res.text.replace(/whats\s?app/gi, 'sms-channel');
                if (res.data) {
                    try { res.data = JSON.parse(res.text); } catch (e) {}
                }
            }
            return res;
        };
        try {
            return await super.bindPhoneAndGetCode(opts);
        } finally {
            this._post = origPost;
            this._responseTextForPhone = origResponseText;
        }
    }

    // 覆写：accept-language 跟随画像，不再写死 ja-JP。
    // ★不加 oai-device-id / oai-language ——
    //   HAR 里 auth.openai.com 的 JSON 接口只带 RUM/trace 头，不带 oai-client-*。
    //   多发这两个头是与真实流量的偏差，没必要冒险。
    async _headers(extra) {
        const base = await super._headers(extra);
        base['accept-language'] = this.profile.acceptLanguage || base['accept-language'];
        return base;
    }

    /**
     * 往 cookie jar 里种 oai-did。
     * ★必须做：sdk.js 在 VM 里是从 document.cookie 读 oai-did 当设备 ID 的，
     *   jar 里没有它 → runner 输出的 token 会缺 id 字段 → 请求被判为指纹不完整。
     *   （turb 的 Python 实现同样在会话构造时往三个域各种一个，见 core/session.py）
     */
    async _ensureDeviceCookie() {
        if (this._didCookieSeeded) return;
        for (const domain of ['auth.openai.com', 'sentinel.openai.com', 'chatgpt.com']) {
            try {
                await this.jar.setCookie(
                    `oai-did=${this.deviceId}; Path=/; Domain=${domain}`,
                    `https://${domain}/`,
                    { ignoreError: true },
                );
            } catch (e) {}
        }
        this._didCookieSeeded = true;
    }

    /**
     * 临时性网络错误重试包装（指数退避）。
     * @param {boolean} idempotent true=GET/导航，任何临时错误都可重试
     *                             false=POST，只重试"连接阶段"错误
     *   （传输中断时服务端可能已处理：重发会造成重复提交邮箱、
     *     或拿同一个已被消费的验证码再验一次 → 反而制造假失败）
     *
     * ★动机：实测失败里有 3 个是 `unexpected EOF` / `request canceled`，
     *   一击不中就整个任务判失败、白烧一封验证码邮件。
     *   turb 有同样的重试（core/openai_auth.py::_with_net_retry），我移植时漏了。
     */
    async _withNetRetry(label, fn, idempotent = true) {
        const maxAttempts = 3;
        let lastErr = null;
        for (let attempt = 1; attempt <= maxAttempts; attempt++) {
            try {
                return await fn();
            } catch (e) {
                lastErr = e;
                const kind = classifyNetError(e);
                const retryable = idempotent ? !!kind : kind === 'connect';
                if (!retryable || attempt >= maxAttempts) throw e;
                const backoff = 1500 * Math.pow(2, attempt - 1);
                this.log(`[pure] ${label} 网络${kind === 'connect' ? '连接' : '传输'}错误`
                    + `(${String(e.message).slice(0, 60)})，${backoff}ms 后重试 ${attempt + 1}/${maxAttempts}`);
                await new Promise(r => setTimeout(r, backoff));
                this._ck();
            }
        }
        throw lastErr;
    }

    async _sentinel(flow) {
        await this._ensureDeviceCookie();
        let cookieHeader = await this.jar.getCookieString(AUTH_BASE).catch(() => '');
        // 双保险：万一 jar 因域名匹配没吐出 oai-did，这里补一个，绝不让 SDK 读不到设备 ID
        if (!/(^|;\s*)oai-did=/.test(cookieHeader)) {
            cookieHeader = cookieHeader
                ? `oai-did=${this.deviceId}; ${cookieHeader}`
                : `oai-did=${this.deviceId}`;
        }
        const client = new SentinelClient({
            deviceId: this.deviceId,
            sentinelSid: this._sentinelSid || undefined,
            profile: this.profile,
            proxyUrl: this.proxyUrl,
            cookieHeader,
            log: m => this.log(m),
            abortCheck: this.abortCheck,
            nodeExecutable: this.nodeExecutable || undefined,
        });
        if (!this._sentinelSid) this._sentinelSid = client.sentinelSid;
        try {
            return await client.token(flow);
        } finally {
            await client.close().catch(() => {});
        }
    }

    // 带 sentinel 头的 POST
    async _postWithSentinel(path, bodyObj, flow, referer) {
        const s = await this._sentinel(flow);
        this._ck();
        const sess = await this._ensureSession();
        const url = AUTH_BASE + path;
        const headers = await this._headers({
            'content-type': 'application/json',
            'accept': 'application/json',
            // 与父类 _post 同源：x-access-flow-invocation-id + 端点专属 referer（referer 参数显式传了就以它为准）
            ...apiCallHeaders(path),
            'openai-sentinel-token': s.sentinelToken,
            ...(s.soToken ? { 'openai-sentinel-so-token': s.soToken } : {}),
            ...(referer ? { referer } : {}),
            // ★★ 这三行必须写在【这里】，不能指望父类 _headers()。
            //   2026-08-14 第一版把它漏了，成因是我在注释里写了句假话：
            //   「父类 _headers 已经带上 sec-fetch + priority」—— 它没有。
            //   父类 _headers() 只返回 user-agent / accept / accept-encoding /
            //   accept-language / sec-ch-ua×3 / origin / referer / cookie；
            //   sec-fetch 与 priority 是在父类 _post 与 _getRaw 的【调用点】加的。
            //   而本方法不走 _post（它要自己塞 sentinel token），于是整条纯协议链路
            //   最受审视的那个 POST 反倒一个都没发。别再把它挪进 _headers ——
            //   挪过去 initiator 就没法按端点算了。
            ...secFetchHeaders({ url, initiator: AUTH_BASE + '/' }),
            ...priorityHeader({}),
        });
        // 自定义头（openai-sentinel-token / so-token）由 headerOrderFor 放进
        // sec-ch-ua 块与 user-agent 之间的 <custom> 槽 —— Chrome 对 XHR 自定义头的常见位置。
        const order = headerOrderFor(Object.keys(headers));
        // POST 只重试"连接阶段"错误：传输中断时服务端可能已处理，重发会重复提交
        const { r, text } = await this._withNetRetry(`POST ${path}`, async () => {
            const resp = await sess.post(url, { headers, body: JSON.stringify(bodyObj), headerOrder: order });
            return { r: resp, text: await resp.text().catch(() => '') };
        }, false);
        await this._absorb(r, url);
        if (this._isCf(r.status, text)) {
            throw codedError('CLOUDFLARE_BLOCK',
                `CLOUDFLARE_BLOCK: ${path} 被 Cloudflare 挑战(HTTP ${r.status})——TLS指纹/IP 需调`);
        }
        let data = null;
        try { data = JSON.parse(text); } catch (e) {}
        if (data && data['oai-client-auth-session']) this.lastSession = data['oai-client-auth-session'];
        return { status: r.status, data, text };
    }

    /**
     * ① 跟 authorize URL 建立 auth.openai.com 会话。
     *    产出 oai-client-auth-session cookie —— 后面 selectWorkspaceAndGetCode 靠它取 workspace_id。
     */
    /**
     * 网络预热：先访问两个普通页面，拿到 Cloudflare 的 __cf_bm bot-management cookie。
     * ★冷 cookie jar 直接打 /oauth/authorize 会被 CF 间歇性 403（实测 3 次里中 2 次）。
     *   turb 的 Python 实现同样在 authorize 前做 network_preflight，不是可选项。
     *   预热只是 GET 公开页面，不带邮箱、不触发任何 OTP。
     */
    async preflight() {
        this._ck();
        await this._ensureDeviceCookie();
        const sess = await this._ensureSession();
        // ★只预热本链路真正会打的两个域：
        //   auth.openai.com  —— authorize / accounts API
        //   sentinel.openai.com —— 每次拿 challenge
        //   不预热 chatgpt.com：纯协议流程压根不访问它，而且 node-tls-client 对该域
        //   在同一 session 上重复请求会抛 "Cannot read properties of undefined (reading 'key')"，
        //   徒增一条每次必失败的噪音日志。
        // ★★ 2026-08-25：sv 走 activeSdk()，不用 this.profile.sentinelSv。
        //   profile 上那个是【编译期常量】，自动跟版之后它就是旧的；
        //   而 SentinelClient 只纠正它自己那份，纠正不到这里 ——
        //   于是会出现"预热请求报旧版、铸 token 报新版"的自相矛盾。
        //   ⚠ 自动跟版关着时 activeSdk().sv 就等于那个常量，此改动等价于原样。
        const _sv = require('./sentinelSdkUpdater').activeSdk().sv || this.profile.sentinelSv;
        const targets = [
            ['https://auth.openai.com/log-in', 'https://chatgpt.com/login'],
            [`https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=${_sv}`,
                'https://auth.openai.com/log-in'],
        ];
        // ★2026-08-11 补：预热本来就是**两次真实的 auth.openai.com 请求**，等于一次天然的 CF 探测，
        //   但这段代码从来不看 r.status、也从不调父类现成的 _isCf() —— 403 被当成正常响应吸收掉。
        //   于是日志那句「预热完成」只代表函数跑完了，跟成不成功无关；而 `cf_bm=已拿到` 也靠不住，
        //   因为 **Cloudflare 的挑战页本身也会下发 __cf_bm**（这正好解释了它为什么毫无预测力：
        //   实测拿到的后续撞 CF 率 26%、没拿到的反而 15%）。
        //   现在把每次预热的真实结果记下来，供事后回答「预热被拦能不能预测 authorize 被拦」。
        //   ★★ 只记录，**绝不抛异常**：preflight 有两个调用点，:445 与 :527，而 :527 在 catch 块里 ——
        //      让它抛会直接穿出 bootstrapAuthorize、跳过剩余重试，与「换出口再试」正好相反。
        this._lastPreflight = { total: 0, cf: 0, statuses: [] };
        for (const [url, referer] of targets) {
            try {
                const cookie = await this.jar.getCookieString(url).catch(() => '');
                const navHeaders = {
                    // ★2026-09-18 补 sec-ch-ua 三件套。此前预热/导航的顶层 GET 只发 user-agent、
                    //   一个 client hint 都不带 —— 而 API 路径（父类 _headers → _clientHints）每个请求都带，
                    //   真 Chrome 从 89 起【连顶层导航都带】。缺了它就是「UA 报 Chrome、却不发 Chrome 的 hint」
                    //   这一层自相矛盾，正是 CF 最容易抓的自动化特征。NAVIGATION_ORDER 早就给这三个头
                    //   留了打头的槽位（chromeHeaderOrder.js:29-31），但 navHeaders 里没这几个键 ⇒ 槽位空着、
                    //   从来没真发出去。补进字典即可，线序由 headerOrderFor 自动摆到最前，无需改排序逻辑。
                    ...this._clientHints(),
                    'user-agent': this.userAgent,
                    'accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8',
                    'accept-encoding': 'gzip, deflate, br, zstd',   // 见父类 _headers 的注释：不发就少 zstd 且被甩到末尾
                    'accept-language': this.profile.acceptLanguage,
                    'upgrade-insecure-requests': '1',
                    'sec-fetch-site': 'same-origin',
                    'sec-fetch-mode': 'navigate',
                    'sec-fetch-dest': 'document',
                    'sec-fetch-user': '?1',
                    referer,
                    ...(cookie ? { cookie } : {}),
                };
                const r = await sess.get(url, {
                    headers: navHeaders,
                    // 导航模板：sec-ch-ua 三件套打头、upgrade-insecure-requests、cookie 收尾
                    headerOrder: headerOrderFor(Object.keys(navHeaders), { navigation: true }),
                });
                const body = await r.text().catch(() => '');
                await this._absorb(r, url);
                this._lastPreflight.total++;
                this._lastPreflight.statuses.push(r.status);
                if (this._isCf(r.status, body)) {
                    this._lastPreflight.cf++;
                    this.log(`[pure] ⚠️ 预热被 CF 挑战：${new URL(url).pathname} HTTP ${r.status}（仅记录，不中断）`);
                }
            } catch (e) {
                this._lastPreflight.statuses.push(0);
                this.log(`[pure] 预热 ${url} 失败（忽略）：${String(e.message).slice(0, 80)}`);
            }
        }
        const jarCookie = await this.jar.getCookieString('https://auth.openai.com').catch(() => '');
        const p = this._lastPreflight;
        // 这一行是「预热到底成没成」的唯一事实来源。cf_bm 保留但已知无预测力，别再拿它当判据。
        this.log(`[pure] 预热完成，cf_bm=${/__cf_bm=/.test(jarCookie) ? '已拿到' : '未拿到'}`
            + ` · 状态码=[${p.statuses.join(',')}]${p.cf ? ` · ⚠️ 其中 ${p.cf}/${p.total} 次被 CF 挑战` : ''}`);
    }

    /**
     * 切换出口代理：重建 TLS 会话 + 清空 cookie jar + 重新种 oai-did。
     * ★三件事缺一不可：
     *   - 不重建 TLS 会话 → node-tls-client 的连接池还挂在旧出口上，换了等于没换
     *   - 不清 cookie jar → 新 IP 带着旧 IP 拿到的 __cf_bm / auth-session，
     *     在 CF 眼里是"cookie 与 IP 不匹配"，比不带 cookie 更可疑
     *   - 清了 jar 就丢了 oai-did，必须补种（sdk.js 靠它读设备 ID）
     */
    async _switchProxy(proxyUrl) {
        const { CookieJar } = require('tough-cookie');
        // ★必须先摘引用再 close：node-tls-client 的 Go 层对同一个 session
        //   重复 destroy 会 nil pointer panic（cffi_src/factory.go RemoveSession），
        //   整个 worker 进程直接挂掉。先置 null，close 失败也不会留下悬挂引用。
        const old = this._sess;
        this._sess = null;
        if (old) { try { await old.close(); } catch (e) {} }
        this.proxyUrl = proxyUrl;
        this.jar = new CookieJar();
        this._didCookieSeeded = false;
        await this._ensureDeviceCookie();
        // ★只有真正切成功才通知上层 —— 换 token 那一步要跟着换到同一个出口。
        //   之前只换了 client 和 sentinel，OAuthService 的闭包还解析着**最初**那个出口对象：
        //   授权走出口 B、换 token 走出口 A。实测代价：换过出口的 attempt 里，
        //   已经拿到 authorization code 的 29 次有 8 次(27.6%)在最后一步 TLS 连不上，
        //   而没换过出口的对照组只有 2/305(0.66%) —— 差 42 倍。
        //   放在这里（而不是让上层在调 rotateProxy 时就赋值）是有意的：
        //   本方法上面几步可能抛错并被调用方吞成「换出口失败，继续用原出口重试」，
        //   那种情况下上层必须**保持旧出口**，否则会造成反向的不一致。
        if (typeof this.onProxySwitched === 'function') {
            try { this.onProxySwitched(proxyUrl); } catch (e) {
                this.log(`[pure] onProxySwitched 回调异常(忽略): ${String(e && e.message).slice(0, 80)}`);
            }
        }
        this.log(`[pure] 已切换出口并重置会话状态`);
    }

    /**
     * 覆写 close：确保同一个底层 session 只被销毁一次。
     * ★父类 close() 在 _switchProxy 之后还会被 finally 再调一次；
     *   node-tls-client 的 Go 层对同一 session 二次 destroy 会 nil pointer panic
     *   （实测直接把 worker 进程带崩）。这里做幂等保护。
     */
    async close() {
        const old = this._sess;
        this._sess = null;
        if (old) { try { await old.close(); } catch (e) {} }
    }

    /**
     * 浏览器**导航**语义的 GET。
     * ★不能用继承来的 _getRaw：它的 accept 头带 application/json，
     *   auth.openai.com 会据此做内容协商，把本该 302 的跳转改成 200+JSON 状态机响应
     *   （这一点 rtProtocolClient._followRedirectsForCode 的注释里已经记过）。
     *   bootstrap 阶段一旦收到 200 就不再跳转，auth session 停在第 0 步，
     *   后续 authorize/continue 必被判 invalid_auth_step。
     *   所以这里用纯导航头：只要 text/html，不要 json，也不发 origin。
     */
    async _navigate(url) {
        this._ck();
        const sess = await this._ensureSession();
        const cookie = await this.jar.getCookieString(url).catch(() => '');
        // 导航是幂等的（纯 GET，不改服务端状态）⇒ 任何临时网络错误都可以重试
        const { r, text } = await this._withNetRetry('导航', async () => {
            const navHeaders = {
                // ★2026-09-18 补 sec-ch-ua 三件套：理由同 preflight 里那段——顶层导航 GET 缺 client hints
                //   是 CF 抓自动化的现成特征，而 /oauth/authorize 的整条跳转链全走这里（bootstrapAuthorize
                //   ≤10 跳）。headerOrderFor 会按 NAVIGATION_ORDER 把它们摆到最前，不动排序逻辑。
                ...this._clientHints(),
                'user-agent': this.userAgent,
                'accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8',
                'accept-encoding': 'gzip, deflate, br, zstd',   // 见父类 _headers 的注释
                'accept-language': this.profile.acceptLanguage,
                'upgrade-insecure-requests': '1',
                'sec-fetch-site': 'same-origin',
                'sec-fetch-mode': 'navigate',
                'sec-fetch-dest': 'document',
                'sec-fetch-user': '?1',
                ...(cookie ? { cookie } : {}),
            };
            const resp = await sess.get(url, {
                headers: navHeaders,
                headerOrder: headerOrderFor(Object.keys(navHeaders), { navigation: true }),
            });
            const body = await resp.text().catch(() => '');
            // ★ node-tls-client 传输失败不抛异常，而是 resolve 出 status=0（Go 错误写在 body 里）。
            //   不转成异常的话上面这层 _withNetRetry 一次都不会触发，status=0 会一路漏到
            //   bootstrapAuthorize 的跳转循环，最后伪装成 PURE_BOOTSTRAP_NO_SESSION ——
            //   一个与真因（网络断了）毫不相干、还会误导排查的死因。
            //   这里是纯 GET、幂等，classifyNetError 判 connect/transfer 都会重试。
            if (resp.status === 0) {
                // ★★★ 2026-08-20：Go 的错误格式是 `failed to do request: GET "<URL>": <原因>`
                //   —— **原因在最末尾**。这里原来写 body.slice(0, 200) 截头部，而 authorize 的 URL
                //   光 query 就 390+ 字符，截出来全是 URL、把唯一的判据 `: EOF` 正好切掉了 ⇒
                //   classifyNetError 两条正则都不命中、返回空 ⇒ _withNetRetry 判不可重试 ⇒ **0 次重试**，
                //   跟没修一样一击即死（实测 addysonmanik99520@outlook.com 的 RT attempt 2/3）。
                //   两道保险，缺一不可：
                //     ① 显式打 netKind —— 判重不重试**不再依赖文案**，任何截断都影响不到它；
                //     ② 摘要保留【尾部】—— 人看日志时也要能看见真正的原因。
                //   status=0 归 transfer 而不是 connect：请求可能已经送到服务端，只有幂等请求该重试
                //   （本处是纯 GET 导航，安全）。分档语义与本文件顶部 CONNECT_PHASE/MID_TRANSFER 一致。
                const reason = String(body).trim();
                const brief = reason.length > 220
                    ? reason.slice(0, 70) + ' …… ' + reason.slice(-130)
                    : reason;
                const err = new Error(`导航传输失败(HTTP 0): ${brief || '(空响应体)'}`);
                err.netKind = 'transfer';
                throw err;
            }
            return { r: resp, text: body };
        }, true);
        await this._absorb(r, url);
        if (this._isCf(r.status, text)) {
            throw codedError('CLOUDFLARE_BLOCK',
                `CLOUDFLARE_BLOCK: 导航被 Cloudflare 挑战(HTTP ${r.status})`);
        }
        const rawLoc = r.headers && (r.headers['location'] || r.headers['Location']);
        const location = Array.isArray(rawLoc) ? rawLoc[0] : rawLoc;
        return { status: r.status, location: location ? String(location) : undefined, text };
    }

    /**
     * @param {string} authUrl
     * @param {object} [opts]
     *   authParams  追加到 authorize URL 上的 query（已存在的键**不覆盖**，与本方法既有的
     *               「没有才补」口径一致）。给「加密码」那条链用，见下面 promptLogin 的说明。
     *   promptLogin 传 false 时【不补】prompt=login。默认 true = 原行为。
     */
    async bootstrapAuthorize(authUrl, opts = {}) {
        this._ck();
        await this._ensureDeviceCookie();
        await this.preflight();
        // 补齐同源上下文参数，保持 oai-did 在 URL / Cookie / Sentinel 三处一致。
        // ★prompt=login 必须有：缺了它服务端 auth session 不会停在「等用户名」这一步，
        //   随后 authorize/continue 会被判 invalid_auth_step（实测 HTTP 400）。
        //   项目里 oauthService.getAuthUrl() 不带 prompt（浏览器链路自己会走到登录页），
        //   所以这里统一补，避免将来接 OAuthService 时再踩一次。
        //
        // ★★ 但「注册后加密码」那条链**必须关掉它**（promptLogin:false）。
        //   2026-09-16 全流程抓包（真人从 chatgpt.com 设置页点“添加密码”）显示，那条链的
        //   authorize 带的是 reauth=password & max_age=0 & post_login_add_password=true，
        //   **没有 prompt**。两者语义正相反：prompt=login 说“当成一次全新登录”，
        //   reauth=password 说“这是同一个人的重认证”。同时发出去，服务端按全新登录处理，
        //   于是把 post-login 的每一步都走一遍 —— 实测就卡在 add_phone，永远到不了密码页。
        let url = authUrl;
        try {
            const u = new URL(authUrl);
            if (!u.searchParams.has('ext-oai-did')) u.searchParams.set('ext-oai-did', this.deviceId);
            if (opts.promptLogin !== false && !u.searchParams.has('prompt')) {
                u.searchParams.set('prompt', 'login');
            }
            for (const [k, v] of Object.entries(opts.authParams || {})) {
                if (!u.searchParams.has(k)) u.searchParams.set(k, String(v));
            }
            url = u.toString();
        } catch (e) {}

        // CF 对 /oauth/authorize 会间歇性挑战。
        // ★重试前必须**换出口 IP**：CF 的挑战是绑在 IP 信誉上的，
        //   同一个被挑战的 IP 再等再试还是被挑战，纯浪费时间。
        //   实测 kookeey 的 CF 失败集中在特定 ASN（AS2516 KDDI），换一个出口就过了。
        //   没有 rotateProxy（比如本机直连）时退回原行为：只退避 + 重新预热。
        const maxAttempts = Number(this.cfMaxAttempts || 4);
        let lastError = null;
        for (let attempt = 1; attempt <= maxAttempts; attempt++) {
            this._ck();
            try {
                this.log(`[pure] 跟随 authorize URL 建立会话…（尝试 ${attempt}/${maxAttempts}）`);
                let cur = url;
                let hops = 0;
                for (let hop = 0; hop < 10; hop++) {
                    this._ck();
                    const r = await this._navigate(cur);
                    if (r.status >= 300 && r.status < 400 && r.location) {
                        cur = r.location.startsWith('http')
                            ? r.location
                            : new URL(r.location, cur).toString();
                        hops++;
                        continue;
                    }
                    break;
                }
                const cookie = await this.jar.getCookieString(AUTH_BASE);
                // ★★ 2026-09-26：服务端把这个 cookie 换名字了。实测跟完 authorize 的 3 跳、
                //    落到 /log-in HTTP 200（78KB 真实登录页）之后，jar 里是：
                //      oai-did / oaicom-stable-id / __cf_bm / __cflb / _cfuvid / rg_context /
                //      iss_context / **auth-session-minimized** / auth-session-minimized-client-checksum /
                //      oai-login-csrf_dev_*
                //    —— 没有 oai-client-auth-session。也就是说会话**建成了**，只是名字变了，
                //    而这道闸按老名字找，于是把一次成功判成失败，整条 RT 从此卡死在第①步。
                //    （这与 Cloudflare 无关：同一趟的预热是 [200,200]，直连和走代理表现一致。）
                // ★ 认哪几个名字放在一处，将来再改名只改这一行；顺带保留老名字 —— 服务端可能在灰度，
                //   两种形态都可能出现。
                const sessionCookieNames = ['oai-client-auth-session', 'auth-session-minimized'];
                const seen = sessionCookieNames.find(name => cookie.includes(name));
                if (!seen) {
                    // ★ 报错里带上**当时 jar 里到底有什么**。上一版只说「没拿到 X」，
                    //   于是「服务端改名」和「会话真没建起来」在日志里长得一模一样 ——
                    //   这正是它能卡住一整天的原因。
                    const names = cookie.split(';').map(x => x.trim().split('=')[0]).filter(Boolean);
                    throw codedError('PURE_BOOTSTRAP_NO_SESSION',
                        'authorize 之后没拿到会话 cookie（认得的名字：'
                        + `${sessionCookieNames.join(' / ')}）—— 当时 jar 里是：${names.join(', ') || '(空)'}`);
                }
                if (seen !== 'oai-client-auth-session') {
                    this.log(`[pure] 会话 cookie 是 ${seen}（不是老的 oai-client-auth-session）`);
                }
                // ★落点必须已经离开 /oauth/authorize（正常是 /log-in 或 /log-in-or-create-account）。
                //   还停在 authorize = 服务端返回了 200 而不是 302，auth session 卡在第 0 步，
                //   此时即便有 cookie，authorize/continue 也会 invalid_auth_step。宁可现在报错也别往下走。
                if (/\/oauth\/authorize/.test(cur)) {
                    throw codedError('PURE_BOOTSTRAP_NOT_ADVANCED',
                        `authorize 未发生跳转（落点仍是 ${cur.slice(0, 80)}），auth session 未推进到登录步`);
                }
                this.log(`[pure] 会话就绪（${hops} 跳，落点 ${cur.replace(AUTH_BASE, '').slice(0, 48)}）`);
                return true;
            } catch (e) {
                lastError = e;
                const msg = String(e.message || '');
                // CF 挑战、以及"拿不到 auth-session cookie"（多半也是被挑战/风控降级）都值得换个出口重试
                // ★2026-08-03 修：原来只测 msg。但 codedError(code,message)（本文件 :25-29）把标记放在
                //   **e.code** 上，PURE_BOOTSTRAP_NO_SESSION 的 message 是纯中文「authorize 之后没拿到
                //   oai-client-auth-session cookie ——…」，永远匹配不上 ⇒ 这条「换出口重试 4 次」的路对它是**死的**，
                //   一撞就直接 blocked（CF 那条能中只是因为它的 message 恰好以 CLOUDFLARE_BLOCK: 开头）。
                //   现在 code 和 message 都测。
                const retryKey = `${e.code || ''} ${msg}`;
                // ★两类失败要**分开治**（2026-09-22）：
                //   · CLOUDFLARE_BLOCK：真被 CF 挡（403）。换出口 IP 有用（新 IP 的信誉不同），
                //     换成新出口后必须重打预热（新 IP 没有 __cf_bm，不预热直接 authorize 又会被挑战）。
                //   · PURE_BOOTSTRAP_NO_SESSION：authorize 回了 200、却没吐 oai-client-auth-session cookie。
                //     实测这【不是】IP/CF 问题——预热常常是好的（cf_bm 已拿到、200,200）却照样拿不到 cookie。
                //     对它换 IP 无用、重打预热更是白惹一次 CF、还丢掉同出口已有的 cf_bm。所以只做**短退避、
                //     原地再敲一次**，把「换出口 + 重预热」这剂药留给真正的 CF。修不好由外层换新 worker 兜底。
                //   ★2026-08-03 的口径保留：标记可能在 e.code（纯中文 message 匹配不上），故 code+message 都测。
                const isCf = /CLOUDFLARE_BLOCK/.test(retryKey);
                const isNoSession = /PURE_BOOTSTRAP_NO_SESSION/.test(retryKey);
                if ((!isCf && !isNoSession) || attempt >= maxAttempts) throw e;

                if (isNoSession && !isCf) {
                    // 短退避、原地重试：不换出口、不重打预热（保住同出口的 cf_bm）。
                    // 抖动同样 ±25%，避免同刻被挡的多个 worker 踩同一节拍齐射。
                    const backoff = Math.round(700 * (0.75 + Math.random() * 0.5));
                    this.log(`[pure] authorize 拿不到 session（非 CF，换 IP/重预热均无用）→ ${backoff}ms 后原地重试 ${attempt + 1}/${maxAttempts}…`);
                    await new Promise(r => setTimeout(r, backoff));
                    continue;
                }

                // ★真被 CF 挡：换出口 IP，退避后（只有真换成新出口才）重预热再试。
                //   退避加抖动（±25%）。原来是死板的 2000/4000/6000ms：**同一瞬间**被挡的多个 worker
                //   会踩着同一个节拍一起回来重试，等于把重试也变成一次小型齐射。
                //   刻意不复用 adaptiveConcurrency 的 jitteredBackoff —— 那个自带 [2s,5s,15s,30s,60s]
                //   的曲线，直接换过去会把 2/4/6s 悄悄变成 2/5/15s，是行为改动而不是加抖动。
                const backoff = Math.round(2000 * attempt * (0.75 + Math.random() * 0.5));
                let switched = false;
                if (this.rotateProxy) {
                    try {
                        const next = await this.rotateProxy(attempt);
                        if (next) {
                            await this._switchProxy(next);
                            switched = true;
                        }
                    } catch (re) {
                        this.log(`[pure] 换出口失败（继续用原出口重试）：${String(re.message).slice(0, 90)}`);
                    }
                }
                this.log(`[pure] authorize 被 CF 挡(${msg.slice(0, 40)})，`
                    + `${switched ? '已换出口 IP，' : ''}${backoff}ms 后${switched ? '重新预热并' : ''}重试 ${attempt + 1}/${maxAttempts}…`);
                await new Promise(r => setTimeout(r, backoff));
                // ★只有真换成新出口才重预热（新 IP 需要自己的 cf_bm）；没换成（rotateProxy 失败/本机直连）
                //   就沿用同出口的 cf_bm，不再白探一次 CF——这一条同时也堵住了「每重试必重预热」的放大器。
                if (switched) await this.preflight();
            }
        }
        throw lastError || codedError('PURE_BOOTSTRAP_FAILED', 'authorize 建会话失败');
    }

    /**
     * ② 提交邮箱 —— 触发 OpenAI 发验证码邮件。
     *    ★调用前务必先 mailProvider.captureBaseline()，否则可能取到收件箱里的旧码。
     */
    async submitEmail(email, opts = {}) {
        // autoPassword=false：落到密码页时**不**自动提交密码，原样返回。
        //   密码自愈流程（forgot-password）要先干净地停在 login_password 步再去触发 send-otp，
        //   若这里自动提交那个已知不对的密码，会先撞一次 401、把会话弄脏。
        const autoPassword = opts.autoPassword !== false;
        const res = await this._postWithSentinel(
            '/api/accounts/authorize/continue',
            { username: { kind: 'email', value: email } },
            'authorize_continue',
            AUTH_BASE + '/log-in',
        );
        if (res.status !== 200 && res.status !== 204) {
            const body = String(res.text || '').slice(0, 300);
            const d = res.data || {};
            const err = d.error || {};
            const message = String(err.message || d.message || d.detail || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            const hay = `${code} ${message}`.toLowerCase();
            // 账号已废优先判定（口径对齐 accountDetector.js），别让它掉进通用失败里被无谓重试
            if (code === 'account_deactivated' || /deactiv|deleted|suspend|banned/i.test(hay)) {
                throw codedError('ACCOUNT_UNUSABLE',
                    `账号已废（${code || 'deactivated'}）: HTTP ${res.status} ${message || body}`);
            }
            if (/sentinel|proof.?of.?work|turnstile|captcha/i.test(body)) {
                throw codedError('SENTINEL_REJECTED',
                    `SENTINEL_REJECTED: authorize/continue 拒绝了 sentinel token(HTTP ${res.status}): ${body}`);
            }
            throw codedError('PURE_SUBMIT_EMAIL_FAILED',
                `提交邮箱失败 HTTP ${res.status}: ${message || body}`);
        }
        const pageType = String((res.data && res.data.page && res.data.page.type) || '');
        this.log(`[pure] 邮箱已提交 → page.type=${pageType || '(无)'}`);
        this._logAuthSession('authorize/continue', res.data);

        // ★★ 关键：HTTP 200 ≠ 验证码已发出。
        //   OpenAI 会**动态决定**给你「邮箱验证码页」还是「密码页」
        //   （Statsig 里有 auto_passwordless_after_failed_attempts / default_to_passwordless_otp_login
        //     这类开关，同一个账号不同时候落点可能不同）。
        //   落到 login_password 时服务端根本没发信 —— 浏览器链路会去点
        //   `intent=passwordless_login_send_otp` 那个「使用一次性验证码」按钮来触发发码，
        //   纯协议没有浏览器点不了，原来就直接去等 200 秒 → 必然 outlook_otp_timeout。
        //   这就是「手动登录邮箱明明正常、协议却说收不到码」的真相：**信压根没发**。
        if (pageType && !/email_otp|otp_verification|verification/i.test(pageType)) {
            if (/login_password|password/i.test(pageType)) {
                if (!autoPassword) {
                    // 自愈流程要的就是这个：干净地停在密码页，由上层去触发 forgot-password。
                    this.log('[pure] 落点是密码页（autoPassword=false，交给上层处理）');
                    this.lastOtpResponse = res.data;
                    return res.data;
                }
                // ★2026-08-03 真流量抓包（logs/authflow/）确认的正解：
                //   服务端要密码时，不是去点什么「使用一次性验证码」，
                //   而是 POST /api/accounts/password/verify {"password":"..."}，
                //   **密码验过之后服务端才自动发验证码邮件**，页面随即变成 email_otp_verification。
                //   之前误以为要触发 passwordless、去调 email-otp/send，
                //   结果会话还停在"等密码"这步 ⇒ invalid_auth_step ⇒ 一封信都没发出去过。
                this.log('[pure] 落点是密码页 → 提交密码（密码验过服务端才发验证码）');
                const after = await this.submitPassword();
                return after;
            }
            // ★2FA 号可能在提交邮箱后直接落到 mfa_challenge（passwordless+2FA）。就地应答再交给下一步。
            if (/^mfa|mfa_challenge|two[-_]?factor|totp/i.test(pageType)) {
                const answered = await this._answerMfaIfPresent(res.data);
                this.lastOtpResponse = answered;
                return answered;
            }
            // ★服务端可能直接跳过邮箱验证码这一步（同会话内邮箱已验过/走了别的信任路径），
            //   直接给 add_phone / consent。这些都是**合法的下一步**，不是异常。
            //   之前一律抛 PURE_UNEXPECTED_PAGE，等于把能成的账号判死。
            if (ADVANCED_PAGE.test(pageType)) {
                this.log(`[pure] 服务端跳过邮箱验证码，直接到 ${pageType} —— 不等码，交给下一步`);
                this.lastOtpResponse = res.data;
                return res.data;
            }
            throw codedError('PURE_UNEXPECTED_PAGE',
                `提交邮箱后落到未预期页面 ${pageType}，纯协议无法继续`);
        }
        return res.data;
    }

    /**
     * POST /api/accounts/password/verify —— 登录密码验证。
     * 端点/字段/flow 全部来自 2026-08-03 真流量抓包：
     *   body   : {"password": "..."}
     *   sentinel flow: **password_verify**（不是 authorize_continue）
     *   referer: https://auth.openai.com/log-in/password
     * 成功后响应 page.type = email_otp_verification，服务端此时才发验证码邮件。
     */
    async submitPassword(password, opts = {}) {
        const pwd = String(password || this.accountPassword || '').trim();
        if (!pwd) {
            throw codedError('PURE_PASSWORD_REQUIRED',
                '服务端要求密码，但任务里没带账号密码（accounts.extra_json.password）——无法继续');
        }
        const res = await this._postWithSentinel(
            '/api/accounts/password/verify',
            { password: pwd },
            'password_verify',
            AUTH_BASE + '/log-in/password',
        );
        if (res.status !== 200) {
            const d = res.data || {};
            const err = d.error || {};
            const message = String(err.message || d.message || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            const type = String(err.type || d.type || '').toLowerCase();
            const hay = `${code} ${message}`.toLowerCase();
            // ★把服务端自己的判据打出来。message 恒定是 "Login failed. Please try again or seek
            //   help at https://help.openai.com/" —— 密码不对、风控拒登、限流锁定在这句话上**长得完全一样**，
            //   能归因的信息全在 error.code / type / 原始 body 里，而它们原来一个都没进日志。
            //   实测 saucer-secular0o@icloud.com 连撞 30 次 401，只能靠反推「走的是哪个分支」猜真因。
            //   只打服务端回的内容；提交的密码绝不进日志。
            this.log(`[diag] password/verify 失败 HTTP ${res.status}`
                + ` code=${code || '(无)'} type=${type || '(无)'}`
                + ` body=${String(res.text || '').replace(/\s+/g, ' ').trim().slice(0, 200) || '(空)'}`);
            const detail = `${message || String(res.text || '').slice(0, 200)}`
                + (code ? `（code=${code}）` : '');
            if (code === 'account_deactivated' || /deactiv|deleted|suspend|banned/i.test(hay)) {
                throw codedError('ACCOUNT_UNUSABLE', `账号已废（${code || 'deactivated'}）: ${message}`, res.status);
            }
            // ★带上 res.status：没有它，classifyRtError 只能拿文案去猜，
            //   而 OpenAI 那句 "Please try again" 会命中给 SMSPool 写的可重试规则 ⇒ 一个改不掉的
            //   密码错被判成 transient_error 无限重跑（见 rtAutomation.js 里 account_password_invalid 那条）。
            if (/password|credential|incorrect|invalid/i.test(hay)) {
                throw codedError('PURE_PASSWORD_INVALID',
                    `密码验证失败 HTTP ${res.status}: ${detail}`, res.status);
            }
            throw codedError('PURE_PASSWORD_VERIFY_FAILED',
                `password/verify 失败 HTTP ${res.status}: ${detail}`, res.status);
        }
        const pt = String((res.data && res.data.page && res.data.page.type) || '');
        this.log(`[pure] ✔ 密码已验证 → page.type=${pt || '(无)'}（服务端此刻发出验证码邮件）`);
        this._logAuthSession('password/verify', res.data);
        // ★ opts.raw：调用方（accountRelogin 的 page.type 状态机）自己按落点派发，不走下面这道
        //   「只认验证码页」的门禁 —— 2FA 登录里密码验完的合法落点是 mfa_challenge，
        //   会被这道门禁误判成 PURE_UNEXPECTED_PAGE。默认 raw=false，主链路（submitEmail 的自动提交）行为逐字不变。
        if (opts.raw) { this.lastOtpResponse = res.data; return res.data; }
        // ★2FA 登录：密码验完的合法落点就是 mfa_challenge。有 totpSecret 就地应答，返回应答后的页面
        //   （consent / 邮箱验证码页），让主链路 ④/⑤ 照常继续；孤儿号（无 secret）在此明确停手。
        const data = await this._answerMfaIfPresent(res.data);
        const landed = String((data && data.page && data.page.type) || '');
        if (landed && !/email_otp|otp_verification|verification/i.test(landed)) {
            // 同 submitEmail：密码验完直接给 add_phone / consent 是合法路径（服务端认为邮箱不用再验），
            // 不该判死。只有真正认不出的页面才抛。
            if (ADVANCED_PAGE.test(landed)) {
                this.log(`[pure] 密码验完直接到 ${landed} —— 不等码，交给下一步`);
                this.lastOtpResponse = data;
                return data;
            }
            throw codedError('PURE_UNEXPECTED_PAGE',
                `密码验证通过但落点是 ${landed}，不是验证码页`);
        }
        return data;
    }

    /**
     * 内部小工具：先裸发（无 sentinel），若被判要 sentinel/挑战再补一发带 sentinel 的。
     * ★send-otp 实测（2026-08-07 probe_pwdpage_otp）裸发即 200；但 password/reset 是安全动作，
     *   服务端**可能**要 sentinel。用这个包装对两种要求都稳，不必事先赌对。
     * @param {string} sentinelFlow 需要 sentinel 时用的 flow（password_verify 语义最接近重设/发码）
     */
    async _postMaybeSentinel(path, body, sentinelFlow, referer) {
        const first = await this._post(path, body);
        // ★检测口径对齐 submitEmail：只认 sentinel/turnstile/pow/captcha，**不认裸 "challenge"**——
        //   否则会与 MFA 的 challenge_required 撞车，把要 2FA 的 403 误当成"要补 sentinel"。
        const needSentinel = (first.status === 400 || first.status === 403)
            && /sentinel|turnstile|proof.?of.?work|captcha/i.test(String(first.text || ''));
        if (!needSentinel) return first;
        this.log(`[pure] ${path} 被要求 sentinel，补一发带 token 的`);
        return this._postWithSentinel(path, body, sentinelFlow, referer);
    }

    /**
     * 「忘记密码」第 1 步：触发重设验证码邮件。
     * 端点/形态来自 2026-08-07 bundle 逆向 + 实测（probe_pwdpage_otp --try-reset）：
     *   POST /api/accounts/password/send-otp   body: {}（空！origin_page_type/intent 是前端字段，不进 wire body）
     *   会话须停在 login_password 步（submitEmail(email,{autoPassword:false}) 后即是）。
     *   成功回 page.type=email_otp_verification、payload.email_verification_mode=password_reset、
     *   continue_url=/email-verification —— 之后用现成的 submitEmailOtp（email-otp/validate）验码。
     * @returns {object} 响应 data，供上层记 baseline 后等码
     */
    async sendPasswordResetOtp() {
        const res = await this._postMaybeSentinel(
            '/api/accounts/password/send-otp', {}, 'password_verify', AUTH_BASE + '/reset-password');
        if (res.status !== 200 && res.status !== 204) {
            const d = res.data || {}; const err = d.error || {};
            const message = String(err.message || d.message || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            const hay = `${code} ${message}`.toLowerCase();
            if (code === 'account_deactivated' || /deactiv|deleted|suspend|banned/i.test(hay)) {
                throw codedError('ACCOUNT_UNUSABLE', `账号已废（${code || 'deactivated'}）: ${message}`, res.status);
            }
            throw codedError('PURE_PWRESET_SEND_FAILED',
                `触发重设验证码失败 HTTP ${res.status}: ${message || String(res.text || '').slice(0, 200)}`
                + (code ? `（code=${code}）` : ''), res.status);
        }
        const pt = String((res.data && res.data.page && res.data.page.type) || '');
        const mode = String((res.data && res.data.page && res.data.page.payload
            && res.data.page.payload.email_verification_mode) || '');
        this.log(`[pure] ✔ 已触发重设验证码 → page.type=${pt || '(无)'} mode=${mode || '(无)'}`);
        this._logAuthSession('password/send-otp', res.data);
        return res.data;
    }

    /**
     * 「忘记密码」最后一步：把密码重设为我们已知的库内统一密码。
     * 端点/形态来自 bundle 逆向：POST /api/accounts/password/reset  body: {"password": "..."}
     *   （前置：send-otp → email-otp/validate 已在同会话内验过重设码）。
     * ★这是不可逆的账号变更；只在上层确认要自愈时才调。成功后账号密码 = 库内密码，
     *   后续用同一个密码走正常 password/verify 就能登进去。密码绝不进日志。
     * @returns {object} 响应 data
     */
    async resetPassword(newPassword) {
        const pwd = String(newPassword || this.accountPassword || '').trim();
        if (!pwd) {
            throw codedError('PURE_PWRESET_NO_PASSWORD',
                '要重设密码但没有目标密码（accounts.extra_json.password 为空）——无法自愈');
        }
        const res = await this._postMaybeSentinel(
            '/api/accounts/password/reset', { password: pwd },
            'password_verify', AUTH_BASE + '/reset-password/new-password');
        if (res.status !== 200 && res.status !== 204) {
            const d = res.data || {}; const err = d.error || {};
            const message = String(err.message || d.message || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            const hay = `${code} ${message}`.toLowerCase();
            this.log(`[diag] password/reset 失败 HTTP ${res.status}`
                + ` code=${code || '(无)'} body=${String(res.text || '').replace(/\s+/g, ' ').trim().slice(0, 200) || '(空)'}`);
            // 重设时被要求 2FA/额外验证：纯协议做不了，交人工（口径与 add-phone 后的 MFA 一致）
            if (/mfa|two[-_]?factor|totp|challenge_required|password_reset_required/i.test(hay)) {
                throw codedError('ACCOUNT_MFA_REQUIRED',
                    `重设密码时服务端要求额外验证（${code || 'mfa'}）：${message}`, res.status);
            }
            if (/password_already_used|same.?password/i.test(hay)) {
                // 目标密码与账号当前密码相同 —— 说明密码其实是对的，重设不是解药，交人工看
                throw codedError('PURE_PWRESET_ALREADY_USED',
                    `重设失败：新密码与账号当前密码相同（${code}）——密码并非病因`, res.status);
            }
            throw codedError('PURE_PWRESET_FAILED',
                `重设密码失败 HTTP ${res.status}: ${message || String(res.text || '').slice(0, 200)}`
                + (code ? `（code=${code}）` : ''), res.status);
        }
        const pt = String((res.data && res.data.page && res.data.page.type) || '');
        this.log(`[pure] ✔ 密码已重设 → page.type=${pt || '(无)'}`);
        this._logAuthSession('password/reset', res.data);
        return res.data;
    }

    // ══════════ 「注册后给 passwordless 号补密码」这条链的三个端点 ══════════
    // 全部逐字来自 2026-09-16 全流程抓包（真人从 chatgpt.com 设置页点「密码 → 添加」）。
    // 整条链的顺序与前置条件写在 src/accountPassword.js 的文件头，改这里之前先看那份。
    //
    // ★ 与上面「忘记密码」那条（send-otp → reset）是**两条不同的链**，别混：
    //   reset 那条要求号本来就有密码、会话停在密码页；passwordless 的号没有密码页，
    //   password/send-otp 对它恒回 HTTP 400 invalid_auth_step。

    /**
     * 取 2FA 挑战（邮箱码验完、服务端把会话导到 mfa_challenge 之后）。
     *   POST /api/accounts/mfa/issue_challenge  {id, type, force_fresh_challenge}
     * ★抓包里这个端点**不带** sentinel —— 别"顺手补上"，多发一个 token 同样是特征。
     *   referer=/email-verification 由 API_REFERER 表给（rtProtocolClient.js）。
     * @param {string} factorId 服务端在 mfa_challenge 的 payload 里给的那个 id
     */
    async issueMfaChallenge(factorId, opts = {}) {
        const id = String(factorId || '').trim();
        if (!id) throw codedError('PURE_MFA_NO_FACTOR', '取 2FA 挑战需要 factor id，但没给');
        const res = await this._post('/api/accounts/mfa/issue_challenge', {
            id,
            type: String(opts.type || 'totp'),
            // 抓包是 false。true 会让服务端重发一个新挑战 —— 对 TOTP 没意义（码是本地算的），
            // 对短信/邮件类 factor 则等于多发一条，所以默认保持 false。
            force_fresh_challenge: opts.forceFresh === true,
        });
        if (res.status !== 200 && res.status !== 204) {
            const d = res.data || {}; const err = d.error || {};
            const message = String(err.message || d.message || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            throw codedError('PURE_MFA_ISSUE_FAILED',
                `取 2FA 挑战失败 HTTP ${res.status}: ${message || String(res.text || '').slice(0, 200)}`
                + (code ? `（code=${code}）` : ''), res.status);
        }
        this.log('[pure] 已取 2FA 挑战');
        this._logAuthSession('mfa/issue_challenge', res.data);
        return res.data;
    }

    /**
     * 提交 TOTP 码回答 2FA 挑战。
     *   POST /api/accounts/mfa/verify  {id, type, code}
     * ★这个端点也**不带** sentinel，但 referer 是 /mfa-challenge/<factor_id> —— **带动态段**，
     *   登记不进 API_REFERER 静态表，所以这里显式传（_post 的第三参会覆盖表里的值）。
     *   少了那段 id 的 referer 是真浏览器发不出来的，属于白送的自动化特征。
     * ⚠ 码错了会消耗 MFA 尝试次数，次数多了服务端会锁 —— 调用方必须先确认
     *   手上的 secret 与服务端要的 factor 是同一个（见 accountPassword 的 PWD_FACTOR_MISMATCH）。
     */
    async verifyMfaTotp(factorId, code, opts = {}) {
        const id = String(factorId || '').trim();
        if (!id) throw codedError('PURE_MFA_NO_FACTOR', '提交 2FA 码需要 factor id，但没给');
        const c = String(code || '').trim();
        if (!c) throw codedError('PURE_MFA_NO_CODE', '没有可提交的 2FA 码');
        const res = await this._post('/api/accounts/mfa/verify',
            { id, type: String(opts.type || 'totp'), code: c },
            { referer: AUTH_BASE + '/mfa-challenge/' + id });
        if (res.status !== 200 && res.status !== 204) {
            const d = res.data || {}; const err = d.error || {};
            const message = String(err.message || d.message || '').trim();
            const errCode = String(err.code || d.code || '').toLowerCase();
            const hay = `${errCode} ${message}`.toLowerCase();
            // 码不对单独成码：它是**可以重试**的（等下一个周期再算一个），
            // 与"这个号的 2FA 根本对不上"必须分开，否则上层无从判断该不该再试。
            if (/invalid.{0,12}code|incorrect|expired|wrong.{0,8}code/i.test(hay)) {
                throw codedError('PURE_MFA_CODE_INCORRECT',
                    `2FA 码无效/过期 HTTP ${res.status}: ${message}`, res.status);
            }
            if (/too.{0,10}many|rate.?limit|locked/i.test(hay)) {
                throw codedError('PURE_MFA_LOCKED',
                    `2FA 尝试次数被限 HTTP ${res.status}: ${message}（别再重试，会越锁越久）`, res.status);
            }
            throw codedError('PURE_MFA_VERIFY_FAILED',
                `2FA 校验失败 HTTP ${res.status}: ${message || String(res.text || '').slice(0, 200)}`
                + (errCode ? `（code=${errCode}）` : ''), res.status);
        }
        const pt = String((res.data && res.data.page && res.data.page.type) || '');
        this.log(`[pure] ✔ 2FA 已通过 → page.type=${pt || '(无)'}`);
        this._logAuthSession('mfa/verify', res.data);
        return res.data;
    }

    /**
     * 应答一次 2FA 挑战（邮箱码验完、服务端把会话导到 mfa_challenge 之后调用）。
     *   从挑战 payload 取 factor → 与库里 totpFactorId 核对 → issue_challenge → 算 TOTP 码 → verify。
     * ★安全不变量（与 accountAddPasswordWeb 的 ⑧⑨ 一致）：
     *   · 没 totpSecret（孤儿号）→ 停，一个 verify 都不发（算不出码，硬发只会消耗尝试次数/锁号）。
     *   · 服务端要的 factor ≠ 库里存的 → 停，绝不提交（同上）。
     * @param {object} mfaResponse page.type=mfa_challenge 的那个响应
     * @returns {object} verify 之后的响应（page.type 变 consent/phone/reset…），并已写入 lastOtpResponse
     */
    async answerMfaChallenge(mfaResponse, opts = {}) {
        const secret = String(this.totpSecret || '').trim();
        if (!secret) {
            throw codedError('PURE_MFA_NO_SECRET',
                '账号开了 2FA，但库里没有 totpSecret —— 算不出 2FA 码，无法应答（孤儿号）');
        }
        const page = (mfaResponse && mfaResponse.page) || {};
        const payload = page.payload || {};
        const factors = Array.isArray(payload.factors) ? payload.factors : [];
        const totpFactor = factors.find(f => String((f && f.factor_type) || '').toLowerCase() === 'totp');
        const serverFactorId = String(payload.factor_id || (totpFactor && totpFactor.id) || '').trim();
        if (!serverFactorId) throw codedError('PURE_MFA_NO_FACTOR', '2FA 挑战里没有 factor_id');
        // ★服务端要的 factor 与库里存的不是同一个 → 硬提交只会消耗 MFA 尝试次数、可能锁号。
        if (this.totpFactorId && this.totpFactorId !== serverFactorId) {
            throw codedError('PURE_MFA_FACTOR_MISMATCH',
                '服务端要的 factor 与库里存的不是同一个 —— 拒绝提交，避免消耗 MFA 尝试次数/锁号');
        }
        await this.issueMfaChallenge(serverFactorId, opts);
        // 避开周期边缘：码只剩 <3s 就等到下一个窗口，别提交一个即将过期的码（省一次尝试次数）
        try {
            const left = msUntilNextStep();
            if (left > 0 && left < 3000) await new Promise(r => setTimeout(r, left + 300));
        } catch (_) {}
        let code = '';
        try {
            code = this._totpFn(secret);
        } catch (e) {
            throw codedError('PURE_MFA_TOTP_CALC_FAILED', `用 secret 算 TOTP 码失败：${e && e.message}`);
        }
        const data = await this.verifyMfaTotp(serverFactorId, code, opts);
        this.lastOtpResponse = data;   // verifyMfaTotp 不自更新 lastOtpResponse，这里补上供下游重新分类
        this.log('[pure] 2FA 已用库内 totpSecret 应答通过');
        return data;
    }

    /**
     * 若响应落在 2FA 挑战页，就地用库内 totpSecret 应答并返回应答后的响应；否则原样返回。
     *   给 submitEmail / submitPassword 用 —— MFA 可能在「密码验完」这一步就冒出来（有密码的硬化号），
     *   在这里应答后，主链路 ④/⑤ 就能照常处理应答后的落点（consent / 邮箱验证码页）。
     *   孤儿号（有服务端 2FA、库里没 secret）→ 明确抛 PURE_MFA_NO_SECRET 停手，别再往下烧。
     */
    async _answerMfaIfPresent(data) {
        const pt = String((data && data.page && data.page.type) || '');
        if (!/^mfa|mfa_challenge|two[-_]?factor|totp/i.test(pt)) return data;
        if (!this.totpSecret) {
            throw codedError('PURE_MFA_NO_SECRET',
                `落到 2FA 挑战(${pt})但库里没有 totpSecret（孤儿号）—— 算不出码，无法继续`);
        }
        this.log(`[pure] 落点是 2FA 挑战(${pt}) → 用库内 totpSecret 应答`);
        return await this.answerMfaChallenge(data);
    }

    /**
     * 给一个本来没有密码的号【添加】密码（不是重设）。
     *   POST /api/accounts/password/add  {password}
     *
     * ★★ 两个反直觉、但抓包实证的点：
     *   ① 上一步（mfa/verify）的 page.type 是 **reset_password_new_password**、
     *      continue_url 也指向 /reset-password/new-password —— 但端点是 password/**add**。
     *      前端在"加密码"场景复用了"重设密码"的页面组件。别按 page.type 去猜端点。
     *   ② sentinel 的 flow 也是 **password_reset**（抓包时序确认），不是 password_add。
     *   这两条都属于"看起来像写错了、其实是对的"，改之前请先重抓一份包。
     *
     * ★这是不可逆的账号变更。成功时服务端直接回 page.type=external_url，
     *   payload.url 里带着一个**全新的 authorization code** —— 跟随它就能换到新的 at/session cookie，
     *   所以设完密码并不需要再用"密码+2FA"重新登录一次。
     * 密码绝不进日志。
     */
    async addPassword(newPassword) {
        const pwd = String(newPassword || this.accountPassword || '').trim();
        if (!pwd) {
            throw codedError('PURE_PWADD_NO_PASSWORD', '要添加密码但没有目标密码 —— 无法继续');
        }
        const res = await this._postWithSentinel(
            '/api/accounts/password/add', { password: pwd },
            'password_reset', AUTH_BASE + '/reset-password/new-password');
        if (res.status !== 200 && res.status !== 204) {
            const d = res.data || {}; const err = d.error || {};
            const message = String(err.message || d.message || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            const hay = `${code} ${message}`.toLowerCase();
            this.log(`[diag] password/add 失败 HTTP ${res.status}`
                + ` code=${code || '(无)'} body=${String(res.text || '').replace(/\s+/g, ' ').trim().slice(0, 200) || '(空)'}`);
            if (code === 'account_deactivated' || /deactiv|deleted|suspend|banned/i.test(hay)) {
                throw codedError('ACCOUNT_UNUSABLE', `账号已废（${code || 'deactivated'}）: ${message}`, res.status);
            }
            // 号上已经有密码了 ⇒ 这条链用错了，该走 reset 那条。单独成码，别让上层盲目重试。
            if (/already.{0,12}(has|set|exists)|password_already|invalid_auth_step/i.test(hay)) {
                throw codedError('PURE_PWADD_NOT_APPLICABLE',
                    `password/add 不适用于这个号（${code || '会话步骤不对'}）：${message}`
                    + ' —— 号上可能已有密码，或会话没停在设密码页', res.status);
            }
            if (/weak|too.{0,10}(short|common)|password_strength/i.test(hay)) {
                throw codedError('PURE_PWADD_WEAK', `密码被判强度不足（${code}）`, res.status);
            }
            throw codedError('PURE_PWADD_FAILED',
                `添加密码失败 HTTP ${res.status}: ${message || String(res.text || '').slice(0, 200)}`
                + (code ? `（code=${code}）` : ''), res.status);
        }
        const pt = String((res.data && res.data.page && res.data.page.type) || '');
        this.log(`[pure] ✔ 密码已添加 → page.type=${pt || '(无)'}`);
        this._logAuthSession('password/add', res.data);
        return res.data;
    }

    /**
     * 主动触发「发送邮箱验证码」。
     * 用在服务端把我们带到密码页（而不是验证码页）时——
     * 等价于浏览器上点「使用一次性验证码登录」。
     * @returns {boolean} 是否确认已触发
     */
    async requestEmailOtp(fromPageType = '') {
        this._ck();
        // ★只用 GET /api/accounts/email-otp/send，而且**必须跟完重定向链**。
        //
        //   两个血的教训（2026-08-02 实测）：
        //   ① 别乱试其它端点。原来还试了 POST authorize/continue{intent:passwordless_login_send_otp}
        //      和 POST email-otp/send —— 两个都恒定 400，而 400 会把服务端的 auth session
        //      推到非法步骤，后面即使收到码、提交也会被判 `Invalid authorization step`。
        //      实测 webcam-orders.51：码明明收到了(406112)，validate 却 HTTP 400 挂掉。
        //   ② 必须跟重定向。这个 GET 返回 302，要一路跟到 /email-verification，
        //      auth session 才真正落在「等验证码」这一步。只看 302 就收手 = 会话卡在半路。
        //      turb 用的是 allow_redirects=True（core/openai_auth.py:435），同一个道理。
        const path = '/api/accounts/email-otp/send';
        try {
            let url = AUTH_BASE + path;
            let last = null;
            for (let hop = 0; hop < 10; hop++) {
                this._ck();
                const r = await this._navigate(url);
                last = r;
                this.log(`[pure] 触发发码 hop${hop}: HTTP ${r.status}`
                    + (r.location ? ` → ${String(r.location).replace(AUTH_BASE, '').slice(0, 56)}` : ''));
                if (r.status >= 400) {
                    this.log(`[pure] ✘ 触发发码失败 HTTP ${r.status}`);
                    return false;
                }
                if (r.status >= 300 && r.status < 400 && r.location) {
                    url = String(r.location).startsWith('http')
                        ? String(r.location)
                        : new URL(String(r.location), url).toString();
                    continue;
                }
                break;
            }
            // ★落点必须是验证码页才算成功。
            //   踩过：跳到 /error?payload=... 时错误页本身返回 HTTP 200，
            //   被当成"触发成功"→ 白等 200 秒 ×3 轮（实测 bye.motels_2r 耗掉 10 分钟）。
            //   /error 的 payload 里就写着真实原因，解出来打进日志，别让它静默。
            if (/\/error\b/.test(url)) {
                let reason = '';
                try {
                    const m = /payload=([^&]+)/.exec(url);
                    if (m) {
                        const j = JSON.parse(Buffer.from(decodeURIComponent(m[1]), 'base64').toString('utf8'));
                        reason = `${j.errorCode || j.kind || ''}`;
                    }
                } catch (e) {}
                this.log(`[pure] ✘ 触发发码被服务端拒绝：${reason || '未知'}（落点 /error）`);
                return false;
            }
            const landedOk = !!last && last.status < 400 && /email-verification|verify|otp/i.test(url);
            this.log(`[pure] ${landedOk ? '✔ 已触发邮箱验证码发送' : '⚠️ 触发后落点非验证码页'}，落点 ${url.replace(AUTH_BASE, '').slice(0, 70)}`);
            return landedOk;
        } catch (e) {
            this.log(`[pure] 触发发码异常：${String(e.message).slice(0, 100)}`);
            return false;
        }
    }

    /**
     * ③ 提交邮箱验证码。返回响应，供 classifyNextStep 判断下一步。
     */
    async submitEmailOtp(code) {
        // ★flow 用 email_otp_validate（真流量抓包确认）。
        //   原来沿用 authorize_continue 是错的——sentinel token 里的 flow 必须与端点匹配。
        const res = await this._postWithSentinel(
            '/api/accounts/email-otp/validate',
            { code: String(code) },
            'email_otp_validate',
            AUTH_BASE + '/email-verification',
        );
        if (res.status !== 200) {
            const body = String(res.text || '').slice(0, 300);
            const d = res.data || {};
            const err = d.error || {};
            // ★只看 message / code，别拿整个 body 做判断：
            //   body 里恒有 "type":"invalid_request_error"，用整体正则匹 /invalid/ 会把
            //   任何错误都误判成"验证码不对"，在生产里表现为对着已废账号反复重试、白烧邮件。
            const message = String(err.message || d.message || d.detail || '').trim();
            const code = String(err.code || d.code || '').toLowerCase();
            const hay = `${code} ${message}`.toLowerCase();

            // 账号已废：口径对齐 src/accountDetector.js:72 的 /deactiv|deleted|suspend/i
            if (code === 'account_deactivated' || /deactiv|deleted|suspend|banned/i.test(hay)) {
                throw codedError('ACCOUNT_UNUSABLE',
                    `账号已废（${code || 'deactivated'}）: HTTP ${res.status} ${message || body}`);
            }
            if (/invalid.{0,12}code|incorrect|expired|wrong.{0,8}code|code.{0,12}(invalid|incorrect|expired)/i.test(hay)) {
                throw codedError('EMAIL_OTP_INCORRECT',
                    `邮箱验证码无效/过期 HTTP ${res.status}: ${message || body}`);
            }
            throw codedError('PURE_EMAIL_OTP_FAILED',
                `邮箱 OTP 验证失败 HTTP ${res.status}: ${message || body}`);
        }
        this.lastOtpResponse = res.data;
        const pageType = res.data && res.data.page && res.data.page.type;
        this.log(`[pure] 邮箱 OTP 通过 → page.type=${pageType || '(无)'}`);
        this._logAuthSession('email-otp/validate', res.data);
        return res.data;
    }

    /**
     * 诊断：把 oai-client-auth-session 里跟"下一步能不能做"相关的字段打出来。
     * ★为什么需要它：add-phone/send 回 409 invalid_state 时，我们手上只有 page.type，
     *   完全看不出服务端认为这个会话处于什么状态。而浏览器链路对照实验（2026-08-03，6/6 全过）
     *   已证明账号侧没问题、是纯协议的缺陷 —— 要定位到底差什么，只能看这个对象。
     *   重点看 multi_channel_allowed：我们在 rtProtocolClient.js:187 **恒定**发 channel:'sms'，
     *   若该账号会话是 multi_channel_allowed=false，这个字段本身就可能构成状态冲突。
     *   只打字段名和标量值，不打 token/邮箱明文。
     */
    _logAuthSession(where, data) {
        try {
            const s = (data && data['oai-client-auth-session']) || null;
            if (!s) { this.log(`[diag] ${where}: 响应里没有 oai-client-auth-session`); return; }
            // ★★ 2026-08-15 加进 promo / signup_source / signup_mode。
            //   起因：协议注册出来的号【7/7 拿不到那个 plus-1-month-free 促销】，
            //   而浏览器号 81% 有。找归因时发现服务端【本来就在这个会话对象里回一个
            //   名叫 promo 的字段】，我们却只打字段名、从不打值 —— 等于把唯一一条
            //   "不建号就能看到促销判定"的通道白白扔了两个月。
            //   三个都是标量、不含 PII，符合本函数既有约定。
            const pick = ['email_verified', 'email_verification_mode', 'multi_channel_allowed',
                'passwordless_disabled', 'passwordless_otp_from_password_redirect',
                'original_screen_hint', 'country_code_hint', 'app_name_enum', 'destination_app_name',
                'promo', 'signup_source', 'signup_mode'];
            const kv = pick.filter(k => s[k] !== undefined).map(k => `${k}=${JSON.stringify(s[k])}`).join(' ');
            const extra = Object.keys(s).filter(k => !pick.includes(k)
                && !/email|name|session_id|logging_id|token|phone/i.test(k));
            this.log(`[diag] ${where} 会话: ${kv}`);
            this.log(`[diag] ${where} 其余字段名: ${extra.join(',') || '(无)'}`
                + ` | page=${JSON.stringify((data.page || {}))} | continue_url=${data.continue_url || '(无)'}`);
        } catch (e) { this.log(`[diag] ${where} 会话打印失败: ${String(e && e.message).slice(0, 80)}`); }
    }

    /**
     * 覆写 _post：给 phone-otp/validate 补上 sentinel 头。
     * ★真流量抓包显示这个端点是**带** openai-sentinel-token 的（flow=verify_phone_otp），
     *   而父类 RtProtocolClient._post 不带（B2 架构下浏览器已经过了 sentinel，尾巴不用再带）。
     *   纯协议全程自己走，必须补齐，否则 OpenAI 侧看到的是"无 sentinel 的手机验证"。
     *   只对这一个端点特殊处理，其余仍走父类，尽量不动既有行为。
     */
    async _post(path, bodyObj, extraHeaders) {
        if (/\/api\/accounts\/phone-otp\/validate/.test(path)) {
            return this._postWithSentinel(path, bodyObj, 'verify_phone_otp',
                AUTH_BASE + '/phone-verification');
        }
        return super._post(path, bodyObj, extraHeaders);
    }

    /**
     * ④ 判断下一步：绑手机 / 直接 consent / 跟随 URL。
     *    ★B 架构在这里是**无条件**绑号的；C 架构按服务端响应决定：
     *      - 首次拿 RT（号未绑）→ 服务端会要手机，走 NEXT_PHONE
     *      - 重跑（号已绑）    → 直接 consent，不消耗接码
     */
    classifyNextStep(otpData = this.lastOtpResponse) {
        const data = otpData || {};
        const page = data.page || {};
        const pageType = String(page.type || '').toLowerCase();
        const continueUrl = (page.payload && page.payload.url) || data.continue_url || data.external_url || '';

        // ★page.type 是权威信号，必须优先且独占判断。
        //   曾经用整个响应 JSON 做正则兜底 —— 手机验证**成功后**的响应里仍残留
        //   "phone-verification" 字样（continue_url 等），导致刚绑完号又被判成 next=phone，
        //   在生产的循环里就是绑号死循环。所以：有 page.type 就只信它。
        if (pageType) {
            // ★两步验证：账号开了 MFA，服务端把我们导到 /mfa-challenge/<id> 要求输入 2FA 码。
            //   纯协议拿不到用户的 TOTP/备用码，这条路走不通 —— 必须**明确失败**并转人工/走浏览器。
            //   2026-08-03 实测（saucer-secular0o）：以前 mfa_challenge 不命中任何分支，
            //   靠 continueUrl 掉进 NEXT_FOLLOW → 跟随拿回一个 HTML 输入页（无 Location）→ 跟随失败
            //   → 回退到 selectWorkspaceAndGetCode() → 会话根本没到那步 ⇒ 报「会话里没有 workspace」，
            //   一个跟真因毫不相干的错误，而且判可重试，**连跑 8 次、白烧 8 封验证码邮件**。
            //   会话里的 mfa_factors / mfa_challenge_factors 字段是同一件事的旁证。
            if (/^mfa|mfa_challenge|two[-_]?factor|totp/.test(pageType)) {
                return { next: NEXT_MFA, pageType, continueUrl };
            }
            if (/consent|codex/.test(pageType)) return { next: NEXT_CONSENT, pageType, continueUrl };
            if (/phone/.test(pageType)) return { next: NEXT_PHONE, pageType, continueUrl };
            if (continueUrl) return { next: NEXT_FOLLOW, pageType, continueUrl };
            return { next: NEXT_CONSENT, pageType, continueUrl };
        }

        // 只有在完全没有 page.type 时才退回整体启发式
        const blob = JSON.stringify(data).toLowerCase();
        if (/add[-_]?phone|phone[-_]?otp|phone_verification/.test(blob)) {
            return { next: NEXT_PHONE, pageType, continueUrl };
        }
        if (continueUrl) return { next: NEXT_FOLLOW, pageType, continueUrl };
        return { next: NEXT_CONSENT, pageType, continueUrl };
    }

    /**
     * ⑤ 拿 authorization code。
     *    优先 workspace/select（父类实现）；给了 continue_url 就先跟随。
     */
    async obtainAuthorizationCode(classification) {
        const cls = classification || this.classifyNextStep();
        // ★指定了目标工作空间时，必须先走 workspace/select。
        //   continue_url 那条捷径拿回来的 code 绑的是【服务端默认空间】，
        //   代码无从干预 —— 走了它就等于目标参数白给，而且失败形态是静默的
        //   「绑错空间」，和绑对了在账号状态上完全分不出来。
        //   select 真的走不通时才回落到跟随，最终由换 token 后的 JWT 断言兜底。
        if (this.targetWorkspaceId) {
            try {
                return await this.selectWorkspaceAndGetCode();
            } catch (e) {
                if (e && e.code === 'RT_WORKSPACE_NOT_FOUND') throw e;   // 重试也没用
                if (!(cls.next === NEXT_FOLLOW && cls.continueUrl)) throw e;
                this.log(`[pure] workspace/select 失败(${e.message.slice(0, 80)})，回落跟随 continue_url`);
                return await this._followRedirectsForCode(cls.continueUrl);
            }
        }
        if (cls.next === NEXT_FOLLOW && cls.continueUrl) {
            this.log('[pure] 先跟随 continue_url 找 callback…');
            try {
                return await this._followRedirectsForCode(cls.continueUrl);
            } catch (e) {
                this.log(`[pure] 跟随失败(${e.message.slice(0, 80)})，改走 workspace/select`);
            }
        }
        return await this.selectWorkspaceAndGetCode();
    }
}

module.exports = {
    NEXT_CONSENT,
    NEXT_FOLLOW,
    NEXT_PHONE,
    NEXT_MFA,
    RtPureProtocolClient,
};
