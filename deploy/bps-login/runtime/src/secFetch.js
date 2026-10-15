'use strict';
// Sec-Fetch 元数据头 —— Chrome 对每个请求无条件附加的一组。
//
// 【为什么必须补】（2026-08-12 抓包实证）
//   抓包里凡是能看到线级头的请求（带 :authority/:method/:path/:scheme 四个伪头的那一类，
//   共 2005 条、分布在 44/47 份文件里），这三个头是 **2005/2005 = 100%**，一条不漏。
//   切片同样干净：fetch@auth.openai.com 100/100、xhr@auth.openai.com 29/29、
//   fetch@chatgpt.com 106/106、script@sentinel.openai.com 7/7。
//   而我们的协议链路 12 跳一个都不发。
//
//   ★ 关键在于它【零良性解释】：sec-fetch-* 由浏览器网络栈生成，
//     扩展和内容拦截器都改不了、去不掉。缺 _fbp 那种第三方像素可以说"被拦截器挡了"，
//     缺 sec-fetch-site 没有任何说法。而且单请求静态规则即可判定，检测成本最低。
//
// 【取值不是抄来的，是按 W3C Fetch Metadata 规范算的】
//   sec-fetch-site 取决于「请求目标」与「发起方」的关系：
//     无发起方（地址栏直接打开）      → none
//     同源                            → same-origin
//     同站不同源（auth. 与 sentinel.）→ same-site
//     跨站（chatgpt.com ↔ openai.com）→ cross-site
//   sec-fetch-mode / dest 取决于请求种类：导航是 navigate/document，XHR 是 cors/empty。
//
// ⚠ 重定向链：规范要求跨整条链取【最严格】的那一档。我们是手动跟 302 的，
//   所以跟随时必须显式把上一跳算出来的 site 传下来（siteOverride），
//   不能拿"上一跳的 URL"当发起方重算 —— 那会把 cross-site 悄悄降级成 same-origin。
//
// ⚠ sec-fetch-user 刻意【不发】：全 47 份抓包 0 次出现，且 FULL 类里根本没有 document
//   样本，导航该不该带它无从观测。补一个语料里 0 次出现的头，是在造新特征。

// 我们这条链路会遇到的站点。用显式表而不是通用 eTLD+1 库 ——
// 只有这几个域，写死更准，也不引依赖。
const SITES = ['openai.com', 'chatgpt.com', 'oaistatic.com', 'oaiusercontent.com'];

function siteOf(host) {
    const h = String(host || '').toLowerCase();
    for (const s of SITES) {
        if (h === s || h.endsWith('.' + s)) return s;
    }
    // 未知域：退化成"整个 host 自成一站"，只会让判定更严格（更容易判成 cross-site），
    // 不会把 cross-site 误判成 same-site。
    return h;
}

/** 计算 sec-fetch-site。initiator 为空 = 地址栏直接打开。 */
function siteRelation(targetUrl, initiatorUrl) {
    if (!initiatorUrl) return 'none';
    let t, i;
    try { t = new URL(targetUrl); i = new URL(initiatorUrl); } catch (_) { return 'cross-site'; }
    if (t.origin === i.origin) return 'same-origin';
    if (siteOf(t.hostname) === siteOf(i.hostname)) return 'same-site';
    return 'cross-site';
}

// 同一枚举里越靠后越严格。重定向链取最严格的一档。
const STRICTNESS = ['none', 'same-origin', 'same-site', 'cross-site'];
function moreRestrictive(a, b) {
    return STRICTNESS.indexOf(a) >= STRICTNESS.indexOf(b) ? a : b;
}

/**
 * 产出一次请求的 sec-fetch-* 三件套。
 *
 * @param {object} o
 *   url          请求目标
 *   initiator    发起方页面 URL；顶层地址栏导航传 null
 *   navigation   true = 顶层文档导航；false/省略 = XHR / fetch
 *   siteOverride 跟随 302 时，把上一跳算出的 site 传进来（规范要求取最严格）
 * @returns {{'sec-fetch-site':string,'sec-fetch-mode':string,'sec-fetch-dest':string}}
 */
function secFetchHeaders(o = {}) {
    let site = siteRelation(o.url, o.initiator);
    if (o.siteOverride) site = moreRestrictive(site, o.siteOverride);
    return {
        'sec-fetch-site': site,
        'sec-fetch-mode': o.navigation ? 'navigate' : 'cors',
        'sec-fetch-dest': o.navigation ? 'document' : 'empty',
    };
}

