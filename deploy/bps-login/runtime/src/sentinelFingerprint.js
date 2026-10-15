'use strict';
// ── Sentinel 指纹 p 字段生成器（移植自 turb-gpt-free-register/core/sentinel.py）──
//
// 用途：POST sentinel/req 时 body.p 的值 —— 即 SDK 的 getRequirementsToken()。
//   格式： "gAAAAAC" + base64(JSON.stringify(config25)) + "~S"
//
// ★与既有 src/sentinelPow.js 的区别（后者目前无调用方）：
//   sentinelPow.js 的 solvePow 用 SHA3-512，且注释自承"待 difficulty>0 真样本对齐"。
//   实测 2026-08 服务端已下发 difficulty>0（例 07a120），真实算法是 FNV-1a（见下）。
//   但本模块**只负责首个 p**（requirements token，不需要 PoW）；
//   带 PoW 的最终 token 交给 sentinel/sentinel-runner.js 里的真 sdk.js 去算，
//   不在 JS 侧手写 PoW —— 这样 OpenAI 改算法时我们不用跟着逆向。
//
// config25 各槽语义见 sentinel.py 的注释；这里保持逐槽一一对应，改指纹要整套一起改。
const crypto = require('crypto');

// ── 指纹样本池（与 sentinel-runner.js 的 VM 环境共用同一组候选，抽样值可变但集合必须真实存在）──
const NAVIGATOR_PROTO_SAMPLES = [
    'createAuctionNonce−function createAuctionNonce() { [native code] }',
    'clearOriginJoinedAdInterestGroups−function clearOriginJoinedAdInterestGroups() { [native code] }',
    'updateAdInterestGroups−function updateAdInterestGroups() { [native code] }',
    'canLoadAdAuctionFencedFrame−function canLoadAdAuctionFencedFrame() { [native code] }',
    'gpu−[object GPU]',
    'getBattery−function getBattery() { [native code] }',
    'getGamepads−function getGamepads() { [native code] }',
    'javaEnabled−function javaEnabled() { [native code] }',
    'sendBeacon−function sendBeacon() { [native code] }',
    'vibrate−function vibrate() { [native code] }',
    'login−[object NavigatorLogin]',
];

// iOS 版抽样池。DEFAULT 那份里 11 个有 7 个在真 iPhone 上不存在
// （createAuctionNonce / clearOriginJoinedAdInterestGroups / updateAdInterestGroups /
//  canLoadAdAuctionFencedFrame 是 Protected Audience，getBattery 是 Battery API，
//  vibrate 是 Vibration API，NavigatorLogin 是 FedCM —— 全是 Chromium 桌面独有）。
// 抽中任意一个，就与 iPhone 的 UA 直接矛盾。
// ★ 实测生产载荷 [10] 抽到的是 'hardwareConcurrency−4' —— 一个普通属性，不是那些方法。
//   下面这些是 iOS Safari/CriOS 上确实存在的 Navigator 成员。
const IOS_NAVIGATOR_PROTO_SAMPLES = [
    'hardwareConcurrency−4',
    'sendBeacon−function sendBeacon() { [native code] }',
    'javaEnabled−function javaEnabled() { [native code] }',
    'getGamepads−function getGamepads() { [native code] }',
    'maxTouchPoints−5',
    'onLine−true',
    'cookieEnabled−true',
    'vendor−Apple Computer, Inc.',
    'platform−iPhone',
];

// window 键抽样：DEFAULT 那份含 'chrome'，而 CriOS/WebKit 上没有 window.chrome。
// ★ 实测生产载荷 [12] 抽到的是 'sessionStorage'。
const IOS_WINDOW_KEY_SAMPLES = [
    'window', 'self', 'top', 'parent', 'frames', 'navigator', 'screen', 'location',
    'localStorage', 'sessionStorage', 'history', 'innerWidth', 'innerHeight',
    'outerWidth', 'outerHeight', 'devicePixelRatio', 'performance', 'crypto',
    'TextEncoder', 'URL', 'URLSearchParams', 'AbortController',
    'scrollX', 'scrollY', 'ondevicemotion', 'requestAnimationFrame',
    'queueMicrotask', 'onfocus', 'onblur', 'onpageshow',
];

