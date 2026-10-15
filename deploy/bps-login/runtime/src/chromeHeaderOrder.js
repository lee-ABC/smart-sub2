'use strict';
// Chrome 的请求头线序 —— 给 node-tls-client 的 headerOrder 用。
//
// 【为什么需要】（2026-08-12 本地实测）
//   起了一个本地 h2 服务器读 rawHeaders 的真实线序，连打 8 个请求，结果是：
//     accept  accept-encoding  accept-language  content-type  referer
//     sec-ch-ua  sec-ch-ua-mobile  sec-ch-ua-platform  user-agent
//   —— 稳定，但**按字母序排的**。
//   浏览器从不按字母序发头。字母序是 Go / Python 那类 HTTP 库的典型特征
//   （它们把头存进无序 map 再排序输出）。TLS 指纹装得再像，这一层照样露馅。
//
// 【数据来源的诚实说明】
//   ⚠ 这个顺序【不是】从我们自己的抓包里量出来的 —— logs/registration-flow 的
//     headerNames 是排过序后才落盘的，线序已经丢了。
//   这里用的是 Chrome 公开且长期稳定的排列（与 curl-impersonate / tls-client 社区
//   逆出来的一致）：sec-ch-ua 三件套打头，accept-encoding / accept-language / cookie 收尾。
//   与字母序相比，最大的差异正是这两端 —— 字母序把 accept-encoding/accept-language
//   顶到了第二三位，把 sec-ch-ua 塞到中间，两处都和真 Chrome 相反。
//   要拿到逐字精确的顺序，需要抓一份【保留线序】的报文（当前抓包器做不到）。
//
// 【不确定的部分】
//   自定义头（openai-sentinel-token / x-datadog-* / traceparent 等）在真 Chrome 里的
//   确切槽位没有权威依据。这里放在 sec-ch-ua 块之后、user-agent 之前 ——
//   这是 Chrome 对 XHR 自定义头的常见位置。列表里没提到的头一律排在末尾、
//   cookie 之前，保持相对顺序稳定（不排序，避免又变回字母序）。

// 导航请求（顶层文档）的顺序
const NAVIGATION_ORDER = [
    'sec-ch-ua',
    'sec-ch-ua-mobile',
    'sec-ch-ua-platform',
    'upgrade-insecure-requests',
    'user-agent',
    'accept',
    'sec-fetch-site',
    'sec-fetch-mode',
    'sec-fetch-user',
    'sec-fetch-dest',
    'referer',
    'accept-encoding',
    'accept-language',
    'priority',
    'cookie',
];

// XHR / fetch 的顺序。自定义头插在 sec-ch-ua 块与 user-agent 之间。
const FETCH_ORDER = [
    'sec-ch-ua',
    'sec-ch-ua-mobile',
    'sec-ch-ua-platform',
    'content-type',
    '<custom>',            // 占位：自定义头在这里展开
    'user-agent',
    'accept',
    'origin',
    'sec-fetch-site',
    'sec-fetch-mode',
    'sec-fetch-dest',
    'referer',
    'accept-encoding',
    'accept-language',
    'priority',
    'cookie',
];

/**
 * 给定这次请求实际要发的头名，算出 headerOrder。
 *
 * @param {string[]} names 本次请求的头名（大小写不敏感）
 * @param {{navigation?: boolean}} opts
 * @returns {string[]} 只包含本次真的会发的头，按 Chrome 的顺序排列
 */
function headerOrderFor(names, opts = {}) {
    const present = new Set(names.map(n => String(n).toLowerCase()));
    const template = opts.navigation ? NAVIGATION_ORDER : FETCH_ORDER;
    const known = new Set(template.filter(k => k !== '<custom>'));
    // 模板没覆盖到的都算自定义头，保持传入的相对顺序（不排序 —— 一排就又成字母序了）
    const custom = names.map(n => String(n).toLowerCase()).filter(n => !known.has(n));

    const out = [];
    for (const key of template) {
        if (key === '<custom>') { out.push(...custom); continue; }
        if (present.has(key)) out.push(key);
    }
    // 导航模板没有 <custom> 槽，自定义头收尾（cookie 之前）
    if (opts.navigation && custom.length) {
        const at = out.indexOf('cookie');
        if (at >= 0) out.splice(at, 0, ...custom);
        else out.push(...custom);
    }
    // 去重，保序
    return [...new Set(out)];
}

module.exports = { headerOrderFor, NAVIGATION_ORDER, FETCH_ORDER };
