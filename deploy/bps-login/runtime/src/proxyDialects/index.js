// 代理方言注册表：一家供应商 = 一个方言模块。
//
// 为什么需要方言层：各家供应商把「地区/会话/粘性」编码在**不同字段、不同语法**里
//   cliproxy → 用户名   mg4y1193081-region-JP-sid-PeTJpAE5-t-30
//   kookeey  → 密码     base00000-jp-14910981-30m                 （位置式，四段定长）
//   iproyal  → 密码     wGCST…_country-kr_session-5K0PS8Z1_lifetime-30m  （标签式，可缺省乱序）
// 所有变换必须收发 {username, password} 整对，只改一个字段的代码在换家后会静默失效
// （典型症状：所有账号共用同一出口 IP，而日志一切正常）。
//
// ★ 本层完全无状态：每次调用按传入 cfg 现算。这是配置热重载不用重启进程的前提。
'use strict';

const cliproxy = require('./cliproxy');
const kookeey = require('./kookeey');
const iproyal = require('./iproyal');

// 三家并存、随时切换是设计目标，不是过渡态：凭据各存各的（config.proxyVendors），
// 切换只改 proxyVendor / proxyVendorByLane，来回切不用重填。
const DIALECTS = { cliproxy, kookeey, iproyal };
const NAMES = Object.keys(DIALECTS);
const DEFAULT_VENDOR = 'cliproxy';

function byName(name) {
    const key = String(name || '').trim().toLowerCase();
    return DIALECTS[key] || null;
}

// 从凭据反推供应商。用于：① 未显式声明时兜底 ② 检测 registerProxies 等覆盖项是否还是旧家凭据
function sniff(cred) {
    if (!cred) return null;
    for (const name of NAMES) {
        if (DIALECTS[name].matches(cred)) return DIALECTS[name];
    }
    return null;
}

/**
 * 选方言：链路覆盖 → 全局声明 → 从凭据嗅探 → 默认 cliproxy
 * @param {object} cfg  配置对象（须含 proxyVendor / proxyVendorByLane / proxy* 凭据）
 * @param {string} [lane]  'register' | 'detect' | 'sidecar'，用于分链路灰度
 */
function getDialect(cfg, lane) {
    const c = cfg || {};
    if (lane) {
        const laneVendor = byName((c.proxyVendorByLane || {})[lane]);
        if (laneVendor) return laneVendor;
    }
    const global = byName(c.proxyVendor);
    if (global) return global;
    const sniffed = sniff({ username: c.proxyUsername, password: c.proxyPassword });
    return sniffed || DIALECTS[DEFAULT_VENDOR];
}

// 该配置下某链路实际生效的供应商名（GUI 展示用）
function effectiveVendor(cfg, lane) {
    return getDialect(cfg, lane).name;
}

module.exports = {
    DIALECTS,
    NAMES,
    DEFAULT_VENDOR,
    byName,
    sniff,
    getDialect,
    effectiveVendor,
};
