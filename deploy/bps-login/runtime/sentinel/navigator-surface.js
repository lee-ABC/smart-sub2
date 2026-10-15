'use strict';
// 真 Chromium 的 Navigator.prototype 表面。
//
// 【为什么需要它】（2026-08-12）
//   sentinel 的 p 载荷第 [10] 槽是「从 navigator 原型上随机抽一个成员，
//   报成 `<键名>−<String(值)>`」。线上实测抽到的两个值是：
//       "webkitPersistentStorage−[object DeprecatedStorageQuota]"
//       "hardwareConcurrency−4"
//   两个都是【数据属性】（真 Chromium 里是 prototype 上的 getter）。
//
//   而 sentinel-runner 原来的 navigatorProto 只有 9 个成员、【全是函数】：
//     createAuctionNonce / clearOriginJoinedAdInterestGroups / updateAdInterestGroups /
//     canLoadAdAuctionFencedFrame / getBattery / getGamepads / javaEnabled /
//     sendBeacon / vibrate
//   于是我们抽出来的永远是 "getBattery−function getBattery() { [native code] }" 这类，
//   与线上零重合；而且 9 个取值意味着每个值被约 11% 的账号共用 —— 真 Chromium 是 60+，
//   单值占比降到 1.6%。这是【跨账号】层面的聚类特征，单个号跑通时完全看不出来。
//
//   更关键的一点：userAgent / language / hardwareConcurrency 这些在真 Chromium 里
//   是【原型上的 getter】，而 runner 原来用 Object.assign 挂在【实例】上。
//   这有两个后果：① 原型抽样永远抽不到它们（线上却抽得到）；
//   ② Object.keys(navigator) 在真浏览器里是空数组，在我们这里却列出一堆自有属性。
//
// 【口径】
//   · getters[] 里的键在真 Chromium 里是访问器；这里用 defineProperty(get) 复刻。
//   · methods[] 里的键是方法；用 makeNativeFunction 保持 [native code] 形态。
//   · 这份清单对应 Chromium 15x 桌面版。我们参照的 Roxy 就是 Chromium 冒充 iPhone
//     （抓包里 zstd / priority / sec-ch-ua 都是 Chromium 独有），所以照抄桌面表面
//     是【对的】，不要按 iOS Safari 裁剪 —— 线上抽到的 webkitPersistentStorage
//     本身就是桌面 Chromium 独有的。

/** 真 Chromium Navigator.prototype 上的访问器（getter）键。 */
const GETTER_KEYS = [
    'appCodeName', 'appName', 'appVersion', 'bluetooth', 'clipboard', 'connection',
    'cookieEnabled', 'credentials', 'deviceMemory', 'devicePosture', 'doNotTrack',
    'geolocation', 'gpu', 'hardwareConcurrency', 'hid', 'ink', 'keyboard', 'language',
    'languages', 'locks', 'login', 'managed', 'maxTouchPoints', 'mediaCapabilities',
    'mediaDevices', 'mediaSession', 'mimeTypes', 'onLine', 'pdfViewerEnabled',
    'permissions', 'platform', 'plugins', 'presentation', 'product', 'productSub',
    'scheduling', 'serial', 'serviceWorker', 'storage', 'storageBuckets', 'usb',
    'userActivation', 'userAgent', 'userAgentData', 'vendor', 'vendorSub',
    'virtualKeyboard', 'wakeLock', 'webdriver', 'webkitPersistentStorage',
    'webkitTemporaryStorage', 'windowControlsOverlay', 'xr',
];

/** 真 Chromium Navigator.prototype 上的方法键。 */
const METHOD_KEYS = [
    'canLoadAdAuctionFencedFrame', 'canShare', 'clearAppBadge',
    'clearOriginJoinedAdInterestGroups', 'createAuctionNonce', 'deprecatedReplaceInURN',
    'deprecatedURNToURL', 'getBattery', 'getGamepads', 'getInstalledRelatedApps',
    'getUserMedia', 'javaEnabled', 'joinAdInterestGroup', 'leaveAdInterestGroup',
    'registerProtocolHandler', 'requestMIDIAccess', 'requestMediaKeySystemAccess',
    'runAdAuction', 'sendBeacon', 'setAppBadge', 'share', 'unregisterProtocolHandler',
    'updateAdInterestGroups', 'vibrate', 'webkitGetUserMedia',
    // ★★ 2026-08-16 实测补：把同一份枚举脚本在【本机真 Chrome 150.0.7871.187】和本 VM 里
    //   各跑一次做差集，Navigator.prototype 我们 78 键、真 Chrome 83 键，差的就是下面这几个
    //   （外加单独处理的 constructor）。全是 Protected Audience / FLEDGE 广告竞价 API，
    //   Chromium 独有、平台无关（桌面与移动 Chrome 都有），属于该补的那一类。
    //   ⚠ 反向差集是 0 —— 我们没有漏出真 Chrome 没有的键，这一侧是干净的。
    'adAuctionComponents', 'protectedAudience',
    'getInterestGroupAdAuctionData', 'deprecatedRunAdAuctionEnforcesKAnonymity',
];