const DOCUMENT_KEY_SAMPLES = [
    'currentScript', 'scripts', 'cookie', 'URL', 'documentURI', 'referrer',
    'title', 'characterSet', 'charset', 'compatMode', 'contentType', 'readyState',
    'visibilityState', 'hidden', 'hasFocus', 'documentElement', 'body',
    'addEventListener', 'removeEventListener', 'querySelector', 'querySelectorAll',
    'getElementById', 'getElementsByTagName', 'createElement',
];

const WINDOW_KEY_SAMPLES = [
    'window', 'self', 'top', 'parent', 'frames', 'navigator', 'screen', 'location',
    'localStorage', 'sessionStorage', 'history', 'innerWidth', 'innerHeight',
    'outerWidth', 'outerHeight', 'devicePixelRatio', 'chrome', 'performance', 'crypto',
    'TextEncoder', 'URL', 'URLSearchParams', 'AbortController',
    'locationbar', 'scrollX', 'scrollY', 'ondevicemotion',
    'requestAnimationFrame', 'queueMicrotask', 'onfocus', 'onblur', 'onpageshow',
];

const WINDOW_FEATURE_FLAGS = {
    ai: 0, InstallTrigger: 0, cache: 0, data: 0, solana: 0, dump: 0,
    requestIdleCallback: 0,   // HAR 样本 p[24] 为 0
};

// ── 默认画像（HAR 参考：Default-all-domains-1784468371563.json 解码 p[0]/p[2]/p[16]）──
const DEFAULT_PROFILE = {
    screenWidth: 1680,
    screenHeight: 1050,
    hardwareConcurrency: 6,
    jsHeapSizeLimit: 4395630592,
    deviceMemory: 8,
    devicePixelRatio: 2,
    // ★大版本必须与 rtProtocolClient 的 TLS 指纹(node-tls-client chrome_131，库里最高的一档)对齐。
    //   原来这里写 Chrome/149：TLS 握手说"我是 Chrome 131"、HTTP 头说"我是 Chrome 149"，
    //   差两个大版本 —— 真浏览器不可能这样，是很干净的自动化特征。
    //   保留 macOS：下面的 screen 1680x1050 / devicePixelRatio 2 都是 Mac 画像，
    //   只改 OS 不改这些反而会让 sentinel token 内部自相矛盾。
    userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
    navigatorLanguage: 'ja-JP',
    navigatorLanguages: ['ja-JP'],
    timezoneOffsetMinutes: 9 * 60,
    timezoneName: 'Japan Standard Time',
    timezoneIana: 'Asia/Tokyo',
    buildId: null,          // auth 页面的 documentElement 通常没有 data-build
    sentinelSv: '20260810913b',
};

// ── iOS 画像（2026-08-12 从【生产浏览器真实上报】的 sentinel/req 载荷解码而来）──
//
// 来源：logs/registration-flow/registration-flow-bb0b36a2-*.json 的 body.p，
//       flow=email_otp_validate，base64 解出的 25 槽原值。不是猜的，是抄的。
//
// 【为什么需要它】DEFAULT_PROFILE 是 Mac 桌面画像（1680x1050 / hwc 6 / Macintosh UA），
//   而生产【浏览器】路径上报的是 iPhone 画像。两条路径的 sentinel 身份证长得完全不同 ——
//   浏览器号说自己是 iPhone，纯协议号说自己是 Mac。协议号要与浏览器号同簇，就得用这份。
//
// 【实测值 vs 我原先的预判】我本来以为 Roxy（桌面 Chromium 套壳）上报的会是桌面画像，
//   结果它做得相当到位：屏幕、UA、核数、requestIdleCallback 全是 iPhone 的。
//   唯一破绽是 [2] jsHeapSizeLimit —— performance.memory 是 Chromium 专属，真 iPhone 上没有。
//   ★ 这里【刻意保留】那个破绽：带着它的浏览器号有 3785 个样本、0 封号背书；
//     去掉它就变成一套零样本的新画像。要改成"真 iOS"是另一个决策，得用 A/B 数据说话。
//
// ⚠ 已知未验证项：[0] 只给出宽+高的【和】=1334，拆分方式载荷里没有。
//   而 screenWidth/screenHeight 会分别传给 sentinel-runner（--width/--height），
//   所以拆分理论上有影响。下面这组拆分是推断的，和对得上、但不保证与 Roxy 一致。
//   要确认得另外从浏览器里读一次 screen.width/height。
const IOS_PROFILE = {
    screenWidth: 390,
    screenHeight: 944,          // 390+944 = 1334，与实测的 [0] 一致（拆分未验证，见上）
    hardwareConcurrency: 4,     // 实测 [16]=4（桌面画像是 6）
    jsHeapSizeLimit: 4395630592,// 实测 [2] —— 保留这个 Chromium 破绽，理由见上
    deviceMemory: 8,
    devicePixelRatio: 3,        // iPhone 常见 3（未在载荷中体现，属推断）
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 '
        + '(KHTML, like Gecko) CriOS/150.0.7871.49 Mobile/15E148 Safari/537.36',
    navigatorLanguage: 'ja-JP',
    navigatorLanguages: ['ja-JP'],
    timezoneOffsetMinutes: 9 * 60,
    timezoneName: '日本標準時',   // ★ 实测 [1] 用的是本地化名称，不是英文 "Japan Standard Time"
    timezoneIana: 'Asia/Tokyo',
    buildId: null,
    sentinelSv: '20260810913b',
    // 抽样池也要换：DEFAULT 的 NAVIGATOR_PROTO_SAMPLES 里 7/11 是 iOS 上不存在的
    // （FLEDGE 那套、getBattery、vibrate、NavigatorLogin）。实测抽到的是普通属性。
    navigatorProtoSamples: IOS_NAVIGATOR_PROTO_SAMPLES,
    windowKeySamples: IOS_WINDOW_KEY_SAMPLES,
};

