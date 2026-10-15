// kookeey 方言：地区/会话/轮转周期编码在**密码**里，用户名固定不变。（与 cliproxy 正好相反）
//   用户名  1234567-abcdef0123              ← 用户ID-用户名，固定
//   密码    base00000-jp-14910981-5m       ← 密码-国家ISO码-会话-轮转周期
//                     └地区┘ └─会话──┘ └周期┘
// 实测（2026-07-26）：地区码大小写不敏感；换会话=换出口 IP；同会话=粘住同一 IP；
//                    JP/BR/NL/VN/IN/US 全通；周期接受 1m/5m/30m/0。
//
// ★ 纯函数、无状态：不 log、不抛、不计数。
'use strict';
const crypto = require('crypto');

// 从**右往左**绑定：base 允许含 '-'（贪婪 .+ 会回溯到最后一个合法切分点）。
// 例 'a-b-c0c0-jp-123-30m' → base='a-b-c0c0', region='jp', session='123', ttl='30m'
const PW = /^(?<base>.+)-(?<region>[A-Za-z]{2}|global)-(?<session>[A-Za-z0-9]+)-(?<ttl>\d+[smh]|0)$/i;

// 会话用 12 位十六进制（48 bit）。不用 8 位是因为跑批高并发下 32 bit 存在生日碰撞风险，
// 两个任务撞上同一 session 就会共用出口 IP —— 那正是本次迁移要根除的故障。
const newSession = () => crypto.randomBytes(6).toString('hex');

const DEFAULT_TTL = '30m';

const applied = (cred, changed, reason) => ({ cred, changed, reason });
const parse = cred => PW.exec(String((cred && cred.password) || ''));

// 周期归一化：'0'/'0m'/0 → '0'（无粘性）；'30'→'30m'；其余原样（已带单位）
function normalizeTtl(ttl) {
    const v = String(ttl == null ? '' : ttl).trim().toLowerCase();
    if (!v) return '';
    if (/^0m?$/.test(v)) return '0';
    if (/^\d+$/.test(v)) return `${v}m`;
    return /^\d+[smh]$/.test(v) ? v : '';
}

module.exports = {
    name: 'kookeey',
    label: 'kookeey（地区/会话在密码）',

    caps: {
        variableField: 'password',
        supportsRegion: true,
        supportsSession: true,
        supportsTtl: true,
        regionCase: 'lower',
        protocol: 'SOCKS5',
        defaults: { host: 'gate.kookeey.info', port: 1000, type: 'SOCKS5' },
        sessionExample: 'base00000-jp-14910981-30m',
        // 网关节点实测等价（交错采样 40 轮，中位差 4%），选哪个都行；欧洲/英国明显更慢。
        gateways: ['gate.kookeey.info', 'gate-jp.kookeey.info', 'gate-hk.kookeey.info',
                   'gate-in.kookeey.info', 'gate-th.kookeey.info', 'gate-au.kookeey.info'],
    },

    matches(cred) {
        return PW.test(String((cred && cred.password) || ''));
    },

    parseRegion(cred) {
        const m = parse(cred);
        return m ? m.groups.region.toUpperCase() : null;
    },

    parseSession(cred) {
        const m = parse(cred);
        return m ? m.groups.session : null;
    },

    parseTtl(cred) {
        const m = parse(cred);
        return m ? m.groups.ttl : null;
    },

    // 换会话 = 换出口 IP。opts.ttl='0' 表示无粘性（促销链路要求每次全新 IP）。
    withFreshSession(cred, opts) {
        const o = opts || {};
        const m = parse(cred);
        if (!m) return applied(cred, false, 'no_match');
        const g = m.groups;
        const ttl = normalizeTtl(o.ttl) || g.ttl;
        const session = o.session || newSession();
        const pw = `${g.base}-${g.region}-${session}-${ttl}`;
        return applied({ ...cred, password: pw }, pw !== cred.password, pw === cred.password ? 'noop' : '');
    },

    // 打地区。**保留原会话**（实测同会话换区正常生效，不需要"解钉"，见 cliproxy.js 文件头注释）。
    withRegion(cred, region, opts) {
        const o = opts || {};
        const m = parse(cred);
        const target = String(region || '').trim();
        if (!/^([A-Za-z]{2}|global)$/i.test(target)) return applied(cred, false, 'no_match');
        if (!m) return applied(cred, false, 'no_match');
        const g = m.groups;
        const ttl = normalizeTtl(o.ttl) || g.ttl;
        const pw = `${g.base}-${target.toLowerCase()}-${g.session}-${ttl}`;
        return applied({ ...cred, password: pw }, pw !== cred.password, pw === cred.password ? 'noop' : '');
    },

    validate(cred) {
        const issues = [];
        const u = String((cred && cred.username) || '');
        const p = String((cred && cred.password) || '');
        if (!u) issues.push({ field: 'username', code: 'empty', msg: '用户名不能为空' });
        else if (!/^[A-Za-z0-9]+-[A-Za-z0-9]+$/.test(u)) {
            issues.push({ field: 'username', code: 'shape', msg: '用户名应形如 用户ID-用户名（例 1234567-abcdef0123）' });
        }
        if (!p) issues.push({ field: 'password', code: 'empty', msg: '密码不能为空' });
        else if (!PW.test(p)) {
            issues.push({
                field: 'password',
                code: 'shape',
                msg: '密码应形如 密码-地区-会话-周期（例 base00000-jp-14910981-30m）。在 kookeey 面板「生成连接信息」里复制第 4 段',
            });
        }
        return { ok: issues.length === 0, issues };
    },

    normalizeTtl,
};