/**
 * Chrome 的 priority 头（RFC 9218 Extensible Priorities）。
 *
 * 【取值来自抓包实测，按资源类型分组，213 个带值样本，组间完全不重叠】
 *   fetch / xhr  (dest=empty)  → "u=1, i"   28/28   ← 我们的 XHR 跳全是这一档
 *   script       (dest=script) → "u=1"      167
 *   stylesheet   (dest=style)  → "u=0"      6
 *   image        (dest=image)  → "i"        12
 *
 * ⚠ 别按"XHR 发 u=1、导航发 u=0"来写 —— u=1 是 <script> 的取值、u=0 是 <link rel=stylesheet>
 *   的取值，跟 XHR 和导航都无关。照那样写会造出一个「XHR 上发 script 优先级」的客户端。
 * ⚠ 导航（document）在语料里【一个带值样本都没有】，所以不发。补一个 0 样本的头是造特征。
 */
function priorityHeader(opts = {}) {
    if (opts.navigation) return {};      // 无样本，不发
    // ★ 2026-08-14：按【资源类型】给值。此前一律发 'u=1, i'，
    //   于是 sentinel 的 sdk.js（我们自己声明 sec-fetch-dest: script）
    //   却带着 fetch 的优先级 —— 同一个请求里自相矛盾。
    //   这是逐请求比对（tools/diff_protocol_vs_browser.js）抓出来的，
    //   读源码永远看不出来：两处代码单独看都对。
    const dest = String(opts.dest || 'empty');
    if (dest === 'script') return { priority: 'u=1' };        // 167/167
    if (dest === 'style') return { priority: 'u=0' };          // 6/6
    if (dest === 'image') return { priority: 'i' };            // 12/12
    return { priority: 'u=1, i' };                             // fetch/xhr 28/28
}

/**
 * Origin 头（B3-7，2026-09-04）。
 *
 * 【为什么补：Fetch 规范对非 GET/HEAD 是无条件的】
 *   规范 "append a request Origin header"：请求 mode 是 cors 时无条件加；
 *   否则只要 method 既不是 GET 也不是 HEAD，就加。
 *   ⇒ 一个真浏览器发出去的 POST / OPTIONS **不可能没有 Origin**。
 *   src/chromeHeaderOrder.js:55 的顺序表里本来就给 origin 留了位置 ——
 *   我们只在主干三发上手写了字面量，其余十几发漏了，是无意的遗漏而不是设计。
 *
 * 【冻结语料复算（口径写全，照抄即可复跑）】
 *   数据源：baselines/2026-09-04/browser/*.json.gz，13 份。
 *   线级判据：headerEvidence==='wire'；无该字段则回落到
 *            headerNames 同时含 accept-encoding + accept-language（B1-3 判据）。
 *   复跑：
 *     node -e "const z=require('zlib'),fs=require('fs'),p=require('path');
 *       const d='baselines/2026-09-04/browser';
 *       const wire=e=>{const v=String(e.headerEvidence||'');if(v)return v==='wire';
 *         const n=(e.headerNames||[]).map(x=>x.toLowerCase());
 *         return n.includes('accept-encoding')&&n.includes('accept-language');};
 *       let t=0,o=0;for(const f of fs.readdirSync(d))for(const e of
 *         JSON.parse(z.gunzipSync(fs.readFileSync(p.join(d,f))).toString()).events||[]){
 *         if(e.kind!=='request'||!wire(e))continue;
 *         const m=String(e.method||'GET').toUpperCase();if(m==='GET'||m==='HEAD')continue;
 *         t++;if((e.headerNames||[]).map(x=>x.toLowerCase()).includes('origin'))o++;}
 *       console.log(o+'/'+t);"
 *   结果（2026-09-04 实测）：
 *     · 线级非 GET/HEAD 请求带 origin：**实测 105/105**（其中顶层导航型 0 条）
 *       逐端点：POST auth.openai.com/awe/api/v2/rum                实测 71/71
 *               POST chatgpt.com/backend-api/settings/announcement_viewed 实测 14/14
 *               POST chatgpt.com/backend-anon/bazaar/obi/sync-token  实测 9/9
 *               OPTIONS chatgpt.com/ces/v1/rgstr                     实测 5/5
 *               POST 站外不透明目标                                  实测 5/5
 *               POST chatgpt.com/backend-api/conversation/init       实测 1/1
 *     · 线级 GET 里 resourceType=fetch 的：带 origin **实测 0/4** ⇒ GET 返回 {} 有据。
 *
 * ⚠ 【别拿非线级样本当"没发 origin"的证据】同一批语料里，非线级（CDP
 *   requestWillBeSent 视角）的非 GET/HEAD 请求共 2164 条：
 *     user-agent 实测 2164/2164、content-type 实测 1976/2164，
 *     而 sec-fetch-* 实测 0/2164、origin 实测 0/2164。
 *   sec-fetch-* 已被独立证明是 Chrome 对每个请求无条件附加的（线级 2005/2005），
 *   它在这个视角下 0/2164 ⇒ **这个视角根本看不见网络栈补的头**。
 *   所以该视角里的 origin=0 是"没有证据"，不是"证据表明没有"。
 *   （旁证：同视角的 OPTIONS 预检 实测 47/47 带 origin —— 预检整条由网络栈合成，
 *     所以它反而是全的。）
 *
 * ⚠ navigation:true 一律返回 {}：顶层导航型的非 GET/HEAD（表单 POST 导航）
 *   在冻结语料里 **实测 0/105**，取值无从观测；而我们这条链路也没有这种请求。
 *   补一个 0 样本的形态是造特征 —— 与本文件 sec-fetch-user 同一条纪律。
 *
 * ★★★ 配对红线（W2-S6 红线 3）：本函数缺 initiator 就返回 {}。
 *   如果调用侧有一部分跳带 initiator、一部分不带，就会出现
 *   "同一轮里一部分请求带 Origin、一部分不带" —— 按审计自己的逻辑
 *   （我们在抓包里自己标注了哪些请求是脚本补的），**那比统一不带更糟**。
 *   所以调用侧（probe 的 hop）必须把"非 GET/HEAD 且非导航却算不出 origin"
 *   当成硬错误抛出，并由 test_sec_fetch.js / test_protocol_runtime_contract.js
 *   两侧断言：行为侧遍历所有 POST/OPTIONS 调用点的 initiator 取值逐个执行本函数，
 *   源码侧扫 hop('...','POST'|'OPTIONS' 的调用点 opts 必须含 initiator。
 *
 * @param {object} o
 *   method     请求方法
 *   initiator  发起方页面 URL（与 secFetchHeaders 同一个入参，同一处展开）
 *   navigation true = 顶层文档导航
 * @returns {{origin?:string}}
 */
