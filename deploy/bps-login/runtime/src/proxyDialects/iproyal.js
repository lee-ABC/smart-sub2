// iproyal 方言：地区/会话/粘性编码在**密码**里（与 kookeey 同侧、与 cliproxy 相反），
// 但表达方式是**标签式**而非位置式：
//   用户名  xLyZ99qGHMR5ochW                                          ← 固定不变
//   密码    wGCSTp8W21bb8ldR_country-kr_session-5K0PS8Z1_lifetime-30m
//           └──base──────┘ └─地区──┘ └──会话───────┘ └─粘性──┘
//
// 与 kookeey 的关键差异（决定了解析必须按键切词、不能按位置）：
//   · 标签可缺省、可乱序，还能有我们不认识的（_skipipslist- / _city- / _state- …）
//   · kookeey 是 base-区-会话-周期 四段定长，正则从右往左绑；这里数量不定
//
// ★ 不写 _session- = 官方 randomize，每个请求换一个出口 IP。
//   这是相对 kookeey 的净优势：PROXY_MIGRATION.md 实测过 kookeey 的 ttl=0 **并不等于**无粘性
//   （同会话同地区照样回同一个 IP），促销链路只能靠换会话歪打正着；iproyal 这里是语义精确的。
//
// ★ 纯函数、无状态：不 log、不抛、不计数。只如实报告"改动了没有"，策略由 proxyConfig 决定。
'use strict';
const crypto = require('crypto');

// 我们认识的标签键。作用有两个：① 判断密码从哪里开始是标签区 ② 决定哪些键我们会主动改写。
// 不在此列的标签（未来 iproyal 新增的）仍会被完整保留 —— 见 parse()/render()。
const KNOWN = ['country', 'city', 'state', 'session', 'lifetime', 'skipipslist', 'streaming', 'killswitch'];
const FIRST_KNOWN = new RegExp(`_(?:${KNOWN.join('|')})-`, 'i');
const TOKEN = /^_([A-Za-z][A-Za-z0-9]*)-([^_]*)/;

// 会话必须**恰好 8 位**字母数字（iproyal 硬性要求）。
// 8 位十六进制只有 32 bit —— kookeey 那份注释记过：跑批高并发下 32 bit 存在生日碰撞风险，
// 两个任务撞上同一会话就会共用出口 IP。这里在同样的 8 位长度里改用 base36，约 41 bit。
const ALPHABET = '0123456789abcdefghijklmnopqrstuvwxyz';
function newSession() {
    let out = '';
    while (out.length < 8) {
        for (const b of crypto.randomBytes(8)) {
            if (b >= 252) continue;          // 拒绝采样：256 不是 36 的整数倍，直接取模会有偏置
            out += ALPHABET[b % 36];
            if (out.length === 8) break;
        }
    }
    return out;
}

const DEFAULT_TTL = '30m';
const TTL_SEC = { s: 1, m: 60, h: 3600, d: 86400 };
const MAX_TTL_SEC = 7 * 86400;               // 官方上限 7 天，且「只能写一个单位」

// 夹到官方允许区间。超上限落到 7d，低于 1 秒等价于"不要粘性"。
function clampTtl(v) {
    const m = /^(\d+)([smhd])$/.exec(v);
    if (!m) return '';
    const sec = Number(m[1]) * TTL_SEC[m[2]];
    if (sec < 1) return '0';
    return sec > MAX_TTL_SEC ? '7d' : v;
}

// 周期归一化：'0'/'0m'/0 → '0'（= randomize，不写 session/lifetime）；'30' → '30m'；其余带单位的原样夹紧。
// ★ 与 kookeey.normalizeTtl 的差别：这里多认一个 'd' 单位（iproyal 支持到 7 天）。
function normalizeTtl(ttl) {
    const v = String(ttl == null ? '' : ttl).trim().toLowerCase();
    if (!v) return '';
    if (/^0[smhd]?$/.test(v)) return '0';
    if (/^\d+$/.test(v)) return clampTtl(`${v}m`);
    return /^\d+[smhd]$/.test(v) ? clampTtl(v) : '';
}

/**
 * 拆密码 → { base, tags:[{key,value}…] }，认不出返回 null。
 *
 * ★ 从**第一个已知键**处切开，之后一律按 `_键-值` 逐个吃掉。这样：
 *   · base 里含 '_' 或 '-' 都安全（只要它不长得像 `_country-` 这类已知标签）
 *   · 未知标签（_skipipslist- 等）照样被收进 tags，render 时原样吐回去。
 *     丢一个标签 = 路由行为静默改变，正是本项目最忌讳的那类故障。
 *   · 尾部只要有一处吃不掉，宁可整体认不出（返回 null → 走 no_match 告警），
 *     也绝不半懂不懂地改写 —— 改坏的串会导致认证失败，比不改更贵。
 */
