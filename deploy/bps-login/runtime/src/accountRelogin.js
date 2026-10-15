'use strict';
/*
 * 用「已设好的密码 + 已开好的 2FA」重新登录，走到一个 authorization code。
 *
 * ════════ 为什么需要它 ════════
 * 2026-09-16 实测确认：给号加密码会【吊销旧 AT】（旧 token 立刻 HTTP 401 token_revoked）。
 * 所以「注册后设密码」这条链走完，号会暂时没有可用 AT —— 必须用密码+2FA 重新登录补一份。
 * 它同时是个独立能力：以后任何号 AT 过期/被吊销，只要库里存着密码+2FA，就能自助登回来。
 *
 * ════════ 为什么是 page.type 状态机，而不是写死的步骤序列 ════════
 * 「密码+2FA 登录」这条流程我们【没有抓包】。已知的不确定性：
 *   · 服务端会动态决定给【密码页】还是【passwordless 验证码页】
 *     （Statsig 开关，见 rtPureProtocolClient.submitEmail 的长注释）；
 *   · 密码/邮箱码之后是不是一定走 2FA、2FA 之后落 consent 还是 add_phone 还是直接发 code —— 都不确定。
 * 硬编一个猜出来的顺序，只要服务端和我们想的不一样就整条挂掉、还难查。所以这里按服务端每一步回的
 * page.type 现场派发，并把每一跳都打进日志：即便某一步需要调整，第一次真跑也能把真实序列暴露出来。
 * 本模块的红线只锁【与顺序无关】的安全不变量；真实序列由实跑验证。
 *
 * ════════ 纪律 ════════
 *   · 密码 / TOTP secret 绝不进日志（连长度都不打）。
 *   · 服务端要的 factor 与库里存的不一致就【停】—— 我们的 secret 算出来的码不可能对，
 *     硬提交只会白白消耗 MFA 尝试次数（多了会锁号）。
 *   · 落到 add_phone 这类需要绑手机/我们暂时处理不了的步骤，明确报出来，不硬闯、不假装成功。
 *   · 没真正拿到 code，绝不返回成功。
 *
 * ════════ 产出 ════════
 *   { ok:true, code, pageTypes } —— code 是全新的 authorization code；pageTypes 是这次真实走过的
 *   page.type 序列（供人核对/以后写死优化）。换 token（code → AT/refresh）留给调用方：
 *   那是 oauth 特定的 IO，且与本模块「走到 code」这件事失败处置不同。
 *
 * 【可测性】client / mail / totp 全注入 —— 安全不变量可离线测，一个真实请求都不发。
 */
const path = require('path');
const defaultTotpMod = require(path.join(__dirname, 'totp'));

function codedError(code, message, cause) {
    const e = new Error(message);
    e.code = code;
    if (cause && cause.code) e.causeCode = cause.code;
    if (cause && cause.httpStatus) e.httpStatus = cause.httpStatus;
    return e;
}

const pageTypeOf = data => String((data && data.page && data.page.type) || '');

// page.type 分类（大小写不敏感，调用点已 toLowerCase）。顺序在状态机里很重要 —— 见下。
const IS_MFA = pt => /(^|_)mfa|mfa_challenge|totp|two[-_]?factor/.test(pt);
const IS_RESET = pt => /reset_password|new_password/.test(pt);          // 登录流程里不该出现
const IS_PASSWORD = pt => /password/.test(pt);                          // login_password（RESET 已先行拦掉）
const IS_ADD_PHONE = pt => /add[-_]?phone|phone/.test(pt);             // 需要绑/验手机 —— 本模块暂不处理，明确报出
// 能交给 client.obtainAuthorizationCode 直接取 code 的落点（consent/工作空间/外链）。
// ★ 不含 add_phone/phone：那两个 obtainAuthorizationCode 处理不了（会掉进「没有 workspace」的假错误）。
const IS_CODE_READY = pt => /consent|codex|workspace|oauth|external_url/.test(pt);
const IS_EMAIL_OTP = pt => /email_otp|otp_verification|verification/.test(pt);

/**
 * @param {object} o
 *   client        RtPureProtocolClient（或同形状）。需要：
 *                 bootstrapAuthorize(authUrl) / submitEmail(email,opts) / submitPassword(pwd,{raw:true})
 *                 issueMfaChallenge(id) / verifyMfaTotp(id,code) / submitEmailOtp(code)
 *                 classifyNextStep(data) / obtainAuthorizationCode(cls)
 *   mail          收码 provider（可选）。只有服务端走 passwordless 分支时才用到：captureBaseline()/waitForOpenAICode(...)
 *   totp          可选，注入 totp 模块（测试用），默认 src/totp
 *   authUrl       oauth.getAuthUrl() 的结果
 *   email         账号邮箱
 *   password      账号密码（绝不进日志）
 *   totpSecret    该号 TOTP secret（绝不进日志）—— 必填：2FA 号登录必然要答码
 *   totpFactorId  库里存的 factor id；传了就与服务端给的核对，不一致即停
 *   log           日志函数
 *   otpMaxAttempts / otpIntervalMs  等邮箱码的轮询参数（仅 passwordless 分支用），默认 40 × 5s
 *   maxHops       状态机最多推进几跳（防服务端把我们绕进死循环），默认 8
 */
