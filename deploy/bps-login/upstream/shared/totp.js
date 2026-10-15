const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
const SUPPORTED = new Set(['SHA1', 'SHA256', 'SHA512']);

function normalizeAlgorithm(value) {
  const algorithm = String(value || 'SHA1').toUpperCase().replace(/-/g, '');
  if (!SUPPORTED.has(algorithm)) throw new Error('仅支持 SHA1、SHA256 和 SHA512。');
  return algorithm;
}

export function decodeBase32(input) {
  const original = String(input).trim().replace(/[\s-]/g, '').toUpperCase();
  if (!original || !/^[A-Z2-7]+={0,6}$/.test(original)) throw new Error('密钥必须是有效的 Base32 字符串。');
  const secret = original.replace(/=+$/, '');
  if ([1, 3, 6].includes(secret.length % 8)) throw new Error('Base32 密钥长度不正确。');
  if (original.includes('=') && (original.length % 8 !== 0 || ![2, 4, 5, 7].includes(secret.length % 8))) throw new Error('Base32 填充不正确。');
  let bits = 0, buffer = 0;
  const bytes = [];
  for (const char of secret) {
    buffer = buffer << 5 | ALPHABET.indexOf(char); bits += 5;
    if (bits >= 8) { bits -= 8; bytes.push(buffer >>> bits & 255); }
  }
  if (bits && (buffer & ((1 << bits) - 1)) !== 0) throw new Error('Base32 密钥末尾包含无效填充位。');
  if (!bytes.length) throw new Error('密钥不能为空。');
  return Uint8Array.from(bytes);
}

function validateEntry(value) {
  const secret = String(value.secret || '').trim().replace(/[\s-]/g, '').replace(/=+$/, '').toUpperCase();
  // Validate before stripping padding so malformed URI keys are rejected.
  decodeBase32(value.secret);
  const algorithm = normalizeAlgorithm(value.algorithm);
  const digits = Number(value.digits ?? 6);
  const period = Number(value.period ?? 30);
  if (![6, 8].includes(digits)) throw new Error('验证码位数仅支持 6 或 8。');
  if (!Number.isInteger(period) || period < 1 || period > 3600) throw new Error('周期必须是 1 至 3600 秒的整数。');
  return { label: String(value.label || '未命名密钥').slice(0, 200), secret, algorithm, digits, period };
}

function parseUri(input, defaultLabel) {
  let uri;
  try { uri = new URL(input); } catch { throw new Error('otpauth 链接格式错误。'); }
  if (uri.protocol !== 'otpauth:' || uri.hostname.toLowerCase() !== 'totp') throw new Error('仅支持 otpauth://totp 链接。');
  if (uri.username || uri.password || uri.port || uri.hash) throw new Error('otpauth 链接包含不支持的字段。');
  const params = uri.searchParams;
  const allowed = new Set(['secret', 'issuer', 'algorithm', 'digits', 'period']);
  for (const key of params.keys()) {
    if (!allowed.has(key)) throw new Error('otpauth 链接包含不支持的参数。');
    if (params.getAll(key).length !== 1) throw new Error('otpauth 参数不能重复。');
  }
  if (!params.has('secret') || !params.get('secret')) throw new Error('otpauth 链接缺少 secret。');
  let label;
  try { label = decodeURIComponent(uri.pathname.replace(/^\//, '')); } catch { throw new Error('otpauth 标签编码错误。'); }
  if (!label) throw new Error('otpauth 链接缺少账号标签。');
  const issuer = params.get('issuer');
  if (issuer && label.includes(':') && label.split(':')[0] !== issuer) throw new Error('标签中的发行方与 issuer 不一致。');
  return validateEntry({ label: defaultLabel || label, secret: params.get('secret'), algorithm: params.get('algorithm') || 'SHA1', digits: params.get('digits') ?? 6, period: params.get('period') ?? 30 });
}

export function parseTotpInput(input) {
  const entries = [], errors = [];
  const data = String(input ?? '').replace(/^\uFEFF/, '');
  data.split(/\r?\n/).forEach((line, index) => {
    const value = line.trim();
    if (!value) return;
    try {
      let entry;
      if (/^otpauth:/i.test(value)) entry = parseUri(value);
      else {
        const delimiter = value.includes('----') ? '----' : value.includes('\t') ? '\t' : value.includes('——') ? '——' : null;
        if (delimiter) {
          const cells = value.split(delimiter).map((cell) => cell.trim());
          if (cells.length !== 2) throw new Error('每行请使用“标签----密钥”或单个 otpauth 链接。');
          entry = /^otpauth:/i.test(cells[1]) ? parseUri(cells[1], cells[0]) : validateEntry({ label: cells[0], secret: cells[1] });
        } else { entry = validateEntry({ label: `密钥 ${index + 1}`, secret: value }); }
      }
      entries.push(entry);
    } catch (error) { errors.push({ line: index + 1, code: 'INVALID_TOTP', message: error.message }); }
  });
  return { entries, errors };
}

export async function generateTotp(entry, timeMs = Date.now()) {
  const normalized = validateEntry(typeof entry === 'string' ? { secret: entry } : entry || {});
  if (!Number.isFinite(timeMs) || timeMs < 0 || !Number.isSafeInteger(Math.floor(timeMs / 1000))) throw new Error('时间戳不合法。');
  if (!globalThis.crypto?.subtle) throw new Error('当前浏览器不支持安全密码计算，请使用本地可信页面。');
  const seconds = Math.floor(timeMs / 1000);
  const counter = BigInt(Math.floor(seconds / normalized.period));
  const bytes = new Uint8Array(8);
  new DataView(bytes.buffer).setBigUint64(0, counter, false);
  const key = await crypto.subtle.importKey('raw', decodeBase32(normalized.secret), { name: 'HMAC', hash: normalized.algorithm.replace(/^SHA/, 'SHA-') }, false, ['sign']);
  const digest = new Uint8Array(await crypto.subtle.sign('HMAC', key, bytes));
  const offset = digest[digest.length - 1] & 15;
  const number = ((digest[offset] & 127) << 24 | digest[offset + 1] << 16 | digest[offset + 2] << 8 | digest[offset + 3]) >>> 0;
  return { code: String(number % 10 ** normalized.digits).padStart(normalized.digits, '0'), remaining: normalized.period - seconds % normalized.period, period: normalized.period };
}
