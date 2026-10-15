const MAX_RESULT_LENGTH = 262_144;
const REQUIRED_FIELDS = ['p', 'c', 'id', 'flow'];
const SDK_ERROR_TEXT = /^(?:\d+:\s*)?(SyntaxError|TypeError|ReferenceError|RangeError|EvalError|URIError|Error)\b(?::|\s)/;
const EXCEPTION_CLASSES = ['SyntaxError', 'TypeError', 'ReferenceError', 'RangeError', 'EvalError', 'URIError', 'Error'];
const EXCEPTION_KINDS = ['NULL_ACCESS', 'NOT_CALLABLE', 'MISSING_SYMBOL', 'JSON_PARSE', 'UNEXPECTED_TOKEN', 'REGEX', 'SYNTAX', 'STACK_OR_RANGE', 'OTHER'];
export const SDK_EXCEPTION_CAUSES = Object.freeze(EXCEPTION_CLASSES.flatMap(name => EXCEPTION_KINDS.map(kind => `SDK_JS_${name.toUpperCase()}_${kind}`)));

function classifyException(text) {
  const matched = SDK_ERROR_TEXT.exec(text);
  if (!matched) return null;
  const detail = text.slice(matched[0].length, matched[0].length + 2048);
  const kind = /cannot (?:read|set) propert(?:y|ies) of (?:null|undefined)|undefined is not an object|null is not an object/i.test(detail) ? 'NULL_ACCESS'
    : /is not a function|not callable/i.test(detail) ? 'NOT_CALLABLE'
    : matched[1] === 'ReferenceError' && /not defined|before initialization/i.test(detail) ? 'MISSING_SYMBOL'
    : matched[1] === 'SyntaxError' && /\bJSON\b|JSON\.parse/i.test(detail) ? 'JSON_PARSE'
    : matched[1] === 'SyntaxError' && /regular expression|invalid regex/i.test(detail) ? 'REGEX'
    : matched[1] === 'SyntaxError' && /unexpected token|invalid or unexpected token/i.test(detail) ? 'UNEXPECTED_TOKEN'
    : matched[1] === 'SyntaxError' ? 'SYNTAX'
    : matched[1] === 'RangeError' ? 'STACK_OR_RANGE' : 'OTHER';
  return `SDK_JS_${matched[1].toUpperCase()}_${kind}`;
}

