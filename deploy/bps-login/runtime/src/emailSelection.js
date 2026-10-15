'use strict';

// 注册取号范围（「指定抽取」）的**唯一判据**。
//
// ★三处调用方必须共用本模块，不许各自手写 if：
//   闸1 预检补仓  emailPreflightCoordinator._tickAutoOnce   决定"谁能变就绪"
//   闸2 准入计数  registrationAdmission.isReadyEmail        决定"能不能开工"
//   闸3 预留取号  sqliteStateStore.reserveReadyOutlook /
//                stateManager.reserveReadyOutlookEmailForAttempt /
//                emailPool.reserveReady                     决定"抽哪一个"
//
// 口径一旦漂移的表现不是"抽错邮箱"，而是**假死**：闸2 数到范围外的就绪邮箱说"可以开工"，
// 闸3 按范围找不到 → 抛 capacity wait → attempt 落 CAPACITY_BLOCKED，而该状态又在
// NEW_ATTEMPT_BLOCKING_STATUSES 里，反过来阻塞新 attempt，注册池空转刷日志。
// （同类教训见 emailPool.js summarize() 上方那条"可用口径必须与取号完全一致"的注释。）

const PICK_MODES = Object.freeze({ ALL: 'all', TYPE: 'type', BATCH: 'batch' });
const EMAIL_TYPES = Object.freeze(['outlook', 'icloud']);
const MAX_BATCH_LENGTH = 64;

function normalizeType(value) {
    const type = String(value == null ? '' : value).trim().toLowerCase();
    return EMAIL_TYPES.includes(type) ? type : '';
}

// 批次名由人手填，这里只做"能安全比较"的最小归一：去首尾空白 + 限长。
// 大小写不敏感（比较时统一转小写），但保留用户填的原样用于显示。
function normalizeBatch(value) {
    return String(value == null ? '' : value).replace(/\s+/g, ' ').trim().slice(0, MAX_BATCH_LENGTH);
}

// 坏配置一律退化成 all（= 改造前的行为），不抛错：本判据在取号热路径上，
// 宁可"没框住"也不能让整条注册链因为一个手滑的配置值挂掉。
// 想让坏值被拒绝的，那是写入口的事（poolRuntime.validateRegistrationRuntimePatch 会 400）。
function normalizeEmailPick(raw) {
    const source = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {};
    const type = normalizeType(source.type);
    const batch = normalizeBatch(source.batch);
    const mode = String(source.mode == null ? '' : source.mode).trim().toLowerCase();
    if (mode === PICK_MODES.TYPE && type) return { mode: PICK_MODES.TYPE, type, batch };
    if (mode === PICK_MODES.BATCH && batch) return { mode: PICK_MODES.BATCH, type, batch };
    return { mode: PICK_MODES.ALL, type, batch };
}

function accountType(account) {
    return normalizeType(account && account.type) || 'outlook';
}

function accountBatch(account) {
    return normalizeBatch(account && account.batch);
}

// 热路径用：先把配置归一一次，返回可复用的谓词。
// （闸1 每 5 秒扫全池 7000+ 条，闸3 每次取号扫一遍就绪集，别在循环里反复归一配置。）
function emailPickPredicate(pick) {
    const wanted = normalizeEmailPick(pick);
    if (wanted.mode === PICK_MODES.ALL) return () => true;
    if (wanted.mode === PICK_MODES.TYPE) {
        return account => !!account && typeof account === 'object' && accountType(account) === wanted.type;
    }
    const batchKey = wanted.batch.toLowerCase();
    return account => !!account && typeof account === 'object' && accountBatch(account).toLowerCase() === batchKey;
}

function matchesEmailPick(account, pick) {
    return emailPickPredicate(pick)(account);
}

function isEmailPickRestricted(pick) {
    return normalizeEmailPick(pick).mode !== PICK_MODES.ALL;
}

// 给日志/前端文案用的人话
function describeEmailPick(pick) {
    const wanted = normalizeEmailPick(pick);
    if (wanted.mode === PICK_MODES.TYPE) return wanted.type === 'icloud' ? '仅 iCloud' : '仅 Outlook';
    if (wanted.mode === PICK_MODES.BATCH) return `批次「${wanted.batch}」`;
    return '全部邮箱';
}

module.exports = {
    PICK_MODES,
    EMAIL_TYPES,
    MAX_BATCH_LENGTH,
    normalizeBatch,
    normalizeEmailPick,
    emailPickPredicate,
    matchesEmailPick,
    isEmailPickRestricted,
    describeEmailPick,
};