// 按出口地区选画像。与 config/browser.py 的 BROWSER_LOCALE_PROFILES 对齐。
const LOCALE_PROFILES = {
    jp: { navigatorLanguage: 'ja-JP', navigatorLanguages: ['ja-JP'], timezoneIana: 'Asia/Tokyo', timezoneOffsetMinutes: 540, timezoneName: 'Japan Standard Time', acceptLanguage: 'ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7' },
    // ★ US 默认时区取【纽约/东部】而不是洛杉矶：美国单一时区只能押一个，而住宅 IP 大致按人口分布——
    //   东部 ~47% > 中部 ~29% > 太平洋 ~16% > 山区 ~7%。押洛杉矶时每号约 16% 能对上自己出口 IP 的时区，
    //   押纽约约 47%，接近三倍。这是"单确定值"能做到的最优；要每号都跟自己 IP 精确对齐，得在出口闸门
    //   拿到 colo/IP 之后再定时区（结构改动，见 REGISTER_US 讨论），本档不涉及。
    //   ⚠ 下面 offset/name 只是 Intl 现算失败时的兜底：真实路径按 timezoneIana 用 Intl 现算名字与偏移
    //     （见 iosProfileFactory.generateProfileForRegion / 探针内联那段），会自动跟夏令时走。
    us: { navigatorLanguage: 'en-US', navigatorLanguages: ['en-US'], timezoneIana: 'America/New_York', timezoneOffsetMinutes: -240, timezoneName: 'Eastern Daylight Time', acceptLanguage: 'en-US,en;q=0.9' },
    sg: { navigatorLanguage: 'en-SG', navigatorLanguages: ['en-SG'], timezoneIana: 'Asia/Singapore', timezoneOffsetMinutes: 480, timezoneName: 'Singapore Standard Time', acceptLanguage: 'en-SG,en-US;q=0.9,en;q=0.8' },
    hk: { navigatorLanguage: 'zh-HK', navigatorLanguages: ['zh-HK'], timezoneIana: 'Asia/Hong_Kong', timezoneOffsetMinutes: 480, timezoneName: 'Hong Kong Standard Time', acceptLanguage: 'zh-HK,zh-TW;q=0.9,zh;q=0.8,en-US;q=0.7,en;q=0.6' },
    tw: { navigatorLanguage: 'zh-TW', navigatorLanguages: ['zh-TW'], timezoneIana: 'Asia/Taipei', timezoneOffsetMinutes: 480, timezoneName: 'Taipei Standard Time', acceptLanguage: 'zh-TW,zh;q=0.9,en-US;q=0.8,en;q=0.7' },
    gb: { navigatorLanguage: 'en-GB', navigatorLanguages: ['en-GB'], timezoneIana: 'Europe/London', timezoneOffsetMinutes: 60, timezoneName: 'GMT+01:00', acceptLanguage: 'en-GB,en;q=0.9' },
};