function originHeader(o = {}) {
    const method = String(o.method || 'GET').toUpperCase();
    if (method === 'GET' || method === 'HEAD') return {};
    if (o.navigation) return {};
    if (!o.initiator) return {};
    let origin;
    try { origin = new URL(String(o.initiator)).origin; } catch (_) { return {}; }
    // 不透明来源（data:/blob: 等）序列化成字符串 "null"。我们这条链路不会有，
    // 但真发出去一个 Origin: null 比不发更醒目 —— 宁可返回 {} 让调用侧的红线炸。
    if (!origin || origin === 'null') return {};
    return { origin };
}

/** 自检：三件套是否自洽。给测试与探针用 —— 内部矛盾比缺头更糟。 */
function verifySecFetch(h) {
    const problems = [];
    const site = h['sec-fetch-site'];
    const mode = h['sec-fetch-mode'];
    const dest = h['sec-fetch-dest'];
    if (!STRICTNESS.includes(site)) problems.push(`sec-fetch-site 取值非法: ${site}`);
    if (!['navigate', 'cors', 'no-cors', 'same-origin'].includes(mode)) {
        problems.push(`sec-fetch-mode 取值非法: ${mode}`);
    }
    if (mode === 'navigate' && dest !== 'document') {
        problems.push('navigate 模式的 dest 必须是 document');
    }
    if (mode === 'cors' && dest !== 'empty') {
        problems.push('cors 模式的 dest 必须是 empty');
    }
    // none 只可能出现在导航上：XHR 一定有发起方页面
    if (site === 'none' && mode !== 'navigate') {
        problems.push('sec-fetch-site: none 只可能出现在顶层导航上，XHR 必有发起方');
    }
    return { ok: problems.length === 0, problems };
}

module.exports = {
    secFetchHeaders, priorityHeader, originHeader, verifySecFetch,
    siteRelation, moreRestrictive, siteOf,
};
