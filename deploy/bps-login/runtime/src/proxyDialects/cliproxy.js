// cliproxy 方言：地区/会话/粘性全部编码在**用户名**里，密码固定不变。
//   mg4y1193081-region-JP-sid-PeTJpAE5-t-30
//   └─账号─────┘ └─地区─┘ └─会话───┘ └粘性(分钟)
// 可选粘性段 -st-XXXX 出现在 region 与 sid 之间（历史格式，Python sidecar 换非 JP 区时会剥掉）。
//
// ★ 纯函数、无状态：不 log、不抛、不计数。只如实报告"改动了没有"，策略由 proxyConfig 决定。
'use strict';
const crypto = require('crypto');

// ── 变更用正则：故意"松"，与迁移前逐字节一致（改动这里会破坏 characterization 快照）──
const SID_MUT = /(-sid-)[^-]+/;
const REGION_MUT = /(-region-)[A-Za-z]{2}/;

// ── 识别用正则：比变更正则"严"，必须同时有 region 段和 sid 段。
//    为什么不能复用 SID_MUT：'no-sid-at-all' 也能命中 /-sid-[^-]+/（-sid-at + -all），
//    拿它做供应商识别会把不相干的字符串误判成 cliproxy 凭据。
const LOOKS_LIKE = /-region-[A-Za-z]{2}(-|$)/;

// ── 校验用正则：只给 validate()/GUI 预检，不参与变更 ──
const STRICT = /^(?<acct>.+?)-region-(?<region>[A-Za-z]{2})(?<sticky>-st-[^-]+)?-sid-(?<session>[^-]+)-t-(?<ttl>\d+)$/;

const newSession = () => crypto.randomBytes(5).toString('hex');

// ★ withRegion 一律保留原会话，不推导新的。理由（2026-07-26 实测）：
//   · kookeey 实测没有「会话被钉到首次分配 IP 后改 region 不生效」的坑（同会话 jp→nl→jp 三次全对），
//     所以换区不需要靠换会话来"解钉"。
//   · 保留会话 = 与迁移前逐字节一致。
//   · 需要独立出口的地方（各链路促销）本来就会额外调一次 withFreshSession，不依赖这里。

const applied = (cred, changed, reason) => ({ cred, changed, reason });

module.exports = {
    name: 'cliproxy',
    label: 'cliproxy（地区/会话在用户名）',

    caps: {
        variableField: 'username',
        supportsRegion: true,
        supportsSession: true,
        supportsTtl: true,
        regionCase: 'upper',
        protocol: 'SOCKS5',
        defaults: { host: 'sg2.cliproxy.io', port: 443, type: 'SOCKS5' },
        sessionExample: 'mg4y1193081-region-JP-sid-PeTJpAE5-t-30',
    },

    matches(cred) {
        const u = String((cred && cred.username) || '');
        return LOOKS_LIKE.test(u) && u.includes('-sid-');
    },

    parseRegion(cred) {
        const m = /-region-([A-Za-z]{2})/.exec(String((cred && cred.username) || ''));
        return m ? m[1].toUpperCase() : null;
    },

    parseSession(cred) {
        const m = /-sid-([^-]+)/.exec(String((cred && cred.username) || ''));
        return m ? m[1] : null;
    },

    parseTtl(cred) {
        const m = /-t-(\d+)$/.exec(String((cred && cred.username) || ''));
        return m ? m[1] : null;
    },

    // 换会话 = 换出口 IP。opts.session 可指定（探测/测试用），否则随机。
    withFreshSession(cred, opts) {
        const o = opts || {};
        const u = String((cred && cred.username) || '');
        if (!SID_MUT.test(u)) return applied(cred, false, 'no_match');
        const session = o.session || newSession();
        const next = u.replace(SID_MUT, `$1${session}`);
        // ttl 可选覆盖（-t-N 段）
        const withTtl = o.ttl ? next.replace(/-t-\d+$/, `-t-${String(o.ttl).replace(/[^\d]/g, '') || '30'}`) : next;
        return applied({ ...cred, username: withTtl }, withTtl !== u, withTtl === u ? 'noop' : '');
    },

    // 打地区。**保留原会话**——不做"换区顺便换会话"的推导，理由见文件头注释。
    withRegion(cred, region) {
        const u = String((cred && cred.username) || '');
        const target = String(region || '').toUpperCase();
        if (!/^[A-Za-z]{2}$/.test(target)) return applied(cred, false, 'no_match');
        if (!REGION_MUT.test(u)) return applied(cred, false, 'no_match');
        // 不做"已是目标地区就早退"的优化：历史实现即使地区没变也会走一遍 -st- 剥离
        let next = u.replace(REGION_MUT, `$1${target}`);
        if (target !== 'JP') next = next.replace(/-st-[^-]+(?=-sid-)/, '');
        return applied({ ...cred, username: next }, next !== u, next === u ? 'noop' : '');
    },

    validate(cred) {
        const issues = [];
        const u = String((cred && cred.username) || '');
        if (!u) issues.push({ field: 'username', code: 'empty', msg: '用户名不能为空' });
        else if (!STRICT.test(u)) {
            issues.push({
                field: 'username',
                code: 'shape',
                msg: '用户名应形如 账号-region-JP-sid-XXXX-t-30',
            });
        }
        if (!String((cred && cred.password) || '')) {
            issues.push({ field: 'password', code: 'empty', msg: '密码不能为空' });
        }
        return { ok: issues.length === 0, issues };
    },
};