// 少数键的 String(值) 在真浏览器里有确定形态，抽样时会被原样报上去。
// webkitPersistentStorage 是线上【实测抽到过】的那个 —— 值必须逐字对上。
const OBJECT_TAGS = {
    webkitPersistentStorage: 'DeprecatedStorageQuota',
    webkitTemporaryStorage: 'DeprecatedStorageQuota',
    clipboard: 'Clipboard',
    credentials: 'CredentialsContainer',
    devicePosture: 'DevicePosture',
    gpu: 'GPU',
    hid: 'HID',
    ink: 'Ink',
    keyboard: 'Keyboard',
    locks: 'LockManager',
    login: 'NavigatorLogin',
    managed: 'NavigatorManagedData',
    mediaCapabilities: 'MediaCapabilities',
    mediaSession: 'MediaSession',
    presentation: 'Presentation',
    scheduling: 'Scheduling',
    serial: 'Serial',
    serviceWorker: 'ServiceWorkerContainer',
    storageBuckets: 'StorageBucketManager',
    usb: 'USB',
    userActivation: 'UserActivation',
    virtualKeyboard: 'VirtualKeyboard',
    wakeLock: 'WakeLock',
    windowControlsOverlay: 'WindowControlsOverlay',
    xr: 'XRSystem',
};

/**
 * 造一个 Chromium 形态的 Navigator.prototype。
 *
 * @param {object} values 已有的取值来源（userAgent / language / hardwareConcurrency 等）。
 *   没给的键会用一个 [object Xxx] 形态的占位对象兜底。
 * @param {(name:string, impl?:Function)=>Function} makeNativeFunction runner 里的原生函数伪装器
 * @returns {object} 可直接当 Object.create 的原型用
 */
function createNavigatorPrototype(values, makeNativeFunction) {
    const proto = {};

    for (const key of GETTER_KEYS) {
        let impl;
        if (Object.prototype.hasOwnProperty.call(values, key)) {
            const v = values[key];
            impl = () => v;
        } else if (OBJECT_TAGS[key]) {
            const tag = OBJECT_TAGS[key];
            const stub = {};
            Object.defineProperty(stub, Symbol.toStringTag, { value: tag });
            impl = () => stub;
        } else {
            impl = () => undefined;
        }
        // ★★★ 2026-08-16：getter 也必须走 makeNativeFunction。
        //   原来这里直接 `get = () => v`，于是
        //     Function.prototype.toString.call(取 userAgent 的 getter)
        //       真 Chrome : function get userAgent() { [native code] }
        //       我们      : () => v            ← 直接把箭头函数源码吐出来了
        //   这不是「值不像」，是【证明了这是合成环境】，FP 库标配就查它。
        //   本文件第 26 行早就写了「methods 用 makeNativeFunction 保持 [native code] 形态」，
        //   但 getter 一直漏在外面 —— 补上，命名按真 Chrome 的 "get <key>" 形态。
        const get = makeNativeFunction(`get ${key}`, impl);
        // 真 Chromium 里这些是 enumerable + configurable 的访问器
        Object.defineProperty(proto, key, { get, enumerable: true, configurable: true });
    }

    for (const key of METHOD_KEYS) {
        const impl = values.__methods && values.__methods[key];
        Object.defineProperty(proto, key, {
            value: makeNativeFunction(key, impl || (() => undefined)),
            writable: true, enumerable: true, configurable: true,
        });
    }

    // ★★★ 2026-08-16：constructor —— 这条是硬伤，不是可选项。
    //   【任何】原型对象都有 constructor，一个没有 constructor 的 Navigator.prototype
    //   在真浏览器里不可能存在。实测差集里它就摆在缺失清单第一个。
    //   形态要求（对着真 Chrome 逐项核过）：
    //     · 值是 Navigator 构造函数本身，Navigator.prototype === proto（自指）
    //     · Function.prototype.toString.call(Navigator) → "function Navigator() { [native code] }"
    //     · 【不可枚举】—— 和其它键不同，for...in / Object.keys 都不该看到它
    const Navigator = makeNativeFunction('Navigator');
    Object.defineProperty(Navigator, 'prototype', { value: proto, writable: false, enumerable: false, configurable: false });
    Object.defineProperty(proto, 'constructor', {
        value: Navigator, writable: true, enumerable: false, configurable: true,
    });

    Object.defineProperty(proto, Symbol.toStringTag, { value: 'Navigator', configurable: true });
    return proto;
}

module.exports = { createNavigatorPrototype, GETTER_KEYS, METHOD_KEYS, OBJECT_TAGS };
