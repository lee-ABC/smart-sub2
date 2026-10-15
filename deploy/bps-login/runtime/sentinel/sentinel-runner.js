#!/usr/bin/env node
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const crypto = require("node:crypto");
const { performance } = require("node:perf_hooks");
const { createNavigatorPrototype } = require("./navigator-surface");

function readArgs(argv) {
  const args = {};
  for (let i = 0; i < argv.length; i++) {
    const item = argv[i];
    if (!item.startsWith("--")) continue;
    const key = item.slice(2);
    const next = argv[i + 1];
    // ★ 用 === undefined 而不是 !next：空串是【显式传入的空值】，不是"没给值的开关"。
    //   老写法把 `--build-id ""` 判成开关、赋成字符串 "1"，于是 buildId=1 一路传下去，
    //   最终让 p[6] 发出 "c/1/_" —— 而线上实测是 null。
    //   这是通用缺陷：任何需要传空串的参数都会中招。
    if (next === undefined || next.startsWith("--")) {
      args[key] = "1";
      continue;
    }
    args[key] = next;
    i++;
  }
  return args;
}

function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exit(1);
}

function parseJson(text, source) {
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new Error(`${source} 不是合法 JSON：${error.message}`);
  }
}

function pick(...values) {
  for (const value of values) {
    if (value !== undefined && value !== null && value !== "") return value;
  }
  return "";
}

function truthy(value) {
  return value === true || value === "1" || value === "true" || value === "yes";
}

function readConfig(args) {
  const explicitPath = args.config || process.env.SENTINEL_CONFIG;
  const candidates = explicitPath
    ? [path.resolve(explicitPath)]
    : [
        path.resolve(process.cwd(), "sentinel.config.json"),
        path.resolve(process.cwd(), "tools", "sentinel.config.json"),
        path.resolve(__dirname, "sentinel.config.json"),
        path.resolve(__dirname, "..", "sentinel.config.json"),
      ];

  for (const filePath of candidates) {
    if (!fs.existsSync(filePath)) continue;
    return {
      path: filePath,
      data: parseJson(fs.readFileSync(filePath, "utf8"), filePath),
    };
  }

  return { path: null, data: {} };
}

function configGetter(config) {
  return (...keys) => {
    for (const key of keys) {
      if (config[key] !== undefined && config[key] !== null && config[key] !== "") {
        return config[key];
      }
    }
    return "";
  };
}

function normalizeList(value, fallback) {
  const source = Array.isArray(value) ? value.join(",") : pick(value, fallback);
  return String(source)
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function parseSecChBrands(value, major = "") {
  const text = String(value || "").trim();
  if (!text) {
    return [
      { brand: "Not)A;Brand", version: "8" },
      { brand: "Chromium", version: String(major || "") },
      { brand: "Google Chrome", version: String(major || "") },
    ];
  }
  const out = [];
  const re = /"([^"]+)"\s*;\s*v="([^"]+)"/g;
  let m;
  while ((m = re.exec(text))) out.push({ brand: m[1], version: m[2] });
  return out.length ? out : text.split(",").map((part) => ({ brand: part.trim(), version: String(major || "") })).filter((x) => x.brand);
}

function xorDecode(text, key) {
  let output = "";
  const decoded = atobBinary(text);
  for (let i = 0; i < decoded.length; i++) {
    output += String.fromCharCode(decoded.charCodeAt(i) ^ key.charCodeAt(i % key.length));
  }
  return output;
}

function decodeDx(dx, proof) {
  return JSON.parse(xorDecode(dx, proof));
}

function normalizeChallenge(raw) {
  if (typeof raw === "string") {
    const trimmed = raw.trim();
    if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return trimmed;
    raw = parseJson(trimmed, "challenge 字符串");
  }

  const candidates = [
    raw?.cachedChatReq,
    raw?.result?.cachedChatReq,
    raw?.data?.cachedChatReq,
    raw?.data,
    raw,
  ];

  for (const candidate of candidates) {
    if (!candidate || typeof candidate !== "object") continue;
    if (candidate.proofofwork || candidate.token || candidate.turnstile || candidate.so) {
      return candidate;
    }
  }

  throw new Error("challenge 缺少 cachedChatReq/proofofwork/token 字段，无法喂给 SDK");
}

function readChallengeFile(filePath) {
  const absolutePath = path.resolve(filePath);
  const raw = fs.readFileSync(absolutePath, "utf8");
  return normalizeChallenge(parseJson(raw, absolutePath));
}

const OFFICIAL_CHALLENGE_URL = "https://chatgpt.com/backend-api/sentinel/req";

function headerMapFromEnv(options = {}) {
  const headers = {
    accept: "*/*",
    "content-type":
      options.contentType ||
      (options.ignoreEnv ? "" : process.env.SENTINEL_CONTENT_TYPE) ||
      "text/plain;charset=UTF-8",
  };
  const cookie =
    options.cookie ||
    (options.ignoreEnv ? "" : process.env.SENTINEL_COOKIE || process.env.CHATGPT_COOKIE);
  const authorization =
    options.bearer ||
    (options.ignoreEnv ? "" : process.env.SENTINEL_AUTHORIZATION || process.env.CHATGPT_BEARER_TOKEN);
  const userAgent = options.userAgent || (options.ignoreEnv ? "" : process.env.SENTINEL_USER_AGENT);

  if (cookie) headers.cookie = cookie;
  if (authorization) {
    headers.authorization = authorization.toLowerCase().startsWith("bearer ")
      ? authorization
      : `Bearer ${authorization}`;
  }
  if (userAgent) {
    headers["user-agent"] = userAgent;
  }
  if (options.pageUrl) headers.referer = options.pageUrl;
  if (options.origin) headers.origin = options.origin;
  if (options.deviceId) headers["oai-device-id"] = options.deviceId;
  if (process.env.SENTINEL_HEADERS_JSON) {
    Object.assign(headers, parseJson(process.env.SENTINEL_HEADERS_JSON, "SENTINEL_HEADERS_JSON"));
  }
  return headers;
}

function assertAllowedChallengeHost(challengeUrl, officialMode) {
  const host = new URL(challengeUrl).hostname.toLowerCase();
  const allowed = (process.env.SENTINEL_ALLOW_HOST || "")
    .split(",")
    .map((item) => item.trim().toLowerCase())
    .filter(Boolean);

  if ((host === "chatgpt.com" || host.endsWith(".chatgpt.com")) && !officialMode && !allowed.includes(host)) {
    throw new Error(
      "为避免误打真实生产接口，默认不请求 chatgpt.com。若这是比赛授权接口，请使用 --official 或设置 SENTINEL_ALLOW_HOST=chatgpt.com。"
    );
  }
}

// ★ IPC 模式：与父进程握手换题，保证「加密 dx 用的 p」与「解密 dx 用的 p」是同一个。
//   父进程侧见 src/sentinelClient.js 的 ipc 分支。
const ipcMode = process.argv.includes("--ipc") && typeof process.send === "function";
// 是否额外产出 so token（输出改为信封格式，见 main 末尾）
const emitSo = process.argv.includes("--emit-so");

function requestChallengeViaIpc(proof, flow) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      process.removeListener('message', onMsg);
      reject(new Error('IPC 换题超时：父进程 30s 内没回 challenge'));
    }, 30000);
    function onMsg(msg) {
      if (!msg || msg.type !== 'challenge') return;
      clearTimeout(timer);
      process.removeListener('message', onMsg);
      if (msg.error) reject(new Error('父进程换题失败: ' + msg.error));
      else resolve(msg.challenge);
    }
    process.on('message', onMsg);
    process.send({ type: 'need-challenge', p: proof, flow });
  });
}

async function fetchChallenge(challengeUrl, flow, proof, deviceId, options = {}) {
  assertAllowedChallengeHost(challengeUrl, options.officialMode);
  const hasCookie = Boolean(
    options.cookie || (options.ignoreEnv ? "" : process.env.SENTINEL_COOKIE || process.env.CHATGPT_COOKIE)
  );
  const hasBearer = Boolean(
    options.bearer ||
      (options.ignoreEnv ? "" : process.env.SENTINEL_AUTHORIZATION || process.env.CHATGPT_BEARER_TOKEN)
  );
  if (options.officialMode && !hasCookie && !hasBearer) {
    throw new Error("官方接口模式至少需要 Cookie 或 Bearer；请传 --cookie 或 --bearer。");
  }
  const body = JSON.stringify({ p: proof, id: deviceId, flow });
  const response = await fetch(challengeUrl, {
    method: "POST",
    headers: headerMapFromEnv({
      pageUrl: options.pageUrl,
      origin: new URL(challengeUrl).origin,
      userAgent: options.userAgent,
      deviceId,
      cookie: options.cookie,
      bearer: options.bearer,
      contentType: options.contentType,
      ignoreEnv: options.ignoreEnv,
    }),
    body,
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`challenge API 返回 HTTP ${response.status}：${text.slice(0, 300)}`);
  }
  return normalizeChallenge(text);
}

function createEventTarget() {
  const listeners = new Map();
  return {
    addEventListener(type, listener) {
      const bucket = listeners.get(type) || [];
      bucket.push(listener);
      listeners.set(type, bucket);
    },
    removeEventListener(type, listener) {
      const bucket = listeners.get(type) || [];
      listeners.set(
        type,
        bucket.filter((item) => item !== listener)
      );
    },
    dispatchEvent(event) {
      const bucket = listeners.get(event.type) || [];
      for (const listener of [...bucket]) listener.call(this, event);
    },
  };
}

function btoaBinary(value) {
  return Buffer.from(String(value), "binary").toString("base64");
}

function atobBinary(value) {
  return Buffer.from(String(value), "base64").toString("binary");
}

function createStorage() {
  const values = new Map();
  return {
    get length() {
      return values.size;
    },
    key(index) {
      return [...values.keys()][Number(index)] ?? null;
    },
    getItem(key) {
      const name = String(key);
      return values.has(name) ? values.get(name) : null;
    },
    setItem(key, value) {
      values.set(String(key), String(value));
    },
    removeItem(key) {
      values.delete(String(key));
    },
    clear() {
      values.clear();
    },
  };
}

function createDomRect(width = 0, height = 0) {
  return {
    x: 0,
    y: 0,
    width,
    height,
    top: 0,
    left: 0,
    right: width,
    bottom: height,
    toJSON() {
      return {
        x: this.x,
        y: this.y,
        width: this.width,
        height: this.height,
        top: this.top,
        left: this.left,
        right: this.right,
        bottom: this.bottom,
      };
    },
  };
}


function createDomTokenList(initial = []) {
  const tokens = new Set(initial);
  const api = {
    add(...items) { for (const item of items) if (item) tokens.add(String(item)); },
    remove(...items) { for (const item of items) tokens.delete(String(item)); },
    contains(item) { return tokens.has(String(item)); },
    toggle(item, force) {
      const token = String(item);
      const shouldAdd = force === undefined ? !tokens.has(token) : Boolean(force);
      if (shouldAdd) tokens.add(token); else tokens.delete(token);
      return shouldAdd;
    },
    replace(oldToken, newToken) {
      if (!tokens.has(String(oldToken))) return false;
      tokens.delete(String(oldToken));
      tokens.add(String(newToken));
      return true;
    },
    item(index) { return [...tokens][Number(index)] || null; },
    get length() { return tokens.size; },
    toString() { return [...tokens].join(" "); },
    [Symbol.iterator]() { return tokens[Symbol.iterator](); },
  };
  Object.defineProperty(api, Symbol.toStringTag, { value: "DOMTokenList" });
  return api;
}

function createStyleDeclaration() {
  const values = Object.create(null);
  return {
    get cssText() {
      return Object.entries(values).map(([k, v]) => `${k}: ${v};`).join(" ");
    },
    set cssText(text) {
      for (const part of String(text || "").split(";")) {
        const idx = part.indexOf(":");
        if (idx > 0) this.setProperty(part.slice(0, idx).trim(), part.slice(idx + 1).trim());
      }
    },
    get length() { return Object.keys(values).length; },
    item(index) { return Object.keys(values)[Number(index)] || ""; },
    getPropertyValue(name) { return values[String(name)] || ""; },
    setProperty(name, value) { values[String(name)] = String(value); this[String(name)] = String(value); },
    removeProperty(name) { const key = String(name); const old = values[key] || ""; delete values[key]; delete this[key]; return old; },
  };
}