function parse(cred) {
    const pw = String((cred && cred.password) || '');
    const m = FIRST_KNOWN.exec(pw);
    if (!m) return null;
    const base = pw.slice(0, m.index);
    const tags = [];
    let rest = pw.slice(m.index);
    while (rest) {
        const t = TOKEN.exec(rest);
        if (!t) return null;
        tags.push({ key: t[1].toLowerCase(), value: t[2] });
        rest = rest.slice(t[0].length);
    }
    return { base, tags };
}

const render = (base, tags) => base + tags.map(t => `_${t.key}-${t.value}`).join('');

const get = (tags, k) => { const t = tags.find(x => x.key === k); return t ? t.value : null; };
// 已存在就**就地替换**（保持标签顺序 → 同一份凭据反复变换的结果逐字节稳定），否则追加
const put = (tags, k, v) => { const i = tags.findIndex(x => x.key === k); if (i < 0) tags.push({ key: k, value: v }); else tags[i] = { key: k, value: v }; };
const drop = (tags, k) => { const i = tags.findIndex(x => x.key === k); if (i >= 0) tags.splice(i, 1); };

const applied = (cred, changed, reason) => ({ cred, changed, reason });

module.exports = {
    name: 'iproyal',
    label: 'iproyal（地区/会话在密码，标签式）',

    caps: {
        variableField: 'password',
        supportsRegion: true,
        supportsSession: true,
        supportsTtl: true,
        regionCase: 'lower',
        protocol: 'SOCKS5',
        // 官方标称 SOCKS5=32325 / HTTP=12321；2026-07-31 实测 32325 / 12321 / 11205 三个端口
        // 都在做 SOCKS5 方法协商（网关按首字节自动识别协议），取文档标称的那个最稳妥。
        defaults: { host: 'geo.iproyal.com', port: 32325, type: 'SOCKS5' },
        sessionExample: 'wGCSTp8W21bb8ldR_country-kr_session-5K0PS8Z1_lifetime-30m',
        // ★ 会话可以合法缺席（= randomize，每请求换 IP）。另外两家没有这个状态，
        //   describeSession 靠这个标志把「有意不粘」与「轮换失效」区分开，否则日志里都是 '-'。
        sessionOptional: true,
        gateways: ['geo.iproyal.com', 'proxy.iproyal.com', 'us.proxy.iproyal.com', 'sg.proxy.iproyal.com'],
    },

    matches(cred) {
        // 主机名兜底：randomize 用法下密码可以一个标签都没有（就是纯 base），
        // 那样光看凭据认不出家门。认错家的代价是静默不轮换（全员共用一个出口 IP），
        // 所以宁可按主机名先认领 —— 真解析不了时后续变换会诚实报 no_match 并告警。
        // 注意 buildProxyVendorPatch 传进来的 cred 只有 {username,password}（没有 host），
        // 所以「保存凭据」那条路依然强制要求密码里带标签，不会被这条兜底放水。
        if (/(^|\.)iproyal\.com$/i.test(String((cred && cred.host) || ''))) return true;
        const p = parse(cred);
        if (!p) return false;
        // 只有 _skipipslist- 这类辅助标签不足以判定家门，必须见到地区或会话
        return get(p.tags, 'country') != null || get(p.tags, 'session') != null;
    },

    parseRegion(cred) {
        const p = parse(cred);
        const v = p && get(p.tags, 'country');
        // 多国写法 `_country-dk,it,ie` 是合法的；取第一个用于显示，不做多国推断
        return v ? String(v).split(',')[0].toUpperCase() : null;
    },

    // ★ 无 _session- 是合法状态（randomize），如实返回 null。
    //   「有意不粘」与「轮换失效」的区分放在 proxyConfig.describeSession（靠 caps.sessionOptional），
    //   本层保持纯粹：只报告字段在不在。
    parseSession(cred) {
        const p = parse(cred);
        return p ? get(p.tags, 'session') : null;
    },

    parseTtl(cred) {
        const p = parse(cred);
        if (!p) return null;
        // 没有 lifetime 又没有 session = randomize，粘性周期就是 0
        return get(p.tags, 'lifetime') || (get(p.tags, 'session') ? null : '0');
    },

    // 换会话 = 换出口 IP。opts.ttl='0' 表示无粘性 —— iproyal 下是**删掉 session/lifetime**
    // 落到官方 randomize，而不是像 kookeey 那样写一个 ttl=0（那个实测并不真的解粘）。
    withFreshSession(cred, opts) {
        const o = opts || {};
        const p = parse(cred);
        if (!p) return applied(cred, false, 'no_match');
        const tags = p.tags.slice();
        const ttl = normalizeTtl(o.ttl) || get(tags, 'lifetime') || DEFAULT_TTL;
        if (ttl === '0') {
            drop(tags, 'session');
            drop(tags, 'lifetime');
        } else {
            put(tags, 'session', o.session || newSession());
            put(tags, 'lifetime', ttl);
        }
        const pw = render(p.base, tags);
        return applied({ ...cred, password: pw }, pw !== cred.password, pw === cred.password ? 'noop' : '');
    },

    // 打地区。**保留原会话**（与另外两家一致：iDEAL 的 checkout/provider/approve 三段
    // 各调一次 withRegion，靠保留会话拿到同一个出口 IP → 身份一致，见 proxyConfig 头注释）。
    withRegion(cred, region, opts) {
        const o = opts || {};
        const p = parse(cred);
        const target = String(region || '').trim().toLowerCase();
        if (!/^[a-z]{2}$/.test(target)) return applied(cred, false, 'no_match');
        if (!p) return applied(cred, false, 'no_match');
        const tags = p.tags.slice();
        const prev = String(get(tags, 'country') || '').toLowerCase();
        put(tags, 'country', target);
        // ★ 换国家必须连 _city-/_state- 一起清掉：城市属于旧国家、州只对 country-us 合法。
        //   留着就是非法组合，路由器可能整条忽略 → 地区静默失效。这是 iproyal 独有的坑，
        //   另外两家没有城市/州的概念，所以这段没有对应物可参照。
        if (prev !== target) { drop(tags, 'city'); drop(tags, 'state'); }
        const ttl = normalizeTtl(o.ttl);
        if (ttl && ttl !== '0' && get(tags, 'session') != null) put(tags, 'lifetime', ttl);
        const pw = render(p.base, tags);
        return applied({ ...cred, password: pw }, pw !== cred.password, pw === cred.password ? 'noop' : '');
    },

    validate(cred) {
        const issues = [];
        const u = String((cred && cred.username) || '');
        const pw = String((cred && cred.password) || '');
        if (!u) issues.push({ field: 'username', code: 'empty', msg: '用户名不能为空' });
        else if (!/^[A-Za-z0-9]+$/.test(u)) {
            issues.push({ field: 'username', code: 'shape', msg: '用户名应是面板给的一串字母数字（例 xLyZ99qGHMR5ochW）' });
        }
        if (!pw) {
            issues.push({ field: 'password', code: 'empty', msg: '密码不能为空' });
            return { ok: false, issues };
        }
        const p = parse({ password: pw });
        if (!p) {
            issues.push({
                field: 'password',
                code: 'shape',
                msg: '密码应形如 密码_country-kr_session-XXXXXXXX_lifetime-30m。'
                    + '在 iproyal 面板「代理密码」里整段复制（至少要带 _country- 段）',
            });
            return { ok: false, issues };
        }
        const push = (code, msg) => issues.push({ field: 'password', code, msg });
        if (!p.base) push('shape', '密码缺少基础密码段（第一个 _ 之前那一截）');

        const session = get(p.tags, 'session');
        if (session != null && !/^[A-Za-z0-9]{8}$/.test(session)) {
            push('session', '_session- 必须恰好 8 位字母数字 —— 这是 iproyal 的硬性要求，长度不对会话不生效');
        }
        const lifetime = get(p.tags, 'lifetime');
        if (lifetime != null) {
            if (!/^\d+[smhd]$/.test(lifetime)) push('lifetime', '_lifetime- 只能是一个数字加一个单位（s/m/h/d），例 30m');
            else if (clampTtl(lifetime) !== lifetime) push('lifetime', '_lifetime- 最长 7d');
            if (session == null) push('lifetime', '_lifetime- 必须配合 _session- 使用，单独写不生效');
        }
        const country = get(p.tags, 'country');
        const city = get(p.tags, 'city');
        const state = get(p.tags, 'state');
        if ((city != null || state != null) && country == null) push('shape', '_city- / _state- 必须同时带 _country-');
        if (state != null && String(country || '').toLowerCase() !== 'us') push('shape', '_state- 只对 _country-us 有效');
        if (country != null && !/^[A-Za-z]{2}(,[A-Za-z]{2})*$/.test(country)) {
            push('shape', '_country- 应是两位 ISO 国家码（多国用逗号分隔，例 dk,it,ie）');
        }
        return { ok: issues.length === 0, issues };
    },

    normalizeTtl,
};