const COUNTRY_TO_LOCALE = {
    JP: 'jp', US: 'us', CA: 'us', SG: 'sg', HK: 'hk', TW: 'tw', GB: 'gb', AU: 'gb',
};

function localeProfileForCountry(country) {
    const key = COUNTRY_TO_LOCALE[String(country || '').toUpperCase()] || 'jp';
    return { key, ...LOCALE_PROFILES[key] };
}

function pick(list) {
    return list[Math.floor(Math.random() * list.length)];
}

function randomSuffix(n) {
    const chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
    let out = '';
    for (let i = 0; i < n; i++) out += chars[Math.floor(Math.random() * chars.length)];
    return out;
}

// 生成与配置时区一致的 Date.toString() 串。
// JS 的 new Date().toString() 走本机时区，这里必须按画像时区手工拼，
// 否则 p[1] 的时区和 runner 里 TZ 环境变量不一致 → 同一 token 内自相矛盾。
const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function dateStringForOffset(offsetMinutes, timezoneName, nowMs = Date.now()) {
    const shifted = new Date(nowMs + offsetMinutes * 60_000);
    const pad = n => String(n).padStart(2, '0');
    const sign = offsetMinutes >= 0 ? '+' : '-';
    const abs = Math.abs(offsetMinutes);
    const gmt = `GMT${sign}${pad(Math.floor(abs / 60))}${pad(abs % 60)}`;
    return `${WEEKDAYS[shifted.getUTCDay()]} ${MONTHS[shifted.getUTCMonth()]} ${pad(shifted.getUTCDate())} `
        + `${shifted.getUTCFullYear()} ${pad(shifted.getUTCHours())}:${pad(shifted.getUTCMinutes())}:${pad(shifted.getUTCSeconds())} `
        + `${gmt} (${timezoneName})`;
}

/**
 * 生成 25 元 config 数组。槽位语义与 sentinel.py::generate_fingerprint_data 逐一对应。
 * @param {string} deviceId  oai-did / sentinel sid，必须与后续 HTTP 请求用同一个值
 * @param {object} profile   画像覆盖项
 */
function buildConfig(deviceId, profile = {}) {
    const p = { ...DEFAULT_PROFILE, ...profile };
    const reactListeningKey = p.reactListeningKey || ('_reactListening' + randomSuffix(11));
    const reactContainerKey = p.reactContainerKey || ('__reactContainer$' + randomSuffix(11));
    const reactResourcesKey = p.reactResourcesKey || reactContainerKey.replace('__reactContainer$', '__reactResources$');

    const nowMs = Date.now();
    // perfNow / timeOrigin 必须满足 timeOrigin + perfNow ≈ Date.now()，避免同一 p 内时间自相矛盾
    //
    // ★ 2026-08-12 按线上真实载荷校正量级：实测 p[13] 是 17156 与 28354 毫秒
    //   （「iframe 已经开了 17~28 秒」），而这里原来是 1~8 秒 ——
    //   等于告诉服务端「页面开了几秒就交了 sentinel 答卷」。
    //   注册链路走 VM 里的真 SDK，已经改过；这里是 RT 链路，同步改，
    //   否则两条链路会发出两种可区分的时间分布，把号劈成两个总体。
    const perfNow = p.perfNowMs > 0 ? p.perfNowMs : 15000 + Math.random() * 14000;
    const timeOrigin = nowMs - perfNow;

    // ★ 2026-08-12：只留 sentinel SDK 自己。
    //   sentinel SDK 跑在 sentinel.openai.com 的【跨域 iframe】里，那个 document 的
    //   scripts 里物理上不可能有宿主页的脚本 —— 跨域隔离就是干这个的。
    //   线上真实载荷的 p[5] 3/3 都是 sentinel 自己的 sdk.js，从没出现过别的域。
    //   更糟的是原来那份清单里有 CF 的 jsd 脚本：我们【谎称加载过它】却从不真去取、
    //   也不发它的 oneshot 信标 —— 这比单纯"没有 CF"更容易判。
    const scriptSrcSamples = p.scriptSrcSamples || [
        `https://sentinel.openai.com/sentinel/${p.sentinelSv}/sdk.js`,
    ];
    const flags = { ...WINDOW_FEATURE_FLAGS, ...(p.windowFeatureFlags || {}) };

    return [
        p.screenWidth + p.screenHeight,                                     // [0]
        dateStringForOffset(p.timezoneOffsetMinutes, p.timezoneName, nowMs),// [1]
        p.jsHeapSizeLimit,                                                  // [2]
        1,                                                                  // [3] attempt（无 PoW 时固定 1）
        p.userAgent,                                                        // [4]
        pick(scriptSrcSamples),                                             // [5]
        p.buildId,                                                          // [6] data-build（auth 页通常 null）
        p.navigatorLanguage,                                                // [7]
        p.navigatorLanguages.join(','),                                     // [8]
        1 + Math.floor(Math.random() * 100),                                // [9] 耗时/随机
        pick(p.navigatorProtoSamples || NAVIGATOR_PROTO_SAMPLES),           // [10] 池可被画像覆盖（iOS 版见 IOS_NAVIGATOR_PROTO_SAMPLES）
        pick(DOCUMENT_KEY_SAMPLES.concat([reactListeningKey, reactContainerKey, reactResourcesKey])), // [11]
        pick(p.windowKeySamples || WINDOW_KEY_SAMPLES),                     // [12] 同上
        Math.round(perfNow * 1e10) / 1e10,                                  // [13] performance.now()
        String(deviceId),                                                   // [14] sid
        '',                                                                 // [15] URL search params
        p.hardwareConcurrency,                                              // [16]
        Math.round(timeOrigin * 10) / 10,                                   // [17] performance.timeOrigin
        flags.ai | 0,                                                       // [18]
        flags.InstallTrigger | 0,                                           // [19]
        flags.cache | 0,                                                    // [20]
        flags.data | 0,                                                     // [21]
        flags.solana | 0,                                                   // [22]
        flags.dump | 0,                                                     // [23]
        flags.requestIdleCallback | 0,                                      // [24]
    ];
}