function createElementNode(tagName, ownerDocument, rect = createDomRect()) {
  const target = createEventTarget();
  const children = [];
  const attrs = new Map();
  const dataset = {};
  const element = {
    nodeType: 1,
    nodeName: String(tagName).toUpperCase(),
    tagName: String(tagName).toUpperCase(),
    ownerDocument,
    parentNode: null,
    parentElement: null,
    children,
    childNodes: children,
    firstChild: null,
    lastChild: null,
    style: createStyleDeclaration(),
    dataset,
    classList: createDomTokenList(),
    textContent: "",
    innerHTML: "",
    appendChild(node) {
      children.push(node);
      node.parentNode = element;
      node.parentElement = element;
      element.firstChild = children[0] || null;
      element.lastChild = children[children.length - 1] || null;
      return node;
    },
    removeChild(node) {
      const index = children.indexOf(node);
      if (index >= 0) children.splice(index, 1);
      if (node) { node.parentNode = null; node.parentElement = null; }
      element.firstChild = children[0] || null;
      element.lastChild = children[children.length - 1] || null;
      return node;
    },
    insertBefore(node, before) {
      const index = children.indexOf(before);
      if (index < 0) return this.appendChild(node);
      children.splice(index, 0, node);
      node.parentNode = element;
      node.parentElement = element;
      element.firstChild = children[0] || null;
      element.lastChild = children[children.length - 1] || null;
      return node;
    },
    remove() { if (element.parentNode?.removeChild) element.parentNode.removeChild(element); },
    setAttribute(name, value) {
      const key = String(name);
      const val = String(value);
      attrs.set(key, val);
      if (key === "class") element.classList = createDomTokenList(val.split(/\s+/).filter(Boolean));
      if (key === "id") element.id = val;
      if (key.startsWith("data-")) {
        const prop = key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        dataset[prop] = val;
      }
    },
    getAttribute(name) { return attrs.get(String(name)) ?? null; },
    hasAttribute(name) { return attrs.has(String(name)); },
    removeAttribute(name) { attrs.delete(String(name)); },
    getAttributeNames() { return [...attrs.keys()]; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    matches() { return false; },
    closest() { return null; },
    getBoundingClientRect() { return rect; },
    addEventListener: target.addEventListener,
    removeEventListener: target.removeEventListener,
    dispatchEvent: target.dispatchEvent,
    click() { target.dispatchEvent.call(element, { type: "click", target: element }); },
  };
  Object.defineProperty(element, "attributes", {
    get() { return [...attrs].map(([name, value]) => ({ name, value, nodeName: name, nodeValue: value, valueOf: () => value })); },
  });
  return element;
}


function createIntlObject(options) {
  const intlObject = Object.create(Intl);
  const NativeDateTimeFormat = Intl.DateTimeFormat;
  function DateTimeFormatMock(locales, formatOptions = {}) {
    const mergedOptions = { ...(formatOptions || {}) };
    if (options.timeZone) mergedOptions.timeZone = options.timeZone;
    const fmt = new NativeDateTimeFormat(locales || options.languages || options.language, mergedOptions);
    const nativeResolvedOptions = fmt.resolvedOptions.bind(fmt);
    Object.defineProperty(fmt, "resolvedOptions", {
      value: () => {
        const resolved = nativeResolvedOptions();
        if (options.timeZone) resolved.timeZone = options.timeZone;
        if (options.language) resolved.locale = options.language;
        return resolved;
      },
    });
    return fmt;
  }
  DateTimeFormatMock.prototype = NativeDateTimeFormat.prototype;
  Object.setPrototypeOf(DateTimeFormatMock, NativeDateTimeFormat);
  intlObject.DateTimeFormat = DateTimeFormatMock;
  return intlObject;
}

function createPerformanceObserver(observerSet) {
  return class PerformanceObserverMock {
    constructor(callback) { this.callback = callback; this._observed = false; this._types = new Set(); }
    observe(options = {}) {
      this._observed = true;
      if (options.type) this._types.add(String(options.type));
      if (Array.isArray(options.entryTypes)) for (const type of options.entryTypes) this._types.add(String(type));
      observerSet.add(this);
    }
    disconnect() { this._observed = false; observerSet.delete(this); }
    takeRecords() { return []; }
    _notify(entry) {
      if (!this._observed) return;
      if (this._types.size && !this._types.has(entry.entryType)) return;
      try { this.callback({ getEntries: () => [entry], getEntriesByType: (type) => entry.entryType === String(type) ? [entry] : [] }); } catch {}
    }
    static get supportedEntryTypes() { return ["navigation", "resource", "paint", "mark", "measure"]; }
  };
}

function createNetworkInformation() {
  const target = createEventTarget();
  const info = {
    downlink: 10,
    effectiveType: "4g",
    rtt: 50,
    saveData: false,
    type: "wifi",
    onchange: null,
    addEventListener: target.addEventListener,
    removeEventListener: target.removeEventListener,
    dispatchEvent: target.dispatchEvent,
  };
  Object.defineProperty(info, Symbol.toStringTag, { value: "NetworkInformation" });
  return info;
}

function createCookieJar(initialCookie = "") {
  const values = new Map();
  for (const part of String(initialCookie || "").split(";")) {
    const trimmed = part.trim();
    if (!trimmed) continue;
    const idx = trimmed.indexOf("=");
    if (idx <= 0) continue;
    values.set(trimmed.slice(0, idx), trimmed.slice(idx + 1));
  }
  return {
    get cookie() {
      return [...values.entries()].map(([k, v]) => `${k}=${v}`).join("; ");
    },
    set cookie(value) {
      const first = String(value || "").split(";")[0];
      const idx = first.indexOf("=");
      if (idx > 0) values.set(first.slice(0, idx).trim(), first.slice(idx + 1).trim());
    },
  };
}

// ★★★ 2026-08-16：假原生函数的源码伪装。
//
// 【原来的做法为什么不够】只在函数【自有】的 toString 上伪装。实测：
//     fn.toString()                     -> function vibrate() { [native code] }   ✔
//     String(fn)                        -> function vibrate() { [native code] }   ✔
//     Function.prototype.toString.call(fn)
//         -> function (...args) { return impl.apply(this, args); }               ✗ 漏了
//   `Function.prototype.toString.call(x)` 直接走内建方法，【绕过】对象自有的 toString ——
//   而这恰恰是 FP 库检测假原生函数的标准写法。也就是说我们 VM 里每一个假原生
//   （vibrate / getBattery / sendBeacon / 以及 navigator 的每个 getter）都会漏出真源码。
//
// 【现在的做法】维护一张 函数→伪装源码 的 WeakMap，并在 VM 里把
//   Function.prototype.toString 本身换掉，查表命中就返回伪装串。
//   ⚠ 替换函数【自己】也要登记，否则「查 toString 的 toString」立刻露馅。
//   ⚠ 注入用的临时全局必须在打完补丁后删掉，否则它会变成 window 的一个自有键 ——
//     而 sentinel 正好抽 window 键（p[11]/p[12]），等于自己加了个标记。
const NATIVE_FN_SRC = new WeakMap();

function markNative(fn, src) {
  try { NATIVE_FN_SRC.set(fn, src); } catch (_) {}
  return fn;
}

// ★★ 2026-08-16：真原生函数的 length 是【确定值】，而我们用 function(...args) 写的假原生
//   rest 参数不计入 length，于是全部恒为 0。实测本机真 Chrome 150：
//       vibrate 1 · sendBeacon 1 · getUserMedia 3 · registerProtocolHandler 2 · javaEnabled 0
//   （注意 sendBeacon 是 1 不是 2 —— 我原本猜的 2，实测打脸，所以这张表是【问真 Chrome 要的】，
//     不是查文档或凭印象写的。完整 28 项见 NATIVE_FN_LENGTHS。）
//   CreepJS 这类检测器会查函数 length；恒为 0 是一条确定性差异，不是概率性的。
const NATIVE_FN_LENGTHS = {
  // Navigator.prototype（真 Chrome 150 实测 28 项）
  adAuctionComponents: 1, canLoadAdAuctionFencedFrame: 0, canShare: 0, clearAppBadge: 0,
  clearOriginJoinedAdInterestGroups: 1, createAuctionNonce: 0, deprecatedReplaceInURN: 2,
  deprecatedURNToURL: 1, getBattery: 0, getGamepads: 0, getInstalledRelatedApps: 0,
  getInterestGroupAdAuctionData: 1, getUserMedia: 3, javaEnabled: 0, joinAdInterestGroup: 1,
  leaveAdInterestGroup: 0, registerProtocolHandler: 2, requestMIDIAccess: 0,
  requestMediaKeySystemAccess: 2, runAdAuction: 1, sendBeacon: 1, setAppBadge: 0, share: 0,
  unregisterProtocolHandler: 2, updateAdInterestGroups: 0, vibrate: 1, webkitGetUserMedia: 3,
  // window 上的构造器（同批实测）
  Worker: 1, SharedWorker: 1, BroadcastChannel: 1, RTCPeerConnection: 0, OffscreenCanvas: 2,
  FontFace: 2, Image: 0, ResizeObserver: 1, IntersectionObserver: 1, ReportingObserver: 1,
  Notification: 1, HTMLCanvasElement: 0,
  // 真 Chrome 里 getter 的 length 恒为 0，所以 "get xxx" 不用列 —— 默认就是 0。
};

function makeNativeFunction(name, impl = () => undefined, lengthOverride) {
  const fn = function (...args) { return impl.apply(this, args); };
  Object.defineProperty(fn, "name", { value: name });
  const len = lengthOverride != null ? lengthOverride : (NATIVE_FN_LENGTHS[name] ?? 0);
  // ⚠ length 在函数上是 configurable 的，可以改；不改就永远是 0（rest 参数不计数）
  try { Object.defineProperty(fn, "length", { value: len, writable: false, enumerable: false, configurable: true }); } catch (_) {}
  const src = `function ${name}() { [native code] }`;
  Object.defineProperty(fn, "toString", { value: () => src });
  return markNative(fn, src);
}

/**
 * 把 window 的键表补成真 Chrome 的样子。清单来自实测（见 sentinel/window-surface.js 文件头）。
 *
 * 【三件事，缺一不可】
 *   ① 补 on* 事件处理器（119 个）—— 真浏览器上值恒为 null
 *   ② 补缺的方法/属性（58 个）—— 只补【存在性与形态】，不实现功能：
 *      sdk 实测从不调用它们（--debug-fp 量过 canvas/WebGL/audio 读取为 0），
 *      造一个真能跑的 alert/open 反而引入新的行为差异。
 *   ③ 把 JS 内建与 Web API 构造器改回【不可枚举】（53 个）—— 这条最要紧：
 *      真浏览器里 Object.keys(window) 永远不返回 Array/Math/JSON/Promise，
 *      而 vm.createContext({...}) 把传进去的一切变成可枚举自有属性。
 *      我们铸出的 p[12] 因此抽到过 "Promise" —— 真浏览器不可能出现，单样本判死。
 *
 * ⚠ 只补【不存在】的键，绝不覆盖已有实现。
 * ⚠ 必须在 sdk 加载【之前】跑完：sdk 一起来就会往 window 上写 __sentinel_* / __oai_so_*，
 *   那些是它自己的痕迹，不能被我们的补丁碰到。
 */
function applyWindowSurface(context) {
  const surface = require("./window-surface");
  // ⚠ requestIdleCallback / cancelIdleCallback 【不在】这张表里 —— 它们是人格相关的
  //   （Safari 没有），由 --request-idle-callback 单独管。生成器已把它们排除，
  //   这里不要再补一层过滤（第一版在这写了 opts.exposeRequestIdleCallback，
  //   那个变量在本函数作用域里根本不存在，直接 ReferenceError 把整轮打挂）。
  context.__ws = {
    on: surface.ON_HANDLERS,
    other: surface.OTHER_KEYS,
    nonEnum: surface.NON_ENUMERABLE,
    mk: makeNativeFunction,
  };
  vm.runInContext(`(function () {
    const { on, other, nonEnum, mk } = __ws;
    const g = globalThis;
    for (const k of on) {
      if (k in g) continue;
      Object.defineProperty(g, k, { value: null, writable: true, enumerable: true, configurable: true });
    }
    for (const k of other) {
      if (k in g) continue;
      // 名字像方法的给原生函数存根，其余给一个惰性值；两者都不实现真功能
      const isMethodish = /^(alert|confirm|prompt|open|close|print|stop|find|moveBy|moveTo|resizeBy|resizeTo|scroll|postMessage|createImageBitmap|structuredClone|reportError|queryLocalFonts|getSelection|getScreenDetails|captureEvents|releaseEvents|show[A-Z]|webkit[A-Z]|fetchLater|cancelIdleCallback|requestIdleCallback)/.test(k);
      Object.defineProperty(g, k, {
        value: isMethodish ? mk(k) : (k === 'origin' ? '' : (/^(crossOriginIsolated|isSecureContext|credentialless|originAgentCluster)$/.test(k) ? true : null)),
        writable: true, enumerable: true, configurable: true,
      });
    }
    for (const k of nonEnum) {
      const d = Object.getOwnPropertyDescriptor(g, k);
      if (!d || d.enumerable === false || d.configurable === false) continue;
      Object.defineProperty(g, k, Object.assign({}, d, { enumerable: false }));
    }
  })()`, context, { filename: "window-surface-patch" });
  // 擦掉注入痕迹：留着它就是 window 上一个真浏览器没有的自有键
  try { delete context.__ws; } catch (_) {}
  try { if (context.window) delete context.window.__ws; } catch (_) {}
}

/**
 * --trace-reads：记录 sdk 到底读了什么。
 *
 * 只挂【观测】，不改任何返回值 —— 这样量出来的才是它平常的行为。
 * ⚠ 观测本身会改变环境（多出 __rt 这个全局、Object.keys 被换掉），所以它默认关闭、
 *   只在调查时开。量完的结论要落进代码，别让调查开关留在生产路径上。
 */
function installReadTracer(context) {
  context.__rt = { keys: [], nav: Object.create(null), doc: Object.create(null), misc: Object.create(null) };
  vm.runInContext(`(function () {
    const R = __rt;
    const bump = (m, k) => { m[k] = (m[k] || 0) + 1; };
    const who = (o) => {
      if (o === globalThis || o === window) return 'window';
      if (o === navigator) return 'navigator';
      if (o === Object.getPrototypeOf(navigator)) return 'Navigator.prototype';
      if (o === document) return 'document';
      if (o === screen) return 'screen';
      try { return Object.prototype.toString.call(o); } catch (e) { return '?'; }
    };
    // ① 键枚举：这条直接回答"补 Object.keys 那 237 个，还是 getOwnPropertyNames 那 1232 个"
    for (const fname of ['keys', 'getOwnPropertyNames', 'getOwnPropertyDescriptor', 'entries', 'values']) {
      const orig = Object[fname];
      Object[fname] = function (o, ...rest) {
        try { R.keys.push(fname + '(' + who(o) + ')' + (fname === 'getOwnPropertyDescriptor' ? '.' + rest[0] : '')); } catch (e) {}
        return orig.apply(this, [o, ...rest]);
      };
    }
    const origOwnKeys = Reflect.ownKeys;
    Reflect.ownKeys = function (o) { try { R.keys.push('Reflect.ownKeys(' + who(o) + ')'); } catch (e) {} return origOwnKeys.call(this, o); };

    // ② navigator：把【原型上的 getter】包一层，记录每次取值
    try {
      const proto = Object.getPrototypeOf(navigator);
      for (const k of Object.getOwnPropertyNames(proto)) {
        const d = Object.getOwnPropertyDescriptor(proto, k);
        if (!d || !d.get || !d.configurable) continue;
        const g = d.get;
        Object.defineProperty(proto, k, {
          get() { bump(R.nav, k); return g.call(this); },
          enumerable: d.enumerable, configurable: true,
        });
      }
    } catch (e) { R.misc['nav-hook-failed:' + e.name] = 1; }

    // ③ document / screen / 其它关心的面
    try {
      const dproto = Object.getPrototypeOf(document);
      for (const k of Object.getOwnPropertyNames(dproto)) {
        const d = Object.getOwnPropertyDescriptor(dproto, k);
        if (!d || !d.get || !d.configurable) continue;
        const g = d.get;
        Object.defineProperty(dproto, k, {
          get() { bump(R.doc, k); return g.call(this); }, enumerable: d.enumerable, configurable: true,
        });
      }
    } catch (e) {}
    // iframe / contentWindow：决定要不要造独立 realm。
    // ⚠ 光数 contentWindow 被取几次【不够】—— 这个 iframe 是 sentinel 自己的工作 iframe
    //   （靠 postMessage 通信），取 contentWindow 多半只是为了调 postMessage。
    //   真正要区分的是：它从 contentWindow 上【取的是哪个属性】。
    //     只取 postMessage        → 跨 realm 那个洞没被碰，可以不补
    //     取 Function / navigator → 就是 CreepJS 那种跨 realm 探测，必须补
    //   所以这里给 contentWindow 套一层 Proxy，逐个属性记。
    try {
      const oCreate = document.createElement.bind(document);
      document.createElement = function (t) {
        bump(R.misc, 'createElement:' + String(t).toLowerCase());
        const el = oCreate(t);
        if (String(t).toLowerCase() === 'iframe' && el) {
          const d = Object.getOwnPropertyDescriptor(el, 'contentWindow');
          if (!d || d.configurable !== false) {
            const raw = el.contentWindow;
            const proxied = (raw && typeof raw === 'object') ? new Proxy(raw, {
              get(tg, prop, recv) {
                bump(R.misc, 'cw.get:' + String(prop));
                return Reflect.get(tg, prop, recv);
              },
              has(tg, prop) { bump(R.misc, 'cw.has:' + String(prop)); return Reflect.has(tg, prop); },
            }) : raw;
            Object.defineProperty(el, 'contentWindow', {
              get() { bump(R.misc, 'iframe.contentWindow'); return proxied; }, configurable: true,
            });
          }
        }
        return el;
      };
    } catch (e) { R.misc['iframe-hook-failed:' + e.name] = 1; }
  })()`, context, { filename: "read-tracer" });
}

/** 给 VM 打补丁：把 Function.prototype.toString 换成会查伪装表的版本。 */
function patchFunctionToString(context) {
  context.__nfs = NATIVE_FN_SRC;
  vm.runInContext(`(function () {
    const REG = __nfs;
    const real = Function.prototype.toString;
    const patched = function toString() {
      const hit = REG.get(this);
      return hit !== undefined ? hit : real.call(this);
    };
    // 替换者自身也必须报成原生，否则查它就穿帮
    REG.set(patched, 'function toString() { [native code] }');
    REG.set(real, 'function toString() { [native code] }');
    Object.defineProperty(Function.prototype, 'toString', {
      value: patched, writable: true, configurable: true, enumerable: false,
    });
  })()`, context, { filename: "native-tostring-patch" });
  // 立刻擦掉注入痕迹：留着它就是 window 上一个真浏览器没有的自有键
  try { delete context.__nfs; } catch (_) {}
  try { if (context.window) delete context.window.__nfs; } catch (_) {}
}

function createPluginArray(isSafari = false) {
  const makePlugin = (name) => ({
    name,
    filename: "internal-pdf-viewer",
    description: "Portable Document Format",
    length: 2,
    item(index) { return this[index] || null; },
    namedItem(type) { return this[type] || null; },
  });
  const pdf = { type: "application/pdf", suffixes: "pdf", description: "Portable Document Format", enabledPlugin: null };
  const textPdf = { type: "text/pdf", suffixes: "pdf", description: "Portable Document Format", enabledPlugin: null };
  const plugins = isSafari ? [
    makePlugin("WebKit built-in PDF"),
    makePlugin("PDF Viewer"),
  ] : [
    makePlugin("PDF Viewer"),
    makePlugin("Chrome PDF Viewer"),
    makePlugin("Chromium PDF Viewer"),
    makePlugin("Microsoft Edge PDF Viewer"),
    makePlugin("WebKit built-in PDF"),
  ];
  for (const plugin of plugins) {
    plugin[0] = pdf;
    plugin[1] = textPdf;
    plugin["application/pdf"] = pdf;
    plugin["text/pdf"] = textPdf;
  }
  pdf.enabledPlugin = plugins[0];
  textPdf.enabledPlugin = plugins[0];
  plugins.item = (index) => plugins[index] || null;
  plugins.namedItem = (name) => plugins.find((p) => p.name === name) || null;
  plugins.refresh = () => undefined;
  Object.defineProperty(plugins, Symbol.toStringTag, { value: "PluginArray" });
  return plugins;
}

function createMimeTypeArray() {
  const plugin = { name: "PDF Viewer", filename: "internal-pdf-viewer", description: "Portable Document Format" };
  const mimes = [
    { type: "application/pdf", suffixes: "pdf", description: "Portable Document Format", enabledPlugin: plugin },
    { type: "text/pdf", suffixes: "pdf", description: "Portable Document Format", enabledPlugin: plugin },
  ];
  mimes.item = (index) => mimes[index] || null;
  mimes.namedItem = (type) => mimes.find((m) => m.type === type) || null;
  Object.defineProperty(mimes, Symbol.toStringTag, { value: "MimeTypeArray" });
  return mimes;
}

function createCanvas(width = 300, height = 150, isSafari = false) {
  const canvas = {
    tagName: "CANVAS",
    style: {},
    width,
    height,
    parentNode: null,
    getBoundingClientRect() { return createDomRect(this.width, this.height); },
    toDataURL() { return "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lxv8dQAAAABJRU5ErkJggg=="; },
    getContext(type) {
      const name = String(type || "").toLowerCase();
      if (name === "2d") {
        return {
          canvas,
          fillStyle: "#000000",
          strokeStyle: "#000000",
          font: "10px sans-serif",
          fillRect() {}, clearRect() {}, strokeRect() {}, beginPath() {}, closePath() {}, moveTo() {}, lineTo() {}, stroke() {}, fill() {},
          fillText() {}, strokeText() {}, measureText(text) { return { width: String(text || "").length * 6.5 }; },
          getImageData() { return { data: new Uint8ClampedArray(canvas.width * canvas.height * 4), width: canvas.width, height: canvas.height }; },
          putImageData() {}, createImageData(w, h) { return { data: new Uint8ClampedArray(w * h * 4), width: w, height: h }; },
        };
      }
      if (name === "webgl" || name === "experimental-webgl" || name === "webgl2") {
        return {
          canvas,
          getParameter(param) {
            const values = new Map([
              [0x1f00, "WebKit"],                    // VENDOR
              [0x1f01, "WebKit WebGL"],              // RENDERER
              [0x1f02, isSafari ? "WebGL 2.0" : "WebGL 2.0 (OpenGL ES 3.0 Chromium)"],
              [0x8b8c, isSafari ? "WebGL GLSL ES 1.0" : "WebGL GLSL ES 3.00 (OpenGL ES GLSL ES 3.0 Chromium)"],
              [0x0d33, 16384],                       // MAX_TEXTURE_SIZE
              [0x8869, 16],                          // MAX_VERTEX_ATTRIBS
            ]);
            return values.has(param) ? values.get(param) : 0;
          },
          getExtension(name) {
            if (name === "WEBGL_debug_renderer_info") {
              return { UNMASKED_VENDOR_WEBGL: 0x9245, UNMASKED_RENDERER_WEBGL: 0x9246 };
            }
            return {};
          },
          getSupportedExtensions() { return ["ANGLE_instanced_arrays", "EXT_blend_minmax", "WEBGL_debug_renderer_info", "WEBGL_lose_context"]; },
          clearColor() {}, clear() {}, viewport() {}, createBuffer() { return {}; }, bindBuffer() {}, bufferData() {},
        };
      }
      return null;
    },
    addEventListener() {}, removeEventListener() {},
  };
  return canvas;
}

function createAudioContext() {
  return class AudioContextMock {
    constructor() { this.sampleRate = 48000; this.state = "running"; this.destination = {}; }
    createOscillator() { return { type: "sine", frequency: { value: 440 }, connect() {}, start() {}, stop() {} }; }
    createAnalyser() { return { fftSize: 2048, frequencyBinCount: 1024, getFloatFrequencyData() {}, getByteFrequencyData() {} }; }
    createGain() { return { gain: { value: 1 }, connect() {} }; }
    close() { this.state = "closed"; return Promise.resolve(); }
    resume() { this.state = "running"; return Promise.resolve(); }
    suspend() { this.state = "suspended"; return Promise.resolve(); }
  };
}

function createBrowserContext(options) {
  // ★★ 2026-08-16：人格判据必须在函数【最开头】算一次。
  //   原来只有下面 ~1050 行处有个 isSafari，而 performance.memory 在 ~780 行就建好了 ——
  //   const 不提升，早于声明处用不了，于是 memory 那块【无条件】按 Chromium 造，
  //   实测就出现过"UA 切了、载荷没切"的半切状态（p[2] 照样发 4395630592）。
  //   ⚠ 协议路现在只有 Windows 桌面 Chrome 一种人格（src/personaArm.js），
  //     下面这套 Safari 分支【当前走不到】，保留是因为：它是"人格判据要早算"这条
  //     教训的载体，而且 --user-agent 是外部可传的，别人拿 Safari UA 调 runner 时仍需正确。
  const isSafariUA = String(options.browserFamily || "").toLowerCase() === "safari"
    || /Version\/[^ ]+ Safari\//.test(String(options.userAgent || ""));
  const windowTarget = createEventTarget();
  const managedTimers = new Set();
  const managedSetTimeout = (callback, delay, ...args) => {
    const id = setTimeout(() => {
      managedTimers.delete(id);
      callback(...args);
    }, delay);
    managedTimers.add(id);
    return id;
  };
  const managedClearTimeout = (id) => {
    managedTimers.delete(id);
    clearTimeout(id);
  };
  const forcedRandomUUID = options.sentinelSid || "";
  let randomUUIDCalls = 0;
  const browserCrypto = Object.create(crypto.webcrypto);
  browserCrypto.randomUUID = () => {
    randomUUIDCalls += 1;
    // 当前 sdk.js 的第一次 UUID 调用用于 Sentinel 内部 sid。
    // 只固定这一次，避免后续 UUID 全部重复。
    if (forcedRandomUUID && randomUUIDCalls === 1) return forcedRandomUUID;
    return crypto.randomUUID();
  };
  browserCrypto.getRandomValues = crypto.webcrypto.getRandomValues.bind(crypto.webcrypto);

  const performanceObservers = new Set();
  const perfEntries = [{
    name: options.pageUrl,
    entryType: "navigation",
    startTime: 0,
    // 与 browserPerformance.now() 同一口径（见下方 pageAgeMs 的注释），
    // 否则导航条目的时长会和 performance.now() 打架。
    duration: Math.max(1, performance.now() + (Number(options.pageAgeMs) > 0 ? Number(options.pageAgeMs) : 0)),
    initiatorType: "navigation",
    nextHopProtocol: options.nextHopProtocol || "h2",
    transferSize: 0,
    encodedBodySize: 0,
    decodedBodySize: 0,
    toJSON() { return { ...this }; },
  }];
  function pushPerformanceEntry(entry) {
    perfEntries.push(entry);
    for (const observer of [...performanceObservers]) observer._notify?.(entry);
  }

  const browserIntl = createIntlObject(options);

  // ★★★ 页面年龄（2026-08-12 按线上真实 p 载荷校正）。
  //
  //   线上 p[13]（performance.now()）实测是 17156 与 28354 毫秒 —— 也就是
  //   「这个 iframe 已经开了 17~28 秒」，符合真人在验证码页上停留的时长。
  //   而我们的 VM 一起来就跑 SDK，p[13] 只有几百毫秒 ——
  //   等于告诉服务端「页面开了 0.8 秒就交了 sentinel 答卷」。阈值一条就判掉。
  //
  //   ⚠ 只把 now() 往后推是错的：p[17] timeOrigin + p[13] now 应当等于当前时刻，
  //     光推 now 会让这个和落到【未来】，比原来更可疑。
  //     所以正确做法是把 timeOrigin 一起【往前挪】同样的量 ——
  //     对外表现为「页面在 pageAgeMs 毫秒前打开」，而两者之和仍然等于真实当前时刻。
  //   ★ 同一次注册里的多次 sentinel/req 必须报【同一个 timeOrigin】——
  //     线上实测同一份抓包里的 3 份 p，[14] sid 与 [17] timeOrigin 完全相同，
  //     只有 [13] 和 [1] 在往前走（同一个 iframe 一直开着）。
  //     所以优先用调用方传进来的【绝对时刻】pageOpenAt，而不是相对的年龄 ——
  //     传年龄的话两次调用会各自算出一个 timeOrigin，服务端一 join 就露馅。
  const pageAgeMs = Number(options.pageOpenAt) > 0
    ? Math.max(0, Date.now() - Number(options.pageOpenAt))
    : Number(options.pageAgeMs) > 0
    ? Number(options.pageAgeMs)
    // ⚠ 这是【基数】，不是最终报出去的值：now() 返回 performance.now() + pageAgeMs，
    //   而 VM 从起来到 SDK 交卷还要跑 1~4 秒，那部分会叠加上去。
    //   所以基数取 15~25 秒，最终报出去的落在约 16~29 秒 —— 覆盖实测的 17.2 与 28.4。
    //   （线上只有 2 个样本，别过度拟合，量级对就行。）
    : 15000 + Math.random() * 10000;
  const realTimeOrigin = performance.timeOrigin || Date.now() - performance.now();
  // ★ 页面打开的【绝对时刻】。给了 pageOpenAt 就直接用它 —— 不能拿子进程自己的
  //   performance.timeOrigin 去反推，那样两次调用会因为子进程启动耗时不同而差十几毫秒，
  //   而线上实测同一会话的多份 p 报的是【逐位相同】的 timeOrigin。
  const pageOrigin = Number(options.pageOpenAt) > 0
    ? Number(options.pageOpenAt)
    : realTimeOrigin - pageAgeMs;
  // ★ 量化到 0.1 毫秒网格并带上一位小数。理由见下方 browserPerformance 的注释。
  //   偏移由 pageOrigin 自身派生（不是现随机）—— 同一个 sid 的多次调用必须拿到同一个值，
  //   线上实测同一会话的多份 p 报的 timeOrigin 是【逐位相同】的。
  const pageOriginQ = Math.round((pageOrigin + ((Math.abs(Math.trunc(pageOrigin)) % 10) / 10)) * 10) / 10;

  const browserPerformance = {
    // now() = 当前真实时刻 - 页面打开时刻。这样 timeOrigin + now() 恒等于当前时刻，
    // 不会落到未来，同时 now() 在 VM 生命周期内单调递增。
    //
    // ★★★ 2026-08-15：必须量化到 0.1 毫秒网格。
    //   实测（p[13] 槽）：浏览器 13/13 全是 0.1ms 的整数倍 —— 跨域 iframe 里
    //   performance.now() 的精度被 Chrome 主动粗化到 100 微秒，这是规范行为。
    //   我们 41/41 落在 1/4096 毫秒网格上，Fisher p=9.0e-13，两组零重叠。
    //   成因是浮点误差泄漏：realTimeOrigin 与 pageOrigin 都是 1.786e12 量级的 double，
    //   相减之后的 ulp 恰好是 2⁻¹² 毫秒 —— 这个"过高的精度"本身就是
    //   「我不是跨域 iframe 里的浏览器」的确定性证据，单请求、取模即判。
    //   ⚠ 别改成 Math.round(x)（整毫秒）—— 那会掉进另一个极端，
    //     浏览器是 0.1ms 网格，不是 1ms 网格。
    now: () => Math.round(((realTimeOrigin + performance.now()) - pageOriginQ) * 10) / 10,
    // ★★★ 同上：浏览器 11/13 的 timeOrigin 带一位小数（0.1ms 网格上均匀分布），
    //   我们 41/41 是整数毫秒（因为 pageOpenAt 由调用方传的是整数）。
    //   ⚠ now() 与 timeOrigin 必须用【同一个基准】pageOriginQ ——
    //     只给 timeOrigin 加小数、now() 还用整数基准的话，
    //     timeOrigin + now() 就不再等于当前时刻，那是个更容易判的矛盾。
    timeOrigin: pageOriginQ,
    // ★★ performance.memory 是 Chromium 专属，真 iPhone（WebKit）上【整个对象都不存在】。
    //   所以 Safari 人格下不能把它造出来 —— 造出来再填 0/null 也没用，
    //   sdk 探的是"有没有这个对象"，不是"值是多少"。
    ...(isSafariUA ? {} : {
      memory: {
        jsHeapSizeLimit: options.jsHeapSizeLimit,
        totalJSHeapSize: Math.floor(options.jsHeapSizeLimit / 3),
        usedJSHeapSize: Math.floor(options.jsHeapSizeLimit / 8),
      },
    }),
    getEntries() { return perfEntries.slice(); },
    getEntriesByType(type) { return perfEntries.filter((entry) => entry.entryType === String(type)); },
    getEntriesByName(name) { return perfEntries.filter((entry) => entry.name === String(name)); },
    mark(name) { pushPerformanceEntry({ name: String(name), entryType: "mark", startTime: this.now(), duration: 0, toJSON() { return { ...this }; } }); },
    measure(name) { pushPerformanceEntry({ name: String(name), entryType: "measure", startTime: this.now(), duration: 0, toJSON() { return { ...this }; } }); },
    clearMarks() {},
    clearMeasures() {},
  };
  const mathObject = Object.create(Math);
  if (Number.isFinite(options.fixedRandom)) {
    mathObject.random = () => options.fixedRandom;
  }
  const currentScript = { src: options.scriptSrc, length: options.scriptSrc.length };
  // ★★★ document.scripts 里【只有 sentinel SDK 自己】（2026-08-12 按线上真实 p 载荷校正）。
  //
  //   原来这里模拟的是【宿主页】的脚本清单：chatgpt 的 ssg.js、Google GSI、Stripe、CF jsd。
  //   但 SDK 是跑在 sentinel.openai.com 自己的跨域 iframe 里的 —— 那个 document 里
  //   物理上不可能看得见宿主页的脚本，跨域隔离就是干这个的。
  //
  //   线上真实载荷的铁证（logs/registration-flow/…b49fa5，email-otp/validate）：
  //     p[5] = "https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js"
  //     p[6] = null
  //   而我们发的是 p[5]="https://chatgpt.com/c/1/_/ssg.js"、p[6]="c/1/_" —— 两个都不可能出现。
  //
  //   顺带：那个 "c/prod-fb4a8a2a…" 的兜底常量是全账号共用的写死值，
  //   留着它本身就是一把聚类钥匙。一并去掉。
  const scripts = [currentScript];
  const attrs = new Map();
  if (options.buildId) attrs.set("data-build", options.buildId);
  // Stripe 的 iframe 全局键名，每次随机（真 Stripe 就是随机的）。
  const stripeFrameKey = `__privateStripeFrame${8000 + Math.floor(Math.random() * 2000)}`;
  // ★★★ 2026-08-15：字符集必须是 base36，不是 hex。
  //   实测：线上 9 份注入键 0/9 是纯 hex，全部是 base36（含 g~z）、长度 10~11；
  //   我们 17/17 是纯 hex、长度恒 11。Fisher p=3.2e-07。
  //   成因是 React 用 Math.random().toString(36).slice(2) 生成这个后缀，
  //   而我们用了 crypto.randomBytes(6).toString("hex") —— 后者【永远不含 g~z】，
  //   单看一个键名就能判定，不需要任何聚类。
  //   ⚠ src/sentinelFingerprint.js（RT 链路）用的已经是 base36，两条链路口径本来就该一致；
  //     不统一等于把我们自己的号劈成两个可区分总体。
  const react36 = () => Math.random().toString(36).slice(2, 13);
  const reactListeningKey = options.reactListeningKey || "_reactListening" + react36();
  const reactContainerKey = options.reactContainerKey || "__reactContainer$" + react36();
  const reactResourcesKey = options.reactResourcesKey || reactContainerKey.replace("__reactContainer$", "__reactResources$");

  const cookieJar = createCookieJar(options.cookie);
  const location = new URL(options.pageUrl);
  let iframeNode = null;
  const bodyChildren = [];
  const documentTarget = createEventTarget();
  // ★★ 2026-08-15：document 改成【原型 + 极少数自有属性】的形态。
  //
  //   起因：sentinel 的 p[11] 槽是「从 document 的键里随机抽一个」。实测比对
  //   （55 份线上载荷 vs 我们 18 次运行）：
  //       线上抽到 React 注入键(__reactContainer$… / _reactListening…) 的比例 = 33%
  //       我们                                                            = 0/18
  //   若真按 33% 抽，连抽 18 次一个不中的概率是 0.67^18 ≈ 0.09%。
  //
  //   根因不是"我们没挂 React 键"——早就挂了（下面那三个）。是【比例】不对：
  //   真 Chrome 里 `Object.keys(document).length === 1`（只有 location），
  //   标准属性 URL/cookie/querySelector… 全都在 Document.prototype 上；
  //   而我们写成一个普通对象字面量，31 个属性【全是自有的】，
  //   React 只占 3/31 = 9.7%，被稀释了 3.4 倍。
  //   （本机真 Chrome 实测：Object.keys(document)=1、for..in=298、
  //     Object.keys(window)=223 —— 见 scratchpad/domkeys.js）
  //
  //   这与 navigator-surface.js 当年修的是【同一个 bug】，那个文件的注释里
  //   已经写明「Object.keys(navigator) 在真浏览器里是空数组，在我们这里却列出
  //   一堆自有属性」——当时只修了 navigator，document 漏掉了。
  //
  //   ⚠ 别把 React 那三个键也挪上原型：它们在真页面里【就是】自有属性
  //     （React 直接往 document 对象上挂），挪了等于把这次修复反着做一遍。
  const documentProto = {
    currentScript,
    scripts,
    get cookie() { return cookieJar.cookie; },
    set cookie(value) { cookieJar.cookie = value; },
    URL: options.pageUrl,
    documentURI: options.pageUrl,
    referrer: options.referrer || "https://auth.openai.com/",
    title: "",
    origin: location.origin,
    // ★ location 不在原型上 —— 它是 document 的【自有】属性
    //   （真 Chrome 实测 Object.keys(document) === ['location']）。
    //   赋值见下方 Object.create 之后。
    characterSet: "UTF-8",
    charset: "UTF-8",
    compatMode: "CSS1Compat",
    contentType: "text/html",
    readyState: "complete",
    visibilityState: "visible",
    hidden: false,
    hasFocus() { return true; },
    // ★ React 那三个键【不在这里】—— 它们必须是 document 的自有属性，
    //   见下方 Object.create 之后的赋值。
    defaultView: null,
    head: null,
    documentElement: {
      nodeType: 1,
      nodeName: "HTML",
      tagName: "HTML",
      ownerDocument: null,
      style: createStyleDeclaration(),
      clientWidth: options.screen.width,
      clientHeight: options.screen.height,
      scrollWidth: options.screen.width,
      scrollHeight: options.screen.height,
      getAttribute(name) {
        return attrs.get(name) ?? null;
      },
      setAttribute(name, value) {
        attrs.set(name, String(value));
      },
      querySelector() { return null; },
      querySelectorAll() { return []; },
      getBoundingClientRect() {
        return createDomRect(options.screen.width, options.screen.height);
      },
    },
    body: {
      nodeType: 1,
      nodeName: "BODY",
      tagName: "BODY",
      ownerDocument: null,
      parentNode: null,
      parentElement: null,
      children: bodyChildren,
      childNodes: bodyChildren,
      style: createStyleDeclaration(),
      clientWidth: options.screen.width,
      clientHeight: options.screen.height,
      getBoundingClientRect() {
        return createDomRect(options.screen.width, options.screen.height);
      },
      appendChild(node) {
        bodyChildren.push(node);
        node.parentNode = document.body;
        if (node?.tagName === "IFRAME") iframeNode = node;
        managedSetTimeout(() => node?._emitLoad?.(), 0);
        return node;
      },
      removeChild(node) {
        const index = bodyChildren.indexOf(node);
        if (index >= 0) bodyChildren.splice(index, 1);
        if (iframeNode === node) iframeNode = null;
        if (node) node.parentNode = null;
        return node;
      },
    },
    addEventListener: documentTarget.addEventListener,
    removeEventListener: documentTarget.removeEventListener,
    dispatchEvent: documentTarget.dispatchEvent,
    querySelector(selector) {
      const q = String(selector || "").toLowerCase();
      if (q === "head") return this.head;
      if (q === "body") return this.body;
      if (q === "html" || q === "documentelement") return this.documentElement;
      return null;
    },
    querySelectorAll(selector) { const item = this.querySelector(selector); return item ? [item] : []; },
    getElementById() { return null; },
    getElementsByTagName(name) {
      const n = String(name).toLowerCase();
      if (n === "script") return scripts;
      if (n === "head") return [this.head];
      if (n === "body") return [this.body];
      if (n === "html") return [this.documentElement];
      return [];
    },
    createTextNode(text) { return { nodeType: 3, nodeName: "#text", textContent: String(text || ""), parentNode: null, ownerDocument: document }; },
    createElement(tagName) {
      const lowerTag = String(tagName).toLowerCase();
      if (lowerTag === "canvas") {
        const canvas = createCanvas(300, 150, isSafari);
        canvas.ownerDocument = document;
        return canvas;
      }
      if (lowerTag !== "iframe") {
        return createElementNode(tagName, document);
      }

      const target = createEventTarget();
      const iframe = createElementNode("iframe", document);
      Object.assign(iframe, {
        src: "",
        width: "",
        height: "",
        sandbox: { value: "", toString() { return this.value; } },
        getBoundingClientRect() {
          return createDomRect();
        },
        contentWindow: {
          postMessage(message, origin) {
            Promise.resolve()
              .then(async () => {
                const result = await options.handleIframeMessage(message);
                windowTarget.dispatchEvent({
                  type: "message",
                  source: iframe.contentWindow,
                  origin,
                  data: {
                    type: "response",
                    requestId: message.requestId,
                    result,
                  },
                });
              })
              .catch((error) => {
                windowTarget.dispatchEvent({
                  type: "message",
                  source: iframe.contentWindow,
                  origin,
                  data: {
                    type: "response",
                    requestId: message.requestId,
                    error: error?.message || String(error),
                  },
                });
              });
          },
        },
        addEventListener: target.addEventListener,
        removeEventListener: target.removeEventListener,
        dispatchEvent: target.dispatchEvent,
        _emitLoad() {
          target.dispatchEvent.call(iframe, { type: "load", target: iframe });
        },
      });
      return iframe;
    },
  };

  // ★ 真 Chrome 的 Object.keys(document) 只有 ['location']，其余全在原型上。
  //   所以这里自有属性只留 location + React 挂的那三个，其余一律走原型。
  //   （location 在真页面里确实是自有的 —— 实测 Object.keys(document)===['location']）
  const document = Object.create(documentProto);
  // ★ 自有属性的成分按【纯浏览器抓包】校准（logs/registration-flow，20 份载荷的 p[11]）：
  //     注入键 _reactListening / __reactContainer$   13/20 = 65%
  //     location                                      7/20 = 35%
  //     其它非注入键                                  0/20 = 0%
  //     __reactResources$                             0/20 ← 线上从不出现，不挂
  //   即真页面的 Object.keys(document) ≈ ['location'] + React 挂的那两个 ——
  //   与本机真 Chrome 实测 Object.keys(document)===['location'] 完全吻合。
  //
  //   ⚠⚠ 校准过程踩了两个坑，都记在这，别再犯：
  //     ① 第一版留 location + 三个 React 键 → 实测 83%，比原来的 0% 离基线【更远】。
  //        矫枉过正同样是可检测的异常。
  //     ② 第二版加了 origin 想压到 33% —— 而那个 33% 是【假基线】：
  //        当时扫的是 logs 全目录，把我们【自己】的 protocol-flow 抓包
  //        （34 份、注入键仅 15%）混了进去，把浏览器的 65% 稀释成 33%。
  //        以后算任何线上基线，只许扫 logs/registration-flow/。
  document.location = location;
  document[reactListeningKey] = true;
  document[reactContainerKey] = true;

  // ⚠ 下面这几个赋值【必须打在原型上】。写成 document.xxx = 会当场变成自有属性，
  //   把上面刚调好的比例又冲掉（这正是第一版踩的坑：31 个自有属性稀释了 React 键）。
  documentProto.defaultView = null;
  documentProto.documentElement.ownerDocument = document;
  documentProto.body.ownerDocument = document;
  documentProto.head = createElementNode("head", document);

  const browserFamily = String(options.browserFamily || "chrome").toLowerCase();
  const isSafari = browserFamily === "safari" || /Version\/[^ ]+ Safari\//.test(String(options.userAgent || ""));
  const exposeRequestIdleCallback = !isSafari && options.requestIdleCallback !== false;

  // ★ 是不是移动画像。以 sec-ch-ua-mobile / UA 里的 iPhone 为准 ——
  //   这两个是我们在 HTTP 层已经发出去的事实，navigator 必须与它们一致。
  const isMobileProfile = String(options.secChUaMobile || "") === "?1"
    || /iPhone|Android|Mobile/.test(String(options.userAgent || ""));
  // sec-ch-ua-platform 传进来是带引号的（"iPhone"），userAgentData.platform 不带引号
  const uaDataPlatform = String(
    options.userAgentDataPlatform || options.secChUaPlatform || (isMobileProfile ? "iPhone" : "macOS")
  ).replace(/^"|"$/g, "");
  const navigatorProto = isSafari ? {
    javaEnabled: makeNativeFunction("javaEnabled", () => false),
    sendBeacon: makeNativeFunction("sendBeacon", () => true),
    getGamepads: makeNativeFunction("getGamepads", () => []),
    webkitGetUserMedia: makeNativeFunction("webkitGetUserMedia"),
  } : null;   // Chrome 分支在下面用真实的 Navigator.prototype 表面构造

  // navigator 上所有【数据类】取值。在真 Chromium 里它们是 prototype 上的 getter，
  // 不是实例的自有属性 —— 见 sentinel/navigator-surface.js 的文件头注释。
  const navigatorValues = ({
    userAgent: options.userAgent,
    language: options.language,
    languages: options.languages,
    // ★★★ 2026-08-16：这五个 legacy 常量原来【没配值】。
    //   navigator-surface.js:100 的分支是「values 里没有这个 key → get = () => undefined」，
    //   于是键名在原型上（能被抽到），取值却是 undefined。
    //   为什么这是硬伤：sentinel 的 p[10] 抽的是【键−值】对
    //   （实测抽到过 "windowControlsOverlay−[object WindowControlsOverlay]"、
    //    "vibrate−function vibrate() { [native code] }"）——
    //   一旦抽中这几个键，我们发出去的是 "appName−undefined"，
    //   而【地球上每个浏览器】都是 "appName−Netscape"。单个样本即可判死，不需要聚类。
    //   ⚠ 这五个值在所有浏览器上都恒定，不是可随机项，别加随机。
    //     实测本机真 Chrome 150：Mozilla / Netscape / Gecko / 20030107 / ""（空串）
    appCodeName: "Mozilla",
    appName: "Netscape",
    product: "Gecko",
    productSub: "20030107",
    vendorSub: "",
    // appVersion 是 UA 去掉开头的 "Mozilla/"，跟着 UA 走，不能写死
    appVersion: String(options.userAgent || "").replace(/^Mozilla\//, ""),
    cookieEnabled: true,
    onLine: true,
    pdfViewerEnabled: true,
    doNotTrack: null,
    plugins: createPluginArray(isSafari),
    mimeTypes: createMimeTypeArray(),
    hardwareConcurrency: options.hardwareConcurrency,
    ...(isSafari ? {} : { deviceMemory: options.deviceMemory }),
    // ★ 移动画像必须成组自洽：头里写着 sec-ch-ua-mobile: ?1 / platform: "iPhone"，
    //   navigator 这边却报 maxTouchPoints=0 + MacIntel，SDK 一问就自相矛盾。
    maxTouchPoints: options.maxTouchPoints != null ? Number(options.maxTouchPoints) : (isMobileProfile ? 5 : 0),
    platform: options.navigatorPlatform || (isMobileProfile ? "iPhone" : "MacIntel"),
    vendor: options.navigatorVendor || (isSafari ? "Apple Computer, Inc." : "Google Inc."),
    webdriver: false,
    // ★ 2026-08-16：这两个原来是无条件的，但都是 Chromium 专属：
    //   · navigator.bluetooth —— Web Bluetooth，Safari 不实现
    //   · navigator.connection —— Network Information API，Safari 不实现
    //   留着就是「我是 Safari 但我有 Chrome 的 API」，和 performance.memory 同一类破绽。
    ...(isSafari ? {} : { bluetooth: { toString: () => "[object Bluetooth]" } }),
    ...(isSafari ? {} : { gpu: { toString: () => "[object GPU]" } }),
    ...(isSafari ? {} : { connection: createNetworkInformation() }),
    permissions: { query: async () => ({ state: "prompt", onchange: null }) },
    geolocation: {
      getCurrentPosition(success, error) { if (typeof error === "function") error({ code: 1, message: "User denied Geolocation" }); },
      watchPosition() { return 1; },
      clearWatch() {},
    },
    mediaDevices: {
      enumerateDevices: async () => [],
      getUserMedia: async () => { throw new Error("Permission denied"); },
    },
    storage: { estimate: async () => ({ quota: 10737418240, usage: 0 }) },
    ...(isSafari ? {} : {
      login: { toString: () => "[object NavigatorLogin]" },
      userAgentData: {
        mobile: isMobileProfile,
        platform: uaDataPlatform,
        brands: parseSecChBrands(options.secChUa, options.chromeMajor),
        getHighEntropyValues: async (hints = []) => {
          const values = {
            architecture: options.secChUaArch || "arm",
            bitness: options.secChUaBitness || "64",
            mobile: isMobileProfile,
            model: options.secChUaModel || "",
            platform: uaDataPlatform,
            platformVersion: options.secChUaPlatformVersion || (isMobileProfile ? "26.0.0" : "15.7.0"),
            uaFullVersion: options.chromeFullVersion || "",
            fullVersionList: parseSecChBrands(options.secChUaFullVersionList, options.chromeFullVersion || options.chromeMajor),
          };
          if (!Array.isArray(hints) || hints.length === 0) return values;
          const picked = {};
          for (const hint of hints) if (hint in values) picked[hint] = values[hint];
          return picked;
        },
        toJSON() { return { brands: this.brands, mobile: this.mobile, platform: this.platform }; },
      },
    }),
  });

  // ★★★ 2026-08-12：把数据属性搬到【原型上的 getter】，并把原型扩到真 Chromium 的表面。
  //
  //   起因是线上 p[10]（从 navigator 原型随机抽一个成员）实测抽到的是
  //     "webkitPersistentStorage−[object DeprecatedStorageQuota]" 与 "hardwareConcurrency−4"
  //   —— 两个都是数据属性。而我们原来的原型只有 9 个成员、全是函数，
  //   抽出来永远是 "getBattery−function getBattery() { [native code] }" 这类，与线上零重合；
  //   而且 9 个取值意味着每个值被约 11% 的账号共用（真 Chromium 60+ ⇒ 1.6%）。
  //
  //   顺带修掉两处形态差异：真浏览器里 Object.keys(navigator) 是空数组
  //   （全在原型上），我们原来用 Object.assign 挂在实例上，会列出一堆自有属性。
  const NAV_METHOD_IMPLS = {
    createAuctionNonce: () => crypto.randomUUID(),
    canLoadAdAuctionFencedFrame: () => false,
    getBattery: () => Promise.resolve({ charging: true, chargingTime: 0, dischargingTime: Infinity, level: 1 }),
    getGamepads: () => [],
    javaEnabled: () => false,
    sendBeacon: () => true,
    vibrate: () => false,
    canShare: () => false,
  };
  const navigator = isSafari
    ? Object.assign(Object.create(navigatorProto), navigatorValues)
    : Object.create(createNavigatorPrototype(
        { ...navigatorValues, __methods: NAV_METHOD_IMPLS },
        makeNativeFunction,
      ));

  const localStorage = createStorage();
  const sessionStorage = createStorage();
  const history = {
    length: 1,
    state: null,
    back() {},
    forward() {},
    go() {},
    pushState(state) {
      this.state = state ?? null;
    },
    replaceState(state) {
      this.state = state ?? null;
    },
  };

  async function browserFetch(input, init = {}) {
    const url = typeof input === "string" ? input : (input?.url || String(input));
    const start = browserPerformance.now();
    const isSentinelPing = /\/backend-api\/sentinel\/ping(?:$|[?#])/.test(url);
    if (isSentinelPing) {
      const edge = String(options.cfEdgeMsec ?? 38);
      const origin = String(options.cfOriginTtfbMsec ?? 74);
      const tcp = String(options.cfTcpRttMsec ?? 22);
      const quic = String(options.cfQuicRttMsec ?? 0);
      const duration = Math.max(1, Number(edge) + Number(origin));
      const entry = {
        name: url,
        entryType: "resource",
        initiatorType: "fetch",
        startTime: start,
        requestStart: start + 1,
        responseStart: start + Math.max(1, Number(edge)),
        responseEnd: start + duration,
        duration,
        transferSize: 300,
        encodedBodySize: 0,
        decodedBodySize: 0,
        nextHopProtocol: options.nextHopProtocol || "h2",
        toJSON() { return { ...this }; },
      };
      pushPerformanceEntry(entry);
      return new Response("", {
        status: 204,
        headers: {
          "s-cf-edge-msec": edge,
          "s-cf-origin-ttfb-msec": origin,
          "s-cf-tcp-rtt-msec": tcp,
          "s-cf-quic-rtt-msec": quic,
        },
      });
    }
    const response = await fetch(input, init);
    const end = browserPerformance.now();
    pushPerformanceEntry({
      name: url,
      entryType: "resource",
      initiatorType: init?.method ? String(init.method).toLowerCase() : "fetch",
      startTime: start,
      requestStart: start + 1,
      responseStart: Math.max(start + 1, end - 1),
      responseEnd: end,
      duration: Math.max(1, end - start),
      transferSize: 0,
      encodedBodySize: 0,
      decodedBodySize: 0,
      nextHopProtocol: options.nextHopProtocol || "h2",
      toJSON() { return { ...this }; },
    });
    return response;
  }

  const window = Object.assign(windowTarget, {
    window: null,
    self: null,
    top: null,
    parent: null,
    name: "",
    closed: false,
    length: 0,
    opener: null,
    frames: null,
    focus() {},
    blur() {},
    scrollTo() {},
    scrollBy() {},
    matchMedia(query) { return { matches: false, media: String(query), onchange: null, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() { return false; } }; },
    getComputedStyle(element) { return element?.style || createStyleDeclaration(); },
    MessageEvent: class MessageEvent { constructor(type, init = {}) { this.type = type; Object.assign(this, init); } },
    Event: class Event { constructor(type, init = {}) { this.type = type; Object.assign(this, init); } },
    CustomEvent: class CustomEvent { constructor(type, init = {}) { this.type = type; this.detail = init.detail; Object.assign(this, init); } },
    DOMRect: class DOMRect { constructor(x = 0, y = 0, width = 0, height = 0) { Object.assign(this, { x, y, width, height, top: y, left: x, right: x + width, bottom: y + height }); } },
    HTMLElement: function HTMLElement() {},
    HTMLIFrameElement: function HTMLIFrameElement() {},
    MutationObserver: class MutationObserver { constructor(callback) { this.callback = callback; } observe() {} disconnect() {} takeRecords() { return []; } },
    PerformanceObserver: createPerformanceObserver(performanceObservers),
    document,
    navigator,
    screen: options.screen,
    location,
    locationbar: { visible: true },
    menubar: { visible: true },
    personalbar: { visible: true },
    scrollbars: { visible: true },
    statusbar: { visible: true },
    toolbar: { visible: true },
    scrollX: 0,
    scrollY: 0,
    localStorage,
    sessionStorage,
    history,
    innerWidth: options.screen.width,
    innerHeight: options.screen.height,
    outerWidth: options.screen.width,
    outerHeight: options.screen.height + 88,
    devicePixelRatio: options.devicePixelRatio,
    ...(isSafari ? { safari: { pushNotification: {} } } : { chrome: { runtime: {}, app: {} } }),
    performance: browserPerformance,
    crypto: browserCrypto,
    TextEncoder,
    TextDecoder,
    URL,
    URLSearchParams,
    AbortController,
    setTimeout: managedSetTimeout,
    clearTimeout: managedClearTimeout,
    setInterval: managedSetTimeout,
    clearInterval: managedClearTimeout,
    queueMicrotask: queueMicrotask.bind(globalThis),
    btoa: btoaBinary,
    atob: atobBinary,
    fetch: browserFetch,
    console,
    Math: mathObject,
    Date,
    Intl: browserIntl,
    AudioContext: createAudioContext(),
    webkitAudioContext: createAudioContext(),
    JSON,
    Array,
    Object,
    Reflect,
    Number,
    String,
    Promise,
    RegExp,
    Error,
    Map,
    Set,
    WeakMap,
    Uint8Array,
    encodeURIComponent,
    decodeURIComponent,
    unescape,
    ...(exposeRequestIdleCallback ? {
      requestIdleCallback(callback) {
        return managedSetTimeout(() => callback({ timeRemaining: () => 5, didTimeout: false }), 0);
      },
      cancelIdleCallback(id) {
        managedClearTimeout(id);
      },
    } : {}),
    requestAnimationFrame(callback) {
      return managedSetTimeout(() => callback(performance.now()), 16);
    },
    cancelAnimationFrame(id) {
      managedClearTimeout(id);
    },
    webkitRequestAnimationFrame(callback) {
      return this.requestAnimationFrame(callback);
    },
    // ★ 帧号原来写死成 8094 —— Stripe 每次是随机的，写死意味着所有账号共用同一个值，
    //   单个号看不出来，按 window 键聚类立刻成一条直线（与 navigator 池当年那个问题同类）。
    //   ⚠ 键名是动态的，下面那处「二次导出到沙箱」必须用同一个变量，
    //     写死旧名会导出成 undefined —— 那比写死更糟。
    [stripeFrameKey]: {},
    // ★★ 2026-08-15 补：应用注入的三个全局。
    //   实测 logs/registration-flow 的 20 份线上载荷，p[12] 抽到应用注入键 3 次（15%）：
    //     $RC（React 18 流式渲染）/ __oai_so_lx / __oai_so_sx0（OpenAI 自己的 session observer）
    //   我们此前 0 个 —— 而我们【明明在发 openai-sentinel-so-token】，
    //   声称跑了 SO 却在 window 上找不到 SO 的痕迹，是自相矛盾。
    //   ⚠ 不要为了把命中率"调"到 15% 而多造几个：浏览器真实 window 有 223 个自有键，
    //     3 个注入键的期望命中率本来就只有 1.3%，那 15% 是 3/20 的小样本噪声
    //     （95% 区间约 3%~38%）。照真实成分补，别照观测频率凑。
    $RC: {},
    __oai_so_lx: {},
    __oai_so_sx0: {},
    onpageswap: null,
    ondevicemotion: null,
    onpagehide: null,
    onpageshow: null,
    onvisibilitychange: null,
    onfocus: null,
    onblur: null,

    // ★★★ 2026-08-16：补齐真浏览器有、我们缺的标准全局。
    //
    //   【怎么发现的】把同一份指纹面枚举脚本在【本机真 Chrome 150】和本 VM 里各跑一次逐项 diff，
    //   实测这批在真 Chrome 里全是 true、在我们这里全是 false。
    //
    //   【为什么必须补】sentinel 的 p[11]/p[12] 抽的是 window 的键。少掉一批人人都有的
    //   标准全局，抽样池本身就是错的 —— 这比"抽到的值恒定"更硬，属性缺失没有良性解释。
    //   实测补之前 window 自有键只有 134 个，真 Chrome 是两百多。
    //
    //   ⚠ 这里只补【存在性与形态】，不实现功能：sdk 实测从不调用它们（--debug-fp 量过
    //     canvas/WebGL/audio 读取次数为 0）。造一个能跑的 Worker 反而会引入新的行为差异。
    //   ⚠ 全部走 makeNativeFunction / toStringTag，保证 Function.prototype.toString 查它们
    //     时返回 [native code] —— 否则补了存在性却漏了源码，等于换一个破绽。
    ...(isSafari ? {} : {
      // 这几个 Safari 也有，但 Safari 分支的取舍单独处理，先只保证 Chrome 人格正确
      WebAssembly: (() => { const o = { compile: makeNativeFunction("compile"), instantiate: makeNativeFunction("instantiate"), validate: makeNativeFunction("validate") }; Object.defineProperty(o, Symbol.toStringTag, { value: "WebAssembly" }); return o; })(),
      caches: (() => { const o = { open: makeNativeFunction("open"), match: makeNativeFunction("match"), keys: makeNativeFunction("keys"), has: makeNativeFunction("has"), delete: makeNativeFunction("delete") }; Object.defineProperty(o, Symbol.toStringTag, { value: "CacheStorage" }); return o; })(),
      indexedDB: (() => { const o = { open: makeNativeFunction("open"), deleteDatabase: makeNativeFunction("deleteDatabase"), databases: makeNativeFunction("databases"), cmp: makeNativeFunction("cmp") }; Object.defineProperty(o, Symbol.toStringTag, { value: "IDBFactory" }); return o; })(),
      speechSynthesis: (() => { const o = { speak: makeNativeFunction("speak"), cancel: makeNativeFunction("cancel"), getVoices: makeNativeFunction("getVoices", () => []), pending: false, speaking: false, paused: false }; Object.defineProperty(o, Symbol.toStringTag, { value: "SpeechSynthesis" }); return o; })(),
      Worker: makeNativeFunction("Worker"),
      SharedWorker: makeNativeFunction("SharedWorker"),
      BroadcastChannel: makeNativeFunction("BroadcastChannel"),
      RTCPeerConnection: makeNativeFunction("RTCPeerConnection"),
      webkitRTCPeerConnection: makeNativeFunction("webkitRTCPeerConnection"),
      OffscreenCanvas: makeNativeFunction("OffscreenCanvas"),
      FontFace: makeNativeFunction("FontFace"),
      Image: makeNativeFunction("Image"),
      ResizeObserver: makeNativeFunction("ResizeObserver"),
      IntersectionObserver: makeNativeFunction("IntersectionObserver"),
      ReportingObserver: makeNativeFunction("ReportingObserver"),
      Notification: (() => { const f = makeNativeFunction("Notification"); Object.defineProperty(f, "permission", { value: "default", enumerable: true }); Object.defineProperty(f, "requestPermission", { value: makeNativeFunction("requestPermission"), enumerable: true }); return f; })(),
      // HTMLCanvasElement 必须有：FP 库会 Function.prototype.toString.call(
      //   HTMLCanvasElement.prototype.toDataURL) 检查有没有被 hook。
      //   我们缺这个类时那句直接抛 TypeError —— 抛异常本身就是回答。
      HTMLCanvasElement: (() => {
        const f = makeNativeFunction("HTMLCanvasElement");
        f.prototype = { toDataURL: makeNativeFunction("toDataURL"), getContext: makeNativeFunction("getContext"), toBlob: makeNativeFunction("toBlob") };
        Object.defineProperty(f.prototype, Symbol.toStringTag, { value: "HTMLCanvasElement" });
        return f;
      })(),
    }),
  });

  window.window = window;
  window.self = window;
  window.top = window;
  window.parent = window;
  window.frames = window;
  // ★ 同上：打在原型上，别把它变成 document 的自有属性。
  documentProto.defaultView = window;

  return {
    iframeNode: () => iframeNode,
    context: vm.createContext({
      window,
      self: window,
      globalThis: window,
      document,
      navigator,
      screen: options.screen,
      location,
      localStorage,
      sessionStorage,
      history,
      performance: browserPerformance,
      crypto: browserCrypto,
      TextEncoder,
      TextDecoder,
      URL,
      URLSearchParams,
      AbortController,
      MessageEvent: window.MessageEvent,
      Event: window.Event,
      CustomEvent: window.CustomEvent,
      DOMRect: window.DOMRect,
      HTMLElement: window.HTMLElement,
      HTMLIFrameElement: window.HTMLIFrameElement,
      MutationObserver: window.MutationObserver,
      PerformanceObserver: window.PerformanceObserver,
      setTimeout: managedSetTimeout,
      clearTimeout: managedClearTimeout,
      setInterval: managedSetTimeout,
      clearInterval: managedClearTimeout,
      queueMicrotask: queueMicrotask.bind(globalThis),
      btoa: btoaBinary,
      atob: atobBinary,
      fetch: browserFetch,
      console,
      Math: mathObject,
      Date,
      Intl: browserIntl,
      AudioContext: window.AudioContext,
      webkitAudioContext: window.webkitAudioContext,
      JSON,
      Array,
      Object,
      Reflect,
      Number,
      String,
      Promise,
      RegExp,
      Error,
      Map,
      Set,
      WeakMap,
      Uint8Array,
      encodeURIComponent,
      decodeURIComponent,
      unescape,
      ...(exposeRequestIdleCallback ? {
        requestIdleCallback: window.requestIdleCallback,
        cancelIdleCallback: window.cancelIdleCallback,
      } : {}),
      requestAnimationFrame: window.requestAnimationFrame,
      cancelAnimationFrame: window.cancelAnimationFrame,
      webkitRequestAnimationFrame: window.webkitRequestAnimationFrame,
      [stripeFrameKey]: window[stripeFrameKey],
      $RC: window.$RC,
      __oai_so_lx: window.__oai_so_lx,
      __oai_so_sx0: window.__oai_so_sx0,
      onpageswap: window.onpageswap,
    }),
    clearTimers() {
      for (const id of [...managedTimers]) managedClearTimeout(id);
    },
  };
}

/**
 * 在停留期间往假页面上派发交互事件。
 *
 * ★★★ 2026-08-15：事件清单不再是猜的 —— 是从 collector_dx 里【读出来】的。
 *
 *   做法：向 sentinel 取一次 challenge，challenge.so.collector_dx 是密文，
 *   密钥就是【这次请求发上去的整个 p 串】（含 gAAAAA 前缀）。异或解开后是一段
 *   268 条指令的字节码程序，格式 [操作码, 目标寄存器, 操作数]。
 *   （脚本见 scratchpad/dxkey.js。此前看不到，是因为 authFlowCapture 把响应体截断在
 *    20000 字，而 registration-flow 那个采集器根本不记响应体。）
 *
 *   程序里 addEventListener 只注册这 6 类，【没有别的】：
 *       pointermove  scroll  wheel  keydown  click  paste
 *   上一版广撒网派发的 mousemove / touchstart / focus / visibilitychange
 *   采集器一个都不听 —— 所以 7 个行为槽当时只动了 1 个。
 *
 *   它把状态存在 34 个 window 全局上（都在程序的字符串常量里）：
 *       __oai_so_t0                起始时刻
 *       __oai_so_p / _pc           指针累计量 / 次数
 *       __oai_so_lx / _ly          指针最后坐标（配合 Math.hypot 算移动距离）
 *       __oai_so_k / _kp           按键计数 / paste
 *       __oai_so_s / _st / _sw     滚动；_sx0 / _sy0 滚动起点
 *       __oai_so_we / _wb / _wl    滚轮
 *       __oai_so_h/_hi/_hp/_hw/_ht/_hc   直方图
 *       __oai_so_bc / _bm          点击
 *       _ss/_ss2/_sn、_cs/_cs2/_cn、_fs/_fs2/_fn   和 / 平方和 / 个数（即在算方差）
 *   —— 有「和 / 平方和 / 个数」三件套，说明它不只数次数，还看【分布形态】，
 *   所以事件的数值必须像真人，不是随便非零就行。
 *
 * ⚠ 仍未知：34 个全局与 so 的 7 个行为槽如何对应、各槽的期望量级。
 *   浏览器实测：槽1 是 7~23 的小整数、槽4 是 2~4 的小数、槽10/13 是 2~5 万。
 *   本版先把【事件类型】修对；量级要等 268 条指令反汇编完才谈得上照规格填。
 */
// ★★ 实测结论（2026-08-15）：采集器的监听【全部挂在 window 上】。
//   只往 document 派发时 7 个行为槽全是 0；只往 window 派发时 7/7 非零。
//   所以默认只发 window ——【绝不能两边都发】，那会让每个事件被算两次：
//   实测双发时槽1/11/12 分别是 38~45 / 8~9 / 20~23，改成只发 window 之后
//   变成 22~24 / 6~7 / 27~31，而浏览器是 7~23 / 1~2 / 1。
//   环境变量保留只为将来复验，不要改默认值。
const SO_TARGET = String(process.env.SENTINEL_SO_TARGET || "win").toLowerCase();
const SO_ONLY = String(process.env.SENTINEL_SO_ONLY || "").trim();

async function simulateSession(context, totalMs) {
  const win = context.window;
  const doc = context.document;
  if (!win || typeof win.dispatchEvent !== "function") {
    await new Promise((r) => setTimeout(r, totalMs));
    return;
  }
  const started = Date.now();
  const W = 390, H = 844;                 // iPhone 视口，与画像一致
  let x = 120 + Math.random() * 150;
  let y = 300 + Math.random() * 200;
  let scrollY = 0;
  const fire = (type, extra) => {
    const ev = { type, timeStamp: Date.now() - started, isTrusted: true, bubbles: true, ...extra };
    // ★ 派发目标可切换（SENTINEL_SO_TARGET=win|doc|both，默认 win）。
    //   之所以要能切：同时往 window 和 document 派发会让【每个事件被算两次】——
    //   实测槽1/11/12 分别高出浏览器 2 倍 / 5 倍 / 20 倍，重复计数是首要嫌疑。
    //   监听到底挂在哪个对象上，collector_dx 里看不出来（addEventListener 是从
    //   寄存器里取的对象），只能实测：分别只发一个，看哪个能让槽变非零。
    // ★ 差分实验用：SENTINEL_SO_ONLY=pointermove 时只派发这一类，
    //   用来把「哪个槽由哪类事件驱动」测出来 —— 比反汇编 529 条指令快得多。
    //   生产不设这个变量，全类型照常派发。
    if (SO_ONLY && type !== SO_ONLY) return;
    const targets = SO_TARGET === "doc" ? [doc] : SO_TARGET === "both" ? [win, doc] : [win];
    for (const t of targets) {
      try { if (t && typeof t.dispatchEvent === "function") t.dispatchEvent({ ...ev, target: t }); }
      catch (_) { /* 派发失败不该毁掉整轮 */ }
    }
  };
  // ★★ 行为模型按【iPhone 触摸】重写（2026-08-15 差分实验定的口径）。
  //
  //   差分实测（每次只发一类事件，看哪个槽动）：
  //     keydown     → 槽1 / 槽4 / 槽10 / 槽11
  //     pointermove → 槽12（主要）
  //     click       → 槽13 / 槽14
  //     scroll      → 槽12（少量）
  //     wheel/paste → 在我们的模拟下不驱动任何槽
  //
  //   之前是【桌面鼠标】模型：55% 概率连续移动指针，于是槽12 跑到 18~25，
  //   而浏览器【恒为 1】。原因不是发多了，是【设备不对】——
  //   画像是 iPhone，真机没有鼠标，pointermove 只在手指拖动时才有，
  //   一次填表通常就一两段。同理 wheel 在触摸设备上基本不出现。
  //   所以这不是"把数值调进区间"，是把输入方式换成与画像一致的那种。
  //
  //   这一页是 /about-you（填姓名），真人行为大致是：
  //   点一下输入框 → 打十来个字母 → 偶尔滚一下 → 点提交，其余时间盯着屏幕。
  const tap = () => fire("click", {
    clientX: Math.round(x), clientY: Math.round(y),
    screenX: Math.round(x), screenY: Math.round(y), button: 0, detail: 1,
  });
  // ★ 确定性剂量模式（只给实验用，生产不设这些变量）：
  //   SENTINEL_SO_DRAGS=N   拖动段数
  //   SENTINEL_SO_MOVES=M   每段几个 pointermove
  //   SENTINEL_SO_TAPS=K    点击次数
  //   SENTINEL_SO_SCROLLS=S 滚动次数
  //   SENTINEL_SO_KEYS=T    按键次数
  //   用来做剂量-反应：把某一类事件的个数固定，看槽值怎么变 ——
  //   比反汇编 snapshot_dx 里那几条指令快，而且是实测。
  if (process.env.SENTINEL_SO_DRAGS !== undefined) {
    const N = Number(process.env.SENTINEL_SO_DRAGS) || 0;
    const M = Number(process.env.SENTINEL_SO_MOVES) || 5;
    const K = Number(process.env.SENTINEL_SO_TAPS) || 0;
    const S = Number(process.env.SENTINEL_SO_SCROLLS) || 0;
    const T = Number(process.env.SENTINEL_SO_KEYS) || 0;
    const tapD = () => fire("click", {
      clientX: Math.round(x), clientY: Math.round(y),
      screenX: Math.round(x), screenY: Math.round(y), button: 0, detail: 1,
    });
    for (let i = 0; i < K; i++) { tapD(); await new Promise((r) => setTimeout(r, 200)); }
    for (let i = 0; i < T; i++) {
      const ch = "abcdefghijklmnopqrstuvwxyz"[Math.floor(Math.random() * 26)];
      fire("keydown", { key: ch, code: `Key${ch.toUpperCase()}`, keyCode: ch.charCodeAt(0) - 32 });
      await new Promise((r) => setTimeout(r, 120));
    }
    for (let i = 0; i < S; i++) {
      scrollY += 80; fire("scroll", {});
      try { win.scrollY = scrollY; } catch (_) {}
      await new Promise((r) => setTimeout(r, 300));
    }
    for (let d = 0; d < N; d++) {
      for (let k = 0; k < M; k++) {
        const dx = (Math.random() - 0.5) * 24, dy = (Math.random() - 0.5) * 40;
        x = Math.min(W - 4, Math.max(4, x + dx)); y = Math.min(H - 4, Math.max(4, y + dy));
        fire("pointermove", {
          clientX: Math.round(x), clientY: Math.round(y),
          screenX: Math.round(x), screenY: Math.round(y),
          pageX: Math.round(x), pageY: Math.round(y + scrollY),
          movementX: Math.round(dx), movementY: Math.round(dy),
          pointerType: "touch", isPrimary: true,
        });
        await new Promise((r) => setTimeout(r, 20));
      }
      await new Promise((r) => setTimeout(r, 700));   // 段与段之间有明显间隔
    }
    const left = totalMs - (Date.now() - started);
    if (left > 0) await new Promise((r) => setTimeout(r, left));
    return;
  }
  await new Promise((r) => setTimeout(r, 400 + Math.random() * 900));
  tap();                                              // ① 点进输入框
  const nameLen = 8 + Math.floor(Math.random() * 6);  // ② 打姓名
  for (let i = 0; i < nameLen && Date.now() - started < totalMs; i++) {
    const ch = "abcdefghijklmnopqrstuvwxyz"[Math.floor(Math.random() * 26)];
    fire("keydown", { key: ch, code: `Key${ch.toUpperCase()}`, keyCode: ch.charCodeAt(0) - 32 });
    // 真人打字间隔是长尾的：多数 80~220ms，偶尔停顿半秒以上
    const gap = Math.random() < 0.12 ? 400 + Math.random() * 900 : 80 + Math.random() * 140;
    await new Promise((r) => setTimeout(r, gap));
  }
  // ★★★ 最终形态：整轮【恰好一次点击，零个独立 pointermove，零 wheel】。
  //
  //   这是逐槽实测定下来的，不是估的：
  //     · 剂量-反应：槽12 = 指针事件的【精确计数】
  //       （5 个 pointermove→5、10→10、20→20、40→40，系数恰好 1；一次 click 也 +1）
  //     · 浏览器实测：注册两跳上 19/20 恰好是 1（另 1 个是 5）
  //       —— 也就是说真机整轮【只有一个指针事件】，就是那一次点击本身。
  //     · 组合验证：点击 1 次 + pointermove 0 个 →
  //       槽1=12 槽4=3.09 槽10=20941 槽11=1 槽12=1 槽13=20736 槽14=1
  //       八个槽全部落在浏览器区间内。
  //
  //   机理：触摸设备上没有鼠标，不存在悬停移动；一次点击自带落点，
  //   所以"点一下"就是一个指针事件，不该再单独派发 pointermove。
  //   ⚠ 别加回"拖动段"或"多点几下"：点 2 次会让槽12→2、槽13→5 万（越界）、槽14→2。
  //     这三个槽是联动的，动一个就全歪。
  // ⚠ 连 scroll 都不发：差分实测 scroll 也计入槽12（只发 scroll → 槽12=2）。
  //   浏览器槽12 恒为 1，说明它整轮【没有滚动过】—— /about-you 是短表单，
  //   一屏放得下，真人确实不需要滚。加回来就会让槽12 变成 1~4。
  //   剩下的时间就是静止等待：真人盯着屏幕的时间远多于操作的时间。
  const left = totalMs - (Date.now() - started);
  if (left > 0) await new Promise((r) => setTimeout(r, left));
}


async function main(argv = process.argv.slice(2), writeOutput = true) {
  const args = readArgs(argv);
  if (args.help === "1" || args.h === "1") {
    const helpText = [
      "用法：",
      "  node sentinel-runner.js --cookie \"你的 Cookie\"",
      "  node sentinel-runner.js --bearer \"Bearer 你的 token\"",
      "  node sentinel-runner.js --cookie \"你的 Cookie\" --bearer \"Bearer 你的 token\"",
      "  node sentinel-runner.js --config sentinel.config.json",
      "",
      "默认会读取当前目录、tools 目录或项目根目录的 sentinel.config.json。",
      "",
      "常用参数：",
      "  --flow checkout_session_approval",
      "  --page-url https://chatgpt.com/checkout/openai_llc/cs_xxx",
      "  --device-id 你的_oai-did",
      "  --challenge-url 自定义题目 challenge API",
      "  --sdk 指定 sdk.js 路径",
      "  --no-cookie 生成 token 时不向 challenge API 发送 Cookie",
    ].join("\n");
    if (writeOutput) process.stdout.write(`${helpText}\n`);
    return helpText;
  }

  const { path: configPath, data: config } = readConfig(args);
  const ignoreEnvForCredentials = Boolean(configPath);
  const cfg = configGetter(config);
  const defaultSdkPath = fs.existsSync(path.resolve(__dirname, "sdk.js"))
    ? path.resolve(__dirname, "sdk.js")
    : path.resolve(__dirname, "..", "sdk.js");
  const sdkPath = path.resolve(pick(args["sdk"], cfg("sdk", "sdkPath"), process.env.SENTINEL_SDK_PATH, defaultSdkPath));
  const flow = pick(args.flow, cfg("flow"), process.env.SENTINEL_FLOW, "checkout_session_approval");
  const challengeFile = pick(args["challenge-file"], cfg("challengeFile", "challenge_file"), process.env.SENTINEL_CHALLENGE_FILE);
  const officialMode =
    args.official === "1" ||
    truthy(cfg("official")) ||
    process.env.SENTINEL_OFFICIAL === "1" ||
    (!ipcMode && !challengeFile && !args["challenge-url"] && !cfg("challengeUrl", "challenge_url") && !process.env.SENTINEL_CHALLENGE_URL);
  const challengeUrl =
    pick(args["challenge-url"], cfg("challengeUrl", "challenge_url"), process.env.SENTINEL_CHALLENGE_URL) ||
    (officialMode ? OFFICIAL_CHALLENGE_URL : "");
  const noCookie = args["no-cookie"] === "1" || truthy(cfg("noCookie", "no_cookie"));
  const cookieArg = noCookie ? "" : pick(args.cookie, args.cookies, cfg("cookie", "cookies"));
  const bearerArg = pick(args.bearer, args.authorization, cfg("bearer", "bearerToken", "authorization", "accessToken"));
  const contentType = pick(args["content-type"], cfg("contentType", "content_type"));
  const debugDx = args["debug-dx"] === "1" || truthy(cfg("debugDx", "debug_dx"));
  // 只走命令行/环境变量，不进配置文件：调试钩子绝不能被持久化到正常注册里
  const debugSo = args["debug-so"] === "1" || process.env.SENTINEL_DEBUG_SO === "1";
  // ★ 2026-08-16：--debug-fp —— 只回答一个问题：sdk 到底【读不读】canvas / WebGL /
  //   measureText 这些经典指纹面？读不到的东西改了也是白改，而且改错还会引入新特征。
  //   审计动机：createCanvas 的 toDataURL 是写死的一串 base64（所有号同一个 canvas 哈希），
  //   WEBGL_debug_renderer_info 拿到的 UNMASKED_* 常量在 getParameter 的表里【没有】，
  //   于是 GPU 名返回 0 —— 真浏览器返回 "ANGLE (NVIDIA, ...)"。两条都是标准的连坐键，
  //   但要先证明 sdk 会读，才值得动。
  //
  //   ★★ 结论（2026-08-16 定案）：【读取次数 0，确定不用改】。
  //   12 轮实测，每一轮服务端下发的 challenge 都是 turnstile+pow+so 全要
  //   （turnstile 已解算：token.t 长度 1128、非报错串），canvas / WebGL /
  //   measureText / AudioContext 的读取次数依然全部为 0。
  //   所以那两条"连坐键"是【死代码】，改了纯属白工，还可能引入新特征。
  //
  //   ⚠ 我一度报告"6 轮都没碰上要 turnstile 的 challenge"——那是【错的】，
  //     是我拿一个不存在的字段名去查：token 的字段形状本来就是 p,t,c,id,flow，
  //     turnstile 的解算结果放在 **t** 里（判定见 sentinelClient.js 的 isBrokenTurnstile）。
  //     要看 turnstile 状态就读 parsed.t，别去找名为 turnstile 的字段。
  const debugFp = args["debug-fp"] === "1" || process.env.SENTINEL_DEBUG_FP === "1";
  // ★ 2026-08-16：--trace-reads —— 回答「sentinel 到底看什么」。
  //   之前我们一直在补【猜它会看】的东西，两次差点白干（想破解 so 密文、给 UA 加随机
  //   构建号）。与其继续猜，不如把读取行为直接量出来。它回答三个悬而未决的问题：
  //     ① window 键池：sentinel 用 Object.keys 还是 getOwnPropertyNames？
  //        —— 这决定我们要补几十个键还是上千个构造器。
  //     ② navigator 上到底哪些属性被读了（p[10] 抽样之外还有没有别的）。
  //     ③ 有没有碰 iframe 的 contentWindow（决定要不要造独立 realm）。
  //
  //   ★★ 三条全部定案（2026-08-16），而且【三条都是"不用做"】：
  //     ① keys(window)×2 —— 用的是 Object.keys。真 Chrome 可枚举 237 / 我们 275，
  //        已在正确量级。getOwnPropertyNames 那 1232 个 Web API 构造器它不看，不补。
  //        （只有 Navigator.prototype 它用 getOwnPropertyNames + 逐键 getOwnPropertyDescriptor，
  //          document 原型也逐键查描述符 —— 所以那两处必须按全量对齐，已做。）
  //     ② navigator 实际取值只有 9 个：hardwareConcurrency/userAgent/language/
  //        languages/deviceMemory/login/vendor/platform/maxTouchPoints。
  //        platform 与 maxTouchPoints 正是本轮修的那两个 —— 它真的读，
  //        "UA 说 iPhone、platform 报 MacIntel"是会被实际读到的矛盾，不是理论风险。
  //        document 那边只取 cookie（×4，取 oai-did）。
  //     ③ iframe.contentWindow 被取 6 次，但 cw.get 只有 postMessage×2，
  //        没有 Function / navigator / document —— 那是 SDK 自己的工作通道
  //        （handleIframeMessage），不是 CreepJS 那种跨 realm 探测。不造独立 realm。
  //        ⚠ 光数"取了 6 次"会判错（6 次很像在探测），必须看它从上面取哪个属性 ——
  //          所以下面给 contentWindow 套了 Proxy 逐属性记。
  const traceReads = args["trace-reads"] === "1" || process.env.SENTINEL_TRACE_READS === "1";
  // ★ 2026-08-16：--dump-surface=<js文件> —— 在 VM 里跑一段【外部给的枚举脚本】并把结果打出来。
  //   用途：把同一份枚举脚本在【本机真 Chrome】和【我们的 VM】里各跑一次，逐项 diff。
  //   这比"照着开源 FP 项目补字段"硬得多 —— 差异是量出来的，不是列清单猜的；
  //   而且能抓到「真浏览器有、我们没有」这类【缺失】，那比"值恒定"更没法解释。
  //   ⚠ 只在显式传参时执行，正常注册这段不跑。
  const dumpSurfaceFile = pick(args["dump-surface"], process.env.SENTINEL_DUMP_SURFACE);
  const debugDxLimit = Number(pick(args["debug-dx-limit"], cfg("debugDxLimit", "debug_dx_limit"), 80));
  const deviceId =
    pick(args["device-id"], cfg("deviceId", "device_id", "oaiDid", "oai_did"), process.env.SENTINEL_OAI_DID) ||
    "8a5ad769-e9e7-4461-ae3a-6755d7f46b0b";

  if (!fs.existsSync(sdkPath)) throw new Error(`找不到 SDK 文件：${sdkPath}`);
  if (!ipcMode && !challengeFile && !challengeUrl) {
    throw new Error("请提供 --challenge-file、--challenge-url 或 --official，用于把题目服务器 challenge 喂回 SDK。");
  }

  let cachedChallenge = null;
  const options = {
    flow,
    sentinelSid: pick(args["sentinel-sid"], cfg("sentinelSid", "sentinel_sid"), process.env.SENTINEL_SID, ""),
    pageUrl: pick(args["page-url"], cfg("pageUrl", "page_url"), process.env.SENTINEL_PAGE_URL, "https://chatgpt.com/checkout/openai_llc/cs_ctf"),
    scriptSrc:
      pick(
        args["script-src"],
        cfg("scriptSrc", "script_src"),
        process.env.SENTINEL_SCRIPT_SRC,
      "https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js",
      ),
    buildId: pick(args["build-id"], cfg("buildId", "build_id"), process.env.SENTINEL_BUILD_ID, ""),
    // 页面已经打开多久（毫秒）。不传则按实测区间随机。见 browserPerformance 的注释。
    pageAgeMs: Number(pick(args["page-age-ms"], cfg("pageAgeMs", "page_age_ms"), process.env.SENTINEL_PAGE_AGE_MS, "")) || 0,
    // 页面（iframe）打开的绝对时刻，epoch 毫秒。整条注册流程共用一个，见 pageAgeMs 处的注释。
    pageOpenAt: Number(pick(args["page-open-at"], cfg("pageOpenAt", "page_open_at"), process.env.SENTINEL_PAGE_OPEN_AT, "")) || 0,
    reactListeningKey: pick(args["react-listening-key"], cfg("reactListeningKey", "react_listening_key"), process.env.SENTINEL_REACT_LISTENING_KEY, ""),
    reactContainerKey: pick(args["react-container-key"], cfg("reactContainerKey", "react_container_key"), process.env.SENTINEL_REACT_CONTAINER_KEY, ""),
    reactResourcesKey: pick(args["react-resources-key"], cfg("reactResourcesKey", "react_resources_key"), process.env.SENTINEL_REACT_RESOURCES_KEY, ""),
    cookie: noCookie
      ? `oai-did=${deviceId}`
      : cookieArg ||
        (ignoreEnvForCredentials ? "" : process.env.SENTINEL_COOKIE || process.env.CHATGPT_COOKIE) ||
        `oai-did=${deviceId}`,
    userAgent:
      pick(
        args["user-agent"],
        cfg("userAgent", "user_agent"),
        process.env.SENTINEL_USER_AGENT,
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
      ),
    contentType,
    browserFamily: pick(args["browser-family"], cfg("browserFamily", "browser_family"), process.env.SENTINEL_BROWSER_FAMILY, "chrome"),
    navigatorPlatform: pick(args["navigator-platform"], cfg("navigatorPlatform", "navigator_platform"), process.env.SENTINEL_NAVIGATOR_PLATFORM, "MacIntel"),
    navigatorVendor: pick(args["navigator-vendor"], cfg("navigatorVendor", "navigator_vendor"), process.env.SENTINEL_NAVIGATOR_VENDOR, "Google Inc."),
    userAgentDataPlatform: pick(args["user-agent-data-platform"], cfg("userAgentDataPlatform", "user_agent_data_platform"), process.env.SENTINEL_UA_DATA_PLATFORM, "macOS"),
    requestIdleCallback: truthy(pick(args["request-idle-callback"], cfg("requestIdleCallback", "request_idle_callback"), process.env.SENTINEL_REQUEST_IDLE_CALLBACK, "0")),
    language: pick(args.language, cfg("language"), process.env.SENTINEL_LANGUAGE, "ja-JP"),
    languages: normalizeList(pick(args.languages, cfg("languages")), process.env.SENTINEL_LANGUAGES || "ja-JP"),
    timeZone: pick(args["time-zone"], args.timezone, cfg("timeZone", "time_zone", "timezone"), process.env.SENTINEL_TIME_ZONE, "Asia/Tokyo"),
    timezoneName: pick(args["timezone-name"], cfg("timezoneName", "timezone_name"), process.env.SENTINEL_TIMEZONE_NAME, "Japan Standard Time"),
    timezoneOffsetMinutes: Number(pick(args["timezone-offset-minutes"], cfg("timezoneOffsetMinutes", "timezone_offset_minutes"), process.env.SENTINEL_TIMEZONE_OFFSET_MINUTES, 540)),
    hardwareConcurrency: Number(pick(args.cores, cfg("cores", "hardwareConcurrency"), process.env.SENTINEL_CORES, 6)),
    jsHeapSizeLimit: Number(pick(args["js-heap-size-limit"], cfg("jsHeapSizeLimit", "js_heap_size_limit"), process.env.SENTINEL_JS_HEAP_SIZE_LIMIT, 4395630592)),
    fixedRandom:
      pick(args.random, cfg("random", "fixedRandom"), process.env.SENTINEL_FIXED_RANDOM)
        ? Number(pick(args.random, cfg("random", "fixedRandom"), process.env.SENTINEL_FIXED_RANDOM))
        : Number.NaN,
    deviceMemory: Number(pick(args["device-memory"], cfg("deviceMemory", "device_memory"), process.env.SENTINEL_DEVICE_MEMORY, 8)),
    devicePixelRatio: Number(pick(args["device-pixel-ratio"], cfg("devicePixelRatio", "device_pixel_ratio"), process.env.SENTINEL_DEVICE_PIXEL_RATIO, 2)),
    chromeMajor: pick(args["chrome-major"], cfg("chromeMajor", "chrome_major"), process.env.SENTINEL_CHROME_MAJOR, "149"),
    chromeFullVersion: pick(args["chrome-full-version"], cfg("chromeFullVersion", "chrome_full_version"), process.env.SENTINEL_CHROME_FULL_VERSION, "149.0.0.0"),
    secChUa: pick(args["sec-ch-ua"], cfg("secChUa", "sec_ch_ua"), process.env.SENTINEL_SEC_CH_UA, '"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"'),
    secChUaPlatform: String(pick(args["sec-ch-ua-platform"], cfg("secChUaPlatform", "sec_ch_ua_platform"), process.env.SENTINEL_SEC_CH_UA_PLATFORM, "macOS")).replace(/^"|"$/g, ""),
    secChUaFullVersionList: pick(args["sec-ch-ua-full-version-list"], cfg("secChUaFullVersionList", "sec_ch_ua_full_version_list"), process.env.SENTINEL_SEC_CH_UA_FULL_VERSION_LIST, ""),
    secChUaPlatformVersion: String(pick(args["sec-ch-ua-platform-version"], cfg("secChUaPlatformVersion", "sec_ch_ua_platform_version"), process.env.SENTINEL_SEC_CH_UA_PLATFORM_VERSION, "15.7.0")).replace(/^"|"$/g, ""),
    secChUaArch: String(pick(args["sec-ch-ua-arch"], cfg("secChUaArch", "sec_ch_ua_arch"), process.env.SENTINEL_SEC_CH_UA_ARCH, "arm")).replace(/^"|"$/g, ""),
    secChUaBitness: String(pick(args["sec-ch-ua-bitness"], cfg("secChUaBitness", "sec_ch_ua_bitness"), process.env.SENTINEL_SEC_CH_UA_BITNESS, "64")).replace(/^"|"$/g, ""),
    secChUaModel: String(pick(args["sec-ch-ua-model"], cfg("secChUaModel", "sec_ch_ua_model"), process.env.SENTINEL_SEC_CH_UA_MODEL, "")).replace(/^"|"$/g, ""),
    cfEdgeMsec: Number(pick(args["cf-edge-msec"], cfg("cfEdgeMsec", "cf_edge_msec"), process.env.SENTINEL_CF_EDGE_MSEC, 38)),
    cfOriginTtfbMsec: Number(pick(args["cf-origin-ttfb-msec"], cfg("cfOriginTtfbMsec", "cf_origin_ttfb_msec"), process.env.SENTINEL_CF_ORIGIN_TTFB_MSEC, 74)),
    cfTcpRttMsec: Number(pick(args["cf-tcp-rtt-msec"], cfg("cfTcpRttMsec", "cf_tcp_rtt_msec"), process.env.SENTINEL_CF_TCP_RTT_MSEC, 22)),
    cfQuicRttMsec: Number(pick(args["cf-quic-rtt-msec"], cfg("cfQuicRttMsec", "cf_quic_rtt_msec"), process.env.SENTINEL_CF_QUIC_RTT_MSEC, 0)),
    screen: (() => {
      const width = Number(pick(args.width, cfg("width", "screenWidth"), process.env.SENTINEL_SCREEN_WIDTH, 1680));
      const height = Number(pick(args.height, cfg("height", "screenHeight"), process.env.SENTINEL_SCREEN_HEIGHT, 1050));
      return {
        width,
        height,
        availWidth: width,
        availHeight: Math.max(0, height - 38),
        colorDepth: 30,
        pixelDepth: 30,
        orientation: { type: "landscape-primary", angle: 0 },
      };
    })(),
    async handleIframeMessage(message) {
      if (message.type !== "token" && message.type !== "init") {
        throw new Error(`未知 iframe 消息类型：${message.type}`);
      }
      const proof = message.p;
      // ★★★ 2026-08-12 IPC 模式：把 SDK 现算的 p 交回父进程，由父进程用【带代理和
      //   TLS 指纹的会话】去换题，再把题传回来。
      //
      //   为什么必须这样：challenge 里的三块 dx（turnstile.dx / so.collector_dx /
      //   so.snapshot_dx）是服务端用【请求时发上去的那个 p】做 XOR 密钥加密的。
      //   而 --challenge-file 模式下，题是父进程用它自己造的另一个 p 换来的，
      //   cachedProof 却是这里 SDK 现算的 p —— 两把钥匙不同，dx 解出来是乱码，
      //   JSON.parse 抛错，sdk 的 catch 把「0: SyntaxError: ...」base64 之后当 token 发出去。
      //   实测确证：生产的 turnstile token 一直是那 132 字符的报错串。
      //
      //   为什么不用现成的 --challenge-url：那条路虽然 p 是配对的，但 fetchChallenge
      //   用的是裸 fetch —— 没有代理、没有 TLS 指纹，取题那一跳会从机房 IP 裸奔出去。
      if (ipcMode) {
        cachedChallenge = await requestChallengeViaIpc(proof, flow);
      } else if (challengeFile) {
        cachedChallenge ||= readChallengeFile(challengeFile);
      } else {
        cachedChallenge = await fetchChallenge(challengeUrl, flow, proof, deviceId, {
          officialMode,
          pageUrl: options.pageUrl,
          userAgent: options.userAgent,
          cookie: noCookie ? "" : cookieArg,
          bearer: bearerArg,
          contentType: options.contentType,
          ignoreEnv: ignoreEnvForCredentials,
        });
      }
      if (debugDx && cachedChallenge?.turnstile?.dx) {
        try {
          const decoded = decodeDx(cachedChallenge.turnstile.dx, proof);
          const limit = Number.isFinite(debugDxLimit) && debugDxLimit > 0 ? debugDxLimit : 80;
          process.stderr.write(`dx 前 ${limit} 条指令：${JSON.stringify(decoded.slice(0, limit))}\n`);
        } catch (error) {
          process.stderr.write(`dx 解码失败：${error.message}\n`);
        }
      }
      return {
        cachedProof: proof,
        cachedChatReq: cachedChallenge,
      };
    },
  };

  if (options.timeZone) {
    process.env.TZ = options.timeZone;
  }

  const { context, clearTimers } = createBrowserContext(options);

  // ★ 时区显示名的本地化泄漏（2026-08-12 实测发现）。
  //
  //   process.env.TZ 只管【偏移】，管不了 Date.prototype.toString 括号里那个
  //   时区【显示名】—— 那个名字跟着宿主机的系统语言走。在一台中文 Windows 上，
  //   p[1] 实际发出去的是：
  //       "Wed Aug 12 2026 21:50:31 GMT+0900 (日本标准时间)"   ← 简体中文
  //   而线上真机是日文的「日本標準時」。同一个 p 数组里 [7]/[8] 却写着 ja-JP，
  //   自相矛盾；更糟的是这个值【跨所有账号恒定】，是一把完美的聚类钥匙 ——
  //   它在每一个 token 上签的是操作机的身份，不是号的画像。
  //
  //   --timezone-name 这个参数一直是收了不用的（只在 options 里躺着），这里把它接上。
  //   只改括号里那一段，日期与偏移仍由真 Date 产出，不自己拼时间。
  if (options.timezoneName) {
    const RealDate = context.Date;
    const realToString = RealDate.prototype.toString;
    const tzName = String(options.timezoneName);
    const patched = function toString() {
      const raw = realToString.call(this);
      // 只替换末尾括号里的显示名；Invalid Date 之类没有括号的原样返回
      return raw.replace(/\(([^)]*)\)\s*$/, `(${tzName})`);
    };
    Object.defineProperty(patched, "name", { value: "toString" });
    // sdk 可能对原生函数做 toString 自检 —— 保持 [native code] 形态
    Object.defineProperty(patched, "toString", {
      value: () => "function toString() { [native code] }",
    });
    Object.defineProperty(RealDate.prototype, "toString", {
      value: patched, writable: true, configurable: true, enumerable: false,
    });
  }
  let sdkCode = fs.readFileSync(sdkPath, "utf8");
  if (debugDx) {
    sdkCode = sdkCode.replace(
      "Cn.set(n,Cn.get(e)[Cn.get(r)].bind(Cn[t(24)](e)))",
      "(()=>{const __o=Cn.get(e),__p=Cn.get(r);if(!__o||!__o[__p])console.error('[dx bind missing]',typeof __o,__p,Object.prototype.toString.call(__o));return Cn.set(n,__o[__p].bind(__o))})()"
    );
  }
  // ★★ 2026-08-15：--debug-so —— 把 so token 的【明文 18 槽】读出来。
  //
  //   【为什么要有这个】
  //   so 的密文是我们唯一"发出去了但自己看不见内容"的东西。校准会话真实性
  //   （槽0 存活毫秒、7 个行为槽）全靠读它；读不到就只能靠密文长度这种间接信号。
  //
  //   【为什么不去破解密文】
  //   犯过这个错：2026-08-15 我试过拿 p、c、id、flow 等 14 种候选当 XOR 密钥
  //   逐个爆破，又做了周期分析（自相关峰在 15），全失败 —— 而且没必要。
  //   我们【自己就是运行环境】：明文数组必然经过 JSON.stringify 才能去加密，
  //   在沙箱里把 JSON.stringify 包一层就能原地拿到，不用碰密码学。
  //   ⚠ 抓包救不了这件事：registrationFlowCapture 把头部值截在 2055 字符
  //     （尾巴是 "…[+808]"），authflow 也截，成对样本没有一份是完整的。
  //
  //   只在显式传 --debug-so 时装；正常注册这段代码不执行。
  if (debugSo) {
    vm.runInContext(`(function(){
      const rec = (tag, s) => {
        if (typeof s !== 'string' || !s.length || s.length > 4000) return;
        (globalThis.__so_seen = globalThis.__so_seen || []).push(tag + ' ' + s);
      };
      const origStr = JSON.stringify;
      JSON.stringify = function () { const out = origStr.apply(this, arguments); rec('stringify', out); return out; };
      // so 的密文最后一定过 base64；btoa 的入参就是加密后的二进制串。
      // 把它也记下来，好和 sessionObserverToken 的返回值对上，确认是哪一次调用产的。
      const origB = globalThis.btoa;
      if (typeof origB === 'function') {
        globalThis.btoa = function (s) { rec('btoa-in', String(s)); return origB.apply(this, arguments); };
      }
      // 数组 join 也可能是序列化路径（有些实现不走 JSON）
      const origJoin = Array.prototype.join;
      Array.prototype.join = function () { const out = origJoin.apply(this, arguments); rec('join', out); return out; };
    })()`, context, { filename: "debug-so-hook" });
  }

  // --debug-fp：给经典指纹面挂计数器，看 sdk 有没有真的去读
  if (debugFp) {
    vm.runInContext(`(function(){
      const hits = globalThis.__fp_hits = {};
      const bump = k => { hits[k] = (hits[k] || 0) + 1; };
      const origCreate = document.createElement.bind(document);
      document.createElement = function (tag) {
        const el = origCreate(tag);
        if (String(tag).toLowerCase() === 'canvas') {
          bump('createElement(canvas)');
          const oTo = el.toDataURL, oCtx = el.getContext;
          if (typeof oTo === 'function') el.toDataURL = function () { bump('canvas.toDataURL'); return oTo.apply(this, arguments); };
          if (typeof oCtx === 'function') el.getContext = function (t) {
            bump('canvas.getContext(' + t + ')');
            const c = oCtx.apply(this, arguments);
            if (c && typeof c.getParameter === 'function') {
              const g = c.getParameter.bind(c);
              c.getParameter = function (p) { bump('webgl.getParameter(0x' + Number(p).toString(16) + ')'); return g(p); };
            }
            if (c && typeof c.getExtension === 'function') {
              const g2 = c.getExtension.bind(c);
              c.getExtension = function (n) { bump('webgl.getExtension(' + n + ')'); return g2(n); };
            }
            if (c && typeof c.measureText === 'function') {
              const m = c.measureText.bind(c);
              c.measureText = function (t) { bump('2d.measureText'); return m(t); };
            }
            return c;
          };
        }
        return el;
      };
      if (window.AudioContext) {
        const OA = window.AudioContext;
        window.AudioContext = function (...a) { bump('new AudioContext'); return new OA(...a); };
      }
    })()`, context, { filename: "debug-fp-hook" });
  }
  // ★★ 必须在 sdk 加载【之前】打：sdk 一旦跑起来就可能缓存 Function.prototype.toString。
  //   这条补丁堵的是「Function.prototype.toString.call(假原生) 会吐出真源码」，
  //   详见 makeNativeFunction 上方的注释。正常注册也要跑 —— 它不是调试功能。
  patchFunctionToString(context);
  applyWindowSurface(context);
  if (traceReads) installReadTracer(context);
  vm.runInContext(sdkCode, context, { filename: sdkPath });
  if (!context.SentinelSDK?.token) {
    throw new Error("SDK 加载后没有暴露 SentinelSDK.token");
  }

  // ★★★ 2026-08-15：先 init 装采集器，再算 token，中间/之后留出真实停留时间。
  //
  //   实测（解开 so token 的 18 槽，浏览器 20 份 vs 我们 82 份）：
  //     槽0 = 采集器存活毫秒数
  //       浏览器 3873~20733 ms（create_account 那一跳中位 17990）
  //       我们      9~25 ms   ← 差三个数量级，两组分布零重叠
  //     9 个行为槽(1/4/10~14)：浏览器 70~90% 非零，我们 82/82 恒为 0
  //
  //   也就是说我们每次交上去的「会话观察报告」内容是：
  //   「这个客户端存活了 11 毫秒，期间什么都没发生」。
  //   服务端不需要任何聚类，单看一个 token 就知道背后没有真实页面会话。
  //
  //   根因：采集器由 token() 内部装载，而我们紧接着就调 sessionObserverToken()，
  //   采集窗口 = 算一次 token 的耗时。浏览器是页面加载即 init，人停留几十秒后才铸 so。
  //   ⚠ SentinelSDK.init 是存在的（sdk.js 结尾 t.init=we），我们从来没调过。
  const dwellMs = Math.max(0, Number(args['so-dwell-ms'] ?? process.env.SENTINEL_SO_DWELL_MS ?? 0));
  if (typeof context.SentinelSDK.init === "function") {
    try { await context.SentinelSDK.init(flow); } catch (e) {
      process.stderr.write(`[so] init 失败(不致命): ${e && e.message || e}
`);
    }
  }
  // ★★ 顺序必须是 init → 停留 → token → so，不能先铸 token 再停留。
  //   浏览器是「页面开着十几秒、人填完表单才提交」，提交那一刻才铸 token，
  //   所以 token 铸出来到用掉只隔几秒（实测 2.3~15.9s，中位 8.2s）。
  //   先铸后停留的话，token 陈旧度会凭空多出一个 dwellMs —— 那是服务端
  //   免费读得出的（p[17]+p[13] 一加就知道页面何时打开、token 放了多久）。
  if (dwellMs > 0) await simulateSession(context, dwellMs);
  const tokenText = await context.SentinelSDK.token(flow);

  // ★ so token（2026-08-12 补）。
  //   实测 47 份浏览器抓包：/api/accounts/email-otp/validate（19 次）与
  //   /api/accounts/create_account（12 次）都【同时】带 openai-sentinel-token 与
  //   openai-sentinel-so-token —— 我们一直只发前者，后者从来没有过。
  //
  //   为什么必须在 p 握手修好【之后】才补：so 的两块料（so.collector_dx 与
  //   so.snapshot_dx）跟 turnstile.dx 一样是服务端用请求时那个 p 加密的。p 不配对时
  //   sdk 的 se() 压根不会激活 collector，sessionObserverToken() 只会返回 null，
  //   先补这行调用而不修 p 是白干。
  //
  //   形状由 sdk 的 ce(t,n) 决定：{so, c, id, flow}，id 取自 cookie 里的 oai-did。
  //   这里原样透传 sdk 的输出，不自己拼装。
  let soText = null;
  if (emitSo) {
    try {
      soText = typeof context.SentinelSDK.sessionObserverToken === "function"
        ? await context.SentinelSDK.sessionObserverToken(flow)
        : null;
    } catch (e) {
      // 拿不到 so 不该让整轮失败 —— 浏览器在 so 未激活时也是不发这个头的。
      soText = null;
      process.stderr.write(`[so] sessionObserverToken 失败: ${e && e.message || e}\n`);
    }
  }

  // --dump-surface：在 VM 里跑外部枚举脚本，结果打到 stderr 供 diff
  if (dumpSurfaceFile) {
    try {
      const expr = require(path.resolve(dumpSurfaceFile));
      const res = vm.runInContext(String(expr), context, { filename: "fp-surface" });
      process.stderr.write(`[surface] ${JSON.stringify(res)}\n`);
    } catch (e) {
      process.stderr.write(`[surface] ✗ 枚举失败: ${e && e.message || e}\n`);
    }
  }

  // --trace-reads：把读取记录打出来
  if (traceReads) {
    const R = context.__rt || {};
    const tally = (m) => Object.entries(m || {}).sort((a, b) => b[1] - a[1]);
    const keyCounts = {};
    for (const s of (R.keys || [])) keyCounts[s] = (keyCounts[s] || 0) + 1;
    process.stderr.write(`[read] ── 键枚举（回答"补哪个池子"）──\n`);
    for (const [k, v] of tally(keyCounts)) process.stderr.write(`[read]   ${k} × ${v}\n`);
    if (!Object.keys(keyCounts).length) process.stderr.write('[read]   （一次都没枚举）\n');
    process.stderr.write(`[read] ── navigator 被读的属性 ──\n`);
    for (const [k, v] of tally(R.nav)) process.stderr.write(`[read]   navigator.${k} × ${v}\n`);
    if (!Object.keys(R.nav || {}).length) process.stderr.write('[read]   （一个都没读）\n');
    process.stderr.write(`[read] ── document 被读的属性 ──\n`);
    for (const [k, v] of tally(R.doc)) process.stderr.write(`[read]   document.${k} × ${v}\n`);
    process.stderr.write(`[read] ── 其它 ──\n`);
    for (const [k, v] of tally(R.misc)) process.stderr.write(`[read]   ${k} × ${v}\n`);
  }

  // --debug-fp：sdk 到底读了哪些指纹面
  if (debugFp) {
    const hits = context.__fp_hits || (context.window && context.window.__fp_hits) || {};
    const keys = Object.keys(hits);
    process.stderr.write(`[fp] sdk 读过的指纹面 ${keys.length} 类\n`);
    for (const k of keys.sort()) process.stderr.write(`[fp]   ${k} × ${hits[k]}\n`);
    if (!keys.length) process.stderr.write('[fp]   （一个都没读 —— 这些面不在 sentinel 的采集范围内）\n');
  }

  // --debug-so：把沙箱里截到的槽位数组打到 stderr（不进 stdout，免得污染信封）
  if (debugSo) {
    const seen = context.__so_seen || (context.window && context.window.__so_seen) || [];
    process.stderr.write(`[so-plain] 截到 ${seen.length} 个候选数组\n`);
    for (const s of seen) process.stderr.write(`[so-plain] ${s}\n`);
  }

  clearTimers();
  // ★ 开着 IPC 通道时事件循环不会空 —— 子进程会一直挂着，父进程的 'close' 永远不触发，
  //   最后撞 SENTINEL_RUNNER_TIMEOUT。算完就主动断开。
  if (ipcMode && typeof process.disconnect === "function") {
    try { process.disconnect(); } catch (_) {}
  }
  if (!writeOutput) return emitSo ? { tokenText, soText } : tokenText;
  if (emitSo) {
    // ★ 信封格式：so 不能塞进 sentinel token 里 —— 那个字符串要原样当
    //   openai-sentinel-token 头发出去，多一个字段就是一个新特征。
    process.stdout.write(`${JSON.stringify({ sentinelToken: tokenText, soToken: soText })}\n`);
  } else if (args.pretty || process.env.SENTINEL_PRETTY === "1") {
    process.stdout.write(`${JSON.stringify(JSON.parse(tokenText), null, 2)}\n`);
  } else {
    process.stdout.write(`${tokenText}\n`);
  }
  return tokenText;
}

if (require.main === module) {
  main().catch((error) => fail(error?.stack || error?.message || String(error)));
}

module.exports = {
  main,
  normalizeChallenge,
};