function createRelogin(o = {}) {
    const client = o.client;
    const mail = o.mail || null;
    const totpMod = o.totp || defaultTotpMod;
    const log = typeof o.log === 'function' ? o.log : () => {};
    const email = String(o.email || '');
    const password = String(o.password || '');
    const authUrl = String(o.authUrl || '');
    const totpSecret = String(o.totpSecret || '');
    const totpFactorId = String(o.totpFactorId || '');
    const otpMaxAttempts = Number(o.otpMaxAttempts) || 40;
    const otpIntervalMs = Number(o.otpIntervalMs) || 5000;
    const maxHops = Number(o.maxHops) || 8;
    // 追加到 authorize URL 上的 query（透传给 bootstrapAuthorize 的 authParams）。
    // 目前用途：试探 ccaps=login_methods chatgpt_login_finalizer_v1 能不能让服务端认证完直接发 code、
    //   跳过 add_phone（HAR 里 ChatGPT-web 客户端就是靠它跳过的）。不传＝纯登录，行为不变。
    const authParams = (o.authParams && typeof o.authParams === 'object') ? o.authParams : null;

    if (!client || typeof client.obtainAuthorizationCode !== 'function') {
        throw new Error('createRelogin 需要注入 client（RtPureProtocolClient）');
    }

    /** 答一次 2FA 挑战：核对 factor → 取挑战 → 算码 → 提交。返回服务端的新响应 data。 */
    async function answerMfa(data) {
        const payload = (data && data.page && data.page.payload) || {};
        const factors = Array.isArray(payload.factors) ? payload.factors : [];
        const totpFactor = factors.find(f => String((f && f.factor_type) || '').toLowerCase() === 'totp');
        const serverFactorId = String(payload.factor_id || (totpFactor && totpFactor.id) || '');
        if (!serverFactorId) throw codedError('RELOGIN_NO_FACTOR_ID', '2FA 挑战里没有 factor_id —— 无从回答');
        // ★不一致就停：我们的 secret 对应库里那个 factor，拿它答别的 factor 必然错，
        //   还会消耗 MFA 尝试次数（多了会锁）。
        if (totpFactorId && totpFactorId !== serverFactorId) {
            throw codedError('RELOGIN_FACTOR_MISMATCH',
                '服务端要的 factor 与库里存的不是同一个 —— 我们的码不可能对，硬提交只会消耗尝试次数并可能锁号');
        }
        try {
            await client.issueMfaChallenge(serverFactorId);
        } catch (e) {
            throw codedError('RELOGIN_MFA_ISSUE_FAILED', `取 2FA 挑战失败：${e && e.message}`, e);
        }
        let code = '';
        try {
            code = totpMod.totp(totpSecret);
        } catch (e) {
            throw codedError('RELOGIN_TOTP_CALC_FAILED', `用 secret 算 TOTP 码失败：${e && e.message}`, e);
        }
        try {
            return await client.verifyMfaTotp(serverFactorId, code);
        } catch (e) {
            throw codedError('RELOGIN_MFA_VERIFY_FAILED', `2FA 校验失败：${e && e.message}`, e);
        }
    }

    /**
     * @returns {{ok:true, code:string, pageTypes:string[]}}
     */
    async function relogin() {
        // ── ① 前置校验：注定做不成的情况，一个请求都不发就停 ──
        if (!email) throw codedError('RELOGIN_NO_EMAIL', '没有账号邮箱');
        if (!password) throw codedError('RELOGIN_NO_PASSWORD', '没有账号密码 —— 无法用密码登录');
        // 2FA 号登录一定会被要 TOTP 码；没有 secret 就走不完，别发请求把号绕进半途。
        if (!totpSecret) {
            throw codedError('RELOGIN_NO_TOTP',
                '这个号没有 totpSecret —— 已开 2FA 的号登录必然要答 TOTP 码，算不出来就登不进去');
        }

        // ── ② 基线：万一服务端走 passwordless 分支要邮箱码，得先记基线（否则会把旧码当新码）──
        //    放在一切网络动作之前，代价为零。
        let baseline = null;
        if (mail) {
            try {
                baseline = await mail.captureBaseline();
            } catch (e) {
                throw codedError('RELOGIN_BASELINE_FAILED', `记收件箱基线失败：${e && e.message}`, e);
            }
        }

        // ── ③ 建 auth 会话（普通登录，走默认 prompt=login；不带 add-password 那套 reauth 参数）──
        //    authParams 若有（如 finalizer ccaps 试探）就透传，只影响本条链、不动注册主链路。
        try {
            if (authParams) log(`[登录] authorize 追加参数：${Object.keys(authParams).join(',')}`);
            await client.bootstrapAuthorize(authUrl, authParams ? { authParams } : undefined);
        } catch (e) {
            throw codedError('RELOGIN_AUTHORIZE_FAILED', `建 auth 会话失败：${e && e.message}`, e);
        }

        // ── ④ 提交邮箱（autoPassword:false —— 由本状态机显式派发，不让 submitEmail 自动提交密码，
        //       因为它的自动提交处理不了 2FA 落点）──
        let data = null;
        try {
            data = await client.submitEmail(email, { autoPassword: false });
        } catch (e) {
            throw codedError('RELOGIN_SUBMIT_EMAIL_FAILED', `提交邮箱失败：${e && e.message}`, e);
        }

        // ── ⑤ page.type 状态机 ──
        const pageTypes = [];
        let reachedCodeReady = false;
        for (let hop = 0; hop < maxHops; hop++) {
            const pt = pageTypeOf(data).toLowerCase();
            pageTypes.push(pt || '(空)');
            log(`[登录] 第 ${hop + 1} 跳：page.type=${pt || '(无)'}`);

            // 分类顺序有讲究：
            //   MFA 先（mfa_challenge 不是终点，要答）；RESET 先于 PASSWORD（reset_password 别被 /password/ 命中）；
            //   ADD_PHONE 先于 EMAIL_OTP（phone_verification 含 "verification"，别被邮箱码分支误收）。
            if (IS_MFA(pt)) {
                data = await answerMfa(data);
                continue;
            }
            if (IS_RESET(pt)) {
                throw codedError('RELOGIN_UNEXPECTED_PWRESET',
                    `登录中落到设密码页 ${pt} —— 与预期不符（这个号应当已经有密码），停下不猜`);
            }
            if (IS_PASSWORD(pt)) {
                try {
                    data = await client.submitPassword(password, { raw: true });
                } catch (e) {
                    throw codedError('RELOGIN_PASSWORD_FAILED', `密码验证失败：${e && e.message}`, e);
                }
                continue;
            }
            if (IS_ADD_PHONE(pt)) {
                // 明确报出，而不是硬闯：obtainAuthorizationCode 处理不了 add_phone（会掉进假错误）。
                // 若实跑真在这里停，说明这个号的登录要过手机 —— 再决定是绑号还是找 skip。
                throw codedError('RELOGIN_HIT_ADD_PHONE',
                    `登录落到 ${pt} —— 这个号的重新登录需要手机步骤，relogin 暂不处理。`
                    + `已走过：${pageTypes.join(' → ')}`);
            }
            if (IS_CODE_READY(pt)) {
                reachedCodeReady = true;
                log('[登录] 到达可取 code 的落点');
                break;
            }
            if (IS_EMAIL_OTP(pt)) {
                if (!mail) {
                    throw codedError('RELOGIN_NEED_EMAIL_NO_MAIL',
                        `服务端走了 passwordless 分支要邮箱验证码，但没注入 mail —— 收不了码`);
                }
                let code = '';
                try {
                    code = await mail.waitForOpenAICode(otpMaxAttempts, otpIntervalMs, baseline, {});
                } catch (e) {
                    throw codedError('RELOGIN_WAIT_CODE_FAILED', `等邮箱验证码失败：${e && e.message}`, e);
                }
                if (!code) throw codedError('RELOGIN_NO_EMAIL_CODE', '没等到邮箱验证码');
                try {
                    data = await client.submitEmailOtp(code);
                } catch (e) {
                    throw codedError('RELOGIN_EMAIL_OTP_FAILED', `邮箱验证码校验失败：${e && e.message}`, e);
                }
                continue;
            }
            throw codedError('RELOGIN_UNEXPECTED_PAGE',
                `无法处理的 page.type=${pt || '(无)'}（已走过：${pageTypes.join(' → ')}）`);
        }

        if (!reachedCodeReady) {
            throw codedError('RELOGIN_LOOP_EXHAUSTED',
                `${maxHops} 跳仍没到可取 code 的落点 —— 服务端可能把我们绕住了。序列：${pageTypes.join(' → ')}`);
        }

        // ── ⑥ 取 authorization code（复用注册链路那套，它会走 workspace/select 或跟随外链）──
        let authCode = '';
        try {
            const cls = client.classifyNextStep(data);
            authCode = await client.obtainAuthorizationCode(cls);
        } catch (e) {
            throw codedError('RELOGIN_GET_CODE_FAILED',
                `已到落点(${pageTypes.join(' → ')})但没能取到 code：${e && e.message}`, e);
        }
        if (!authCode) throw codedError('RELOGIN_NO_CODE', '取 code 返回空 —— 不当成功');
        log('[登录] ✔ 已拿到 authorization code');
        return { ok: true, code: authCode, pageTypes };
    }

    return { relogin };
}

module.exports = { createRelogin };
