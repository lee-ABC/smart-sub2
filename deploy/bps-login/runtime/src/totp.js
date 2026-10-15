'use strict';
/*
 * TOTP（RFC 6238）生成器 —— 给账号开 2FA 用。
 *
 * 【为什么自己写】仓库里原本没有任何 TOTP 能力（没有 base32、没有 hmac）。
 *   ChatGPT 的 2FA 用的是标准 TOTP：SHA-1 / 30 秒周期 / 6 位 / base32 密钥，
 *   这是 RFC 6238 的默认档，算法是公开标准，不存在"猜"的成分 ——
 *   而且能用 RFC 附录里的官方测试向量逐条验证（见 test_totp.js）。
 *
 * 【与"编不出来的值坚决不发"的关系】这里生成的 6 位码不是编的：
 *   它是服务端给我们的 secret 经标准算法算出来的确定值，
 *   服务端用同一个 secret 同一套算法能算出同一个码 —— 这是可验证的推导，不是猜测。
 *
 * ⚠ secret 是凭据：本模块只在内存里用，绝不打日志。调用方落库时自行决定脱敏口径。
 */
const crypto = require('crypto');

const BASE32_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

/**
 * 解码 base32（RFC 4648，无填充也接受）。
 * @param {string} input 形如 'JBSWY3DPEHPK3PXP'，允许空格/连字符/小写/尾部 '='
 * @returns {Buffer}
 * @throws 遇到字母表之外的字符就抛 —— 宁可失败也不要静默解成错误的密钥，
 *         那会算出一个"看起来正常但永远验不过"的码。
 */
function base32Decode(input) {
    const clean = String(input == null ? '' : input)
        .toUpperCase().replace(/[\s-]/g, '').replace(/=+$/, '');
    if (!clean) return Buffer.alloc(0);
    let bits = 0;
    let value = 0;
    const out = [];
    for (const ch of clean) {
        const idx = BASE32_ALPHABET.indexOf(ch);
        if (idx < 0) throw new Error(`base32 里出现非法字符 ${JSON.stringify(ch)}`);
        value = (value << 5) | idx;
        bits += 5;
        if (bits >= 8) {
            bits -= 8;
            out.push((value >>> bits) & 0xff);
        }
    }
    return Buffer.from(out);
}

/**
 * HOTP（RFC 4226）：给定密钥与计数器算一个码。
 * @param {Buffer} key
 * @param {number|bigint} counter
 * @param {{digits?: number, algorithm?: string}} [opts]
 */
function hotp(key, counter, opts = {}) {
    const digits = Number(opts.digits) || 6;
    const algorithm = String(opts.algorithm || 'sha1').toLowerCase();
    // 计数器是 8 字节大端。用 BigInt 写，避免 32 位溢出
    //（RFC 的测试向量里 T=20000000000 就超过 32 位）。
    const buf = Buffer.alloc(8);
    buf.writeBigUInt64BE(BigInt(counter));
    const mac = crypto.createHmac(algorithm, key).update(buf).digest();
    // 动态截断（RFC 4226 §5.3）
    const offset = mac[mac.length - 1] & 0x0f;
    const bin = ((mac[offset] & 0x7f) << 24)
        | ((mac[offset + 1] & 0xff) << 16)
        | ((mac[offset + 2] & 0xff) << 8)
        | (mac[offset + 3] & 0xff);
    return String(bin % (10 ** digits)).padStart(digits, '0');
}

/**
 * TOTP（RFC 6238）。
 * @param {string} secretBase32 服务端给的 base32 密钥
 * @param {{atMs?: number, step?: number, digits?: number, algorithm?: string, t0?: number}} [opts]
 *   atMs  取码时刻（毫秒）。默认 Date.now()；测试里必须显式传，保证可复现。
 *   step  周期秒数，默认 30
 *   digits 位数，默认 6
 * @returns {string} 形如 '492039'
 */
function totp(secretBase32, opts = {}) {
    const key = base32Decode(secretBase32);
    if (!key.length) throw new Error('TOTP 密钥为空 —— 拿不到 secret 就不要算码');
    const step = Number(opts.step) || 30;
    const t0 = Number(opts.t0) || 0;
    const atMs = Number.isFinite(Number(opts.atMs)) ? Number(opts.atMs) : Date.now();
    const counter = BigInt(Math.floor((Math.floor(atMs / 1000) - t0) / step));
    return hotp(key, counter, opts);
}

/**
 * 距离当前周期结束还有多少毫秒。
 *
 * 【为什么需要】提交验证码时如果正好卡在周期边界，服务端收到时码已经翻篇，
 *   会白白吃一次 invalid_code。调用方可以据此决定"要不要等下一个周期再发"。
 * @returns {number} 剩余毫秒（0~step*1000）
 */
function msUntilNextStep(opts = {}) {
    const step = (Number(opts.step) || 30) * 1000;
    const atMs = Number.isFinite(Number(opts.atMs)) ? Number(opts.atMs) : Date.now();
    return step - (atMs % step);
}

/**
 * 从 otpauth:// URL 里取出 secret。
 * 服务端同时给 secret 和 qr_code_secret_url，两者应当一致；不一致时调用方该报警而不是二选一。
 * @returns {string} base32 secret；解析不出返回空串
 */
function secretFromOtpauthUrl(url) {
    const s = String(url == null ? '' : url);
    const m = /[?&]secret=([^&#\s]+)/i.exec(s);
    if (!m) return '';
    try { return decodeURIComponent(m[1]); } catch (_) { return m[1]; }
}

module.exports = { totp, hotp, base32Decode, msUntilNextStep, secretFromOtpauthUrl, BASE32_ALPHABET };