// SDK 的 N()：JSON.stringify → TextEncoder(UTF-8) → btoa
function encodeConfig(config) {
    return Buffer.from(JSON.stringify(config), 'utf8').toString('base64');
}

/**
 * requirements token —— POST sentinel/req 的 body.p。不需要 PoW。
 */
function generateRequirementsToken(deviceId, profile = {}) {
    return 'gAAAAAC' + encodeConfig(buildConfig(deviceId, profile)) + '~S';
}

function buildSentinelRequestBody(p, deviceId, flow) {
    return JSON.stringify({ p, id: deviceId, flow });
}

/**
 * FNV-1a 32 位（SDK 的 PoW 校验哈希）。
 * ★这里只做**自检/离线校验**用途——线上 PoW 由 sentinel-runner.js 里的真 sdk.js 计算。
 *   保留它是为了将来能验证 runner 输出是否满足 difficulty，不参与请求路径。
 */
function fnv1a(text) {
    let h = 2166136261;
    for (let i = 0; i < text.length; i++) {
        h ^= text.charCodeAt(i);
        h = Math.imul(h, 16777619) >>> 0;
    }
    h ^= h >>> 16;
    h = Math.imul(h, 2246822507) >>> 0;
    h ^= h >>> 13;
    h = Math.imul(h, 3266489909) >>> 0;
    h ^= h >>> 16;
    return (h >>> 0).toString(16).padStart(8, '0');
}

function newDeviceId() {
    return crypto.randomUUID();
}

module.exports = {
    // ★ 2026-08-15 导出：sentinelClient 要复用它生成 React 注入键的后缀。
    //   两条链路（RT 的离线合成 / 协议的 vm）必须用【同一个字符集】——
    //   这里是 base36，而 sentinelClient 原来用的是 hex，线上 0/9 是纯 hex。
    //   各写各的等于把我们自己的号劈成两个可区分总体。
    randomSuffix,
    DEFAULT_PROFILE,
    IOS_PROFILE,
    IOS_NAVIGATOR_PROTO_SAMPLES,
    IOS_WINDOW_KEY_SAMPLES,
    LOCALE_PROFILES,
    NAVIGATOR_PROTO_SAMPLES,
    DOCUMENT_KEY_SAMPLES,
    WINDOW_KEY_SAMPLES,
    WINDOW_FEATURE_FLAGS,
    buildConfig,
    buildSentinelRequestBody,
    dateStringForOffset,
    encodeConfig,
    fnv1a,
    generateRequirementsToken,
    localeProfileForCountry,
    newDeviceId,
};