function isRecord(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function decodeErrorText(value) {
  const text = value.trim();
  const direct = classifyException(text);
  if (direct) return direct;
  if (!text || text.length > MAX_RESULT_LENGTH || text.length % 4 === 1) return null;
  if (!/^[A-Za-z0-9+/_-]+={0,2}$/.test(text)) return null;
  const decoded = Buffer.from(text, 'base64').toString('utf8').trim();
  return classifyException(decoded);
}

function result(status, code, reason, safeSummary) {
  return { status, code, reason, safeSummary };
}

/**
 * Inspect SDK output without executing an SDK or contacting an upstream service.
 * A structurally valid result is not evidence of upstream authentication success.
 * Never return input values, decoded error text, or untrusted field names.
 */
export function validateSdkResult(value, { expectedFlow } = {}) {
  const safeSummary = {
    hasSoToken: false,
    hasTurnstileToken: false,
    completeFields: false,
    exceptionCause: null,
    fieldLengths: { p: 0, c: 0, id: 0, flow: 0 },
  };
  let candidate = value;
  let soToken;

  if (candidate === null || candidate === undefined || candidate === '') {
    return result('blocked', 'SDK_RESULT_MISSING', 'SDK 未返回可检查的结果，本次尝试已停止。', safeSummary);
  }
  if (typeof candidate === 'string') {
    if (candidate.length > MAX_RESULT_LENGTH) {
      return result('blocked', 'SDK_RESULT_TOO_LARGE', 'SDK 输出超过诊断大小限制，本次尝试已停止。', safeSummary);
    }
    try { candidate = JSON.parse(candidate); } catch {
      return result('blocked', 'SDK_RESULT_INVALID_JSON', 'SDK 输出格式错误，本次尝试已停止。', safeSummary);
    }
  }
  if (!isRecord(candidate)) {
    return result('blocked', 'SDK_RESULT_INVALID_STRUCTURE', 'SDK 输出结构错误，本次尝试已停止。', safeSummary);
  }

  if (Object.hasOwn(candidate, 'sentinelToken')) {
    soToken = candidate.soToken;
    candidate = candidate.sentinelToken;
    if (typeof candidate === 'string') {
      if (candidate.length > MAX_RESULT_LENGTH) {
        return result('blocked', 'SDK_RESULT_TOO_LARGE', 'SDK 输出超过诊断大小限制，本次尝试已停止。', safeSummary);
      }
      try { candidate = JSON.parse(candidate); } catch {
        return result('blocked', 'SDK_RESULT_INVALID_JSON', 'SDK 输出格式错误，本次尝试已停止。', safeSummary);
      }
    }
    if (!isRecord(candidate)) {
      return result('blocked', 'SDK_RESULT_INVALID_ENVELOPE', 'SDK 结果信封不完整，本次尝试已停止。', safeSummary);
    }
  }

  safeSummary.hasSoToken = typeof soToken === 'string' && soToken.trim().length > 0;
  safeSummary.hasSoToken ||= typeof candidate.so === 'string' && candidate.so.trim().length > 0;
  safeSummary.hasTurnstileToken = typeof candidate.t === 'string' && candidate.t.trim().length > 0;
  for (const field of REQUIRED_FIELDS) {
    if (Object.hasOwn(candidate, field) && typeof candidate[field] === 'string') {
      safeSummary.fieldLengths[field] = candidate[field].length;
    }
  }

  // Error-like output must never be promoted to a usable token.
  safeSummary.exceptionCause = safeSummary.hasTurnstileToken ? decodeErrorText(candidate.t) : null;
  if (safeSummary.exceptionCause) {
    return result('blocked', 'SDK_INTERNAL_ERROR_PAYLOAD', 'SDK 返回内部异常报文，本次尝试已停止，请使用官方交互流程验证。', safeSummary);
  }
  if (Object.hasOwn(candidate, 't') && candidate.t !== null && candidate.t !== undefined && typeof candidate.t !== 'string') {
    return result('blocked', 'SDK_RESULT_INVALID_STRUCTURE', 'SDK 输出结构错误，本次尝试已停止。', safeSummary);
  }
  for (const field of REQUIRED_FIELDS) {
    if (!Object.hasOwn(candidate, field) || typeof candidate[field] !== 'string' || candidate[field].trim().length === 0) {
      return result('blocked', 'SDK_RESULT_INCOMPLETE', 'SDK 输出缺少完整上下文字段，本次尝试已停止。', safeSummary);
    }
    if (candidate[field].length > MAX_RESULT_LENGTH) {
      return result('blocked', 'SDK_RESULT_TOO_LARGE', 'SDK 输出超过诊断大小限制，本次尝试已停止。', safeSummary);
    }
  }
  safeSummary.completeFields = true;
  if (expectedFlow !== undefined && candidate.flow !== expectedFlow) {
    return result('blocked', 'SDK_FLOW_MISMATCH', 'SDK 输出与当前操作不一致，本次尝试已停止。', safeSummary);
  }
  if (typeof candidate.t === 'string' && candidate.t.length > MAX_RESULT_LENGTH) {
    return result('blocked', 'SDK_RESULT_TOO_LARGE', 'SDK 输出超过诊断大小限制，本次尝试已停止。', safeSummary);
  }
  return result('ok', 'SDK_RESULT_STRUCTURALLY_VALID', 'SDK 输出结构检查通过，仍需正常验证。', safeSummary);
}
