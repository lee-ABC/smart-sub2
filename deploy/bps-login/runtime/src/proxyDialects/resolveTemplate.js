// 把「按供应商分组的凭据模板」摊平成 config.js 需要的扁平字段。
// 纯函数、无 require 副作用 —— config.js 在模块顶层调用它，不能有任何 I/O 或状态。
//
// 兜底顺序保证迁移过程中「config.json 不动 ⇒ 行为零变化」：
//   proxyVendors[vendor] 存在且可用 → 用它
//   否则 → 回退到旧的扁平 proxyHost/proxyPort/proxyUsername/proxyPassword/proxyType
'use strict';

function normalize(t) {
    return {
        host: String(t.host || ''),
        port: parseInt(t.port, 10) || 0,
        username: String(t.username || ''),
        password: String(t.password || ''),
        type: String(t.type || 'SOCKS5'),
    };
}

/**
 * @param {object} vendors  { cliproxy:{host,port,type,username,password}, kookeey:{...} }
 * @param {string} vendor   当前生效的供应商名
 * @param {object} raw      原始 config.json（提供旧扁平字段做兜底）
 */
function resolveFlatProxy(vendors, vendor, raw) {
    const v = (vendors && typeof vendors === 'object') ? vendors : {};
    const r = raw || {};
    const t = v[String(vendor || '').toLowerCase()];
    if (t && t.host && parseInt(t.port, 10) > 0) return normalize(t);
    return {
        host: r.proxyHost || '',
        port: parseInt(r.proxyPort, 10) || 0,
        username: r.proxyUsername || '',
        password: r.proxyPassword || '',
        type: r.proxyType || 'SOCKS5',
    };
}

// 规范化分链路覆盖表：只保留白名单内的供应商名，空串 = 跟随全局
// ★链路名单必须与 proxyVendorPatch.LANES 保持一致。
//   这里少写一个链路的后果很隐蔽：接口能存进 config.json，但 config 加载时被这里丢掉，
//   于是「保存成功 → 一刷新就没了」，而且没有任何报错。2026-08-02 加 'rt' 时踩到过。
const VENDOR_LANES = ['detect', 'register', 'sidecar', 'rt'];

function normalizeLaneMap(raw, allowedNames) {
    const out = {};
    const src = (raw && typeof raw === 'object' && !Array.isArray(raw)) ? raw : {};
    for (const lane of VENDOR_LANES) {
        const v = String(src[lane] || '').trim().toLowerCase();
        out[lane] = allowedNames.includes(v) ? v : '';
    }
    return out;
}

// 规范化分链路粘性周期表
function normalizeTtlMap(raw) {
    const src = (raw && typeof raw === 'object' && !Array.isArray(raw)) ? raw : {};
    const pick = (k, dflt) => {
        const v = String(src[k] == null ? '' : src[k]).trim().toLowerCase();
        return /^(0|\d+[smh])$/.test(v) ? v : dflt;
    };
    return {
        register: pick('register', '30m'),
        detect: pick('detect', '30m'),
        sidecar: pick('sidecar', '30m'),
        promo: pick('promo', '0'),          // 促销链路必须无粘性：每次全新出口 IP
        // ★2026-08-02 补 rt：拿 RT 一定要粘性。一次授权里 sentinel/req、authorize、
        //   email-otp、add-phone、换 token 是同一个 auth session，中途换出口 IP
        //   会被判会话失效、甚至让刚拿到的 RT 陆续失效（rtBrowserTask.js:242 记过这个坑）。
        //   之前这里没有 rt 项 ⇒ ttl=undefined ⇒ 无粘性保证。
        rt: pick('rt', '30m'),
    };
}

module.exports = { resolveFlatProxy, normalizeLaneMap, normalizeTtlMap, VENDOR_LANES };
