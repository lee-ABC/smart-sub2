'use strict';
// 协议注册的指纹人格：**Windows 桌面 Chrome**，三层（TLS / HTTP / sentinel）同代同平台。
//
// ════════════════════════════════════════════════════════════════════════════
// 【这里曾经有 A / B 两个臂，2026-08-16 删掉了。删之前的状态记在这，别再走回头路】
//
//   A 臂 chromium_ios（曾经的默认）—— chrome_144 握手 + CriOS/iPhone UA
//     它有一条 100% 覆盖、零良性解释的内部矛盾：
//       UA 说   iPhone / iOS 26 / CriOS 150
//       握手说  桌面 Chrome（BoringSSL）
//       p 载荷  jsHeapSizeLimit=4395630592（performance.memory 是 Chromium 专属，
//               真 iPhone 上没有这个 API）
//     iOS 强制所有浏览器走 WebKit + 系统网络栈，所以这个组合真实世界里【不存在】。
//     UA 与 JA3/JA4 一 join 就能挑出来。
//     ⚠ 它曾有"3785 个浏览器号 0 封号"的背书 —— 但那是【浏览器路】的数据，
//       而且那批号是 Roxy 出的、和协议号本就不是一回事。用它当协议路的挡箭牌不成立。
//
//   B 臂 true_ios —— safari_ios_17_0 + Safari UA。平台补对了，但：
//     · DLL 里最新的 iOS 档只到 safari_ios_17_0，而 UA 要报 iOS 26 —— 差 3 年，
//       等于把"平台差"换成"版本差"。
//     · 【拿不到标尺】：我们没有一份真 iPhone Safari 打 OpenAI 的抓包，
//       25 个 p 槽全得靠推。而 C 臂的标尺可以自己造（本机就有真 Chrome）。
//
//   删除的判断：C 臂已经跑通（9 个号 / 60% 成功率 / 能测的 6/6 拿到 0 元 / 0 封禁），
//   留着 A 只会让"明文可识别的矛盾画像"有机会被误开。B 的路已被 C 覆盖且更差。
//   ⚠ 代价要说清楚：删掉之后就【没有回滚臂】了。C 当时只有 9 个号、约 8 小时数据，
//     T+72 那关还没过。真出问题只能靠 git 回滚这个提交，不是改个环境变量。
// ════════════════════════════════════════════════════════════════════════════
//
// 【为什么是 Windows 桌面 Chrome，而不是别的】
//   现行 TLS 档 chrome_144 当初就是拿【本机真 Windows Chrome 150.0.7871.187】
//   逐个扩展比对选定的（见 src/tlsProfile.js 顶部 2026-08-14 那段「扩展集合与真 Chrome 150 逐个相同」）——
//   ⚠ 2026-09-26 补注：那份比对只回答「像不像真 Chrome」。同日实测 chrome_144 打
//     auth.openai.com/log-in **0/6 全被 CF 挑战**，而最不像的 chrome_117 是 6/6。
//     两套判据会打架，改档前先想清楚要的是哪一个。
//   我们的握手【本来就是】一个真实的 Windows Chrome 握手，只是过去套了 iPhone 的 UA。
//   所以这不是"迁就 TLS"，是把 UA 改回它本来该配的身份，平台与版本【同时】自洽。
//   而且 jsHeapSizeLimit / deviceMemory / sec-ch-ua 在桌面 Chrome 上本来就该有，
//   B 臂里要删要猜的东西，这里全是正确值。

const ARM_C = 'win_chrome';

// ── 分布数据 ────────────────────────────────────────────────────────────────
//
// 【随机化：哪些必须随机、哪些必须钉死】
//   仓库踩过的坑（stripeFrameKey 写死 8094 / navigator 池只有 9 个取值）说明
//   "所有号共用一个值"会让按键聚类连成一条直线。所以设备侧必须逐号随机。
//   ★ 但【Chrome 大版本绝不能随机】—— 它被 TLS 档钉死了。
//     src/fingerprint.js 的 CHROME_VERSIONS 是 122~131，照搬就是
//     "UA 报 122~131、握手是 150"，正好把我们要修的版本 join 又造回来。
//   ★ 随机要按【真实分布】，不能均匀撒 —— 均匀撒会让我们在另一个方向上突出。
//
// 【下面三张表的权重来自实测，不是编的】
//   apify/fingerprint-generator 的贝叶斯网络（真实流量训练），采样条件
//   devices=desktop / os=windows / browser=chrome>=130 / locale=ja-JP，n=3000。
//   提取脚本 scratchpad/extract_dist.js，原始计数 scratchpad/real_dist.json。
//   ⚠ 我编的第一版错得不轻，记下来免得再犯：
//     · deviceMemory 只放了 4 和 8 —— 真实里 16 占 38.6%、32 占 20.3%，合计 58.9%，我一个没有
//     · 屏幕漏了 1280x720(8.4%) 和 1680x1050(4.1%)
//     · 核数给了 6 = 1/8 权重，真实只有 1.9%；20/24/32 一个都没有
//   ⚠ 这个包的 Chrome 大版本分布是 147 占 92% —— 那是它训练数据的年代，【不能用】。
//     分布数据可以借，版本号不行，借错了就是自己造一个版本 join。
const WIN_CHROME = {
    tlsClientIdentifier: 'chrome_144',     // 实测≈真 Chrome 150，别单独改
    chromeMajor: '150',                    // 【钉死】受 TLS 档约束

    // ★★★ UA 里的版本【必须】是 150.0.0.0，不能随机构建号。
    //   本机真 Chrome 150.0.7871.187 实测报的 UA 是 Chrome/150.0.0.0 ——
    //   Chrome 从 110 起做了 UA 缩减，次版本/构建/补丁号全被抹成 0。
    //   ⚠ 我最初写过 buildRange/patchRange 去随机构建号，生成 Chrome/150.0.7810.125，
    //     那种 UA 真实世界里【不存在】，100% 可识别 —— 随机过头本身就是特征。
    //   ⚠ 所以 UA 串在所有号之间【恒定】是【正确】的：真实用户的 Chrome 150 UA 全都一样。
    //     差异化必须来自设备侧（屏幕/核数/内存），不能来自版本号。
    uaVersion: '150.0.0.0',
    // 【实测】完整版本（走 UA-CH 高熵才会暴露）。本机真值。
    //   ⚠ 目前只有这一个真实构建号。想按号打散得先枚举出真实发布过的 150.x 构建号 ——
    //     在 7800~7900 里瞎随机会造出根本没发布过的版本，比恒定更糟。
    fullVersion: '150.0.7871.187',

    screens: [
        [{ w: 1920, h: 1080 }, 263], [{ w: 1536, h: 864 }, 231], [{ w: 1366, h: 768 }, 92],
        [{ w: 1280, h: 720 }, 84], [{ w: 2560, h: 1440 }, 77], [{ w: 1680, h: 1050 }, 41],
        [{ w: 1280, h: 800 }, 34], [{ w: 1440, h: 900 }, 18], [{ w: 1536, h: 960 }, 15],
        [{ w: 3440, h: 1440 }, 12], [{ w: 1600, h: 900 }, 12], [{ w: 1707, h: 1067 }, 11],
        [{ w: 3840, h: 2160 }, 9], [{ w: 1920, h: 1200 }, 9], [{ w: 2048, h: 1152 }, 7],
    ],
    dprs: [[1, 481], [1.25, 287], [1.5, 154], [2, 23], [1.75, 7], [2.5, 4]],
    platformVersions: [['15.0.0', 55], ['14.0.0', 25], ['10.0.0', 20]],

    // 【核+内存 联合分布】必须联合，不能各掷各的：真实机器里两者相关
    //   （8c/16G 最常见 10.8%，而 8c/4G 只有 2.3%）。
    //   ⚠ jsHeapSizeLimit 不是自由字段，Chrome 按可用内存推导，我们只有一个真值：
    //     本机 32G 机器实测 4395630592，与线上抓包的桌面样本一致 —— 说明它主要跟
    //     V8 的 64 位堆上限走，不随物理内存线性变。低内存机器报多少【没有实测】，
    //     4G 档用 2172649472（V8 低内存堆上限）属推断，是本表唯一还在推断的数。
    rigs: [
        [{ cores: 8, deviceMemory: 16 }, 108], [{ cores: 8, deviceMemory: 8 }, 94],
        [{ cores: 12, deviceMemory: 16 }, 83], [{ cores: 12, deviceMemory: 8 }, 75],
        [{ cores: 4, deviceMemory: 16 }, 65], [{ cores: 8, deviceMemory: 32 }, 55],
        [{ cores: 12, deviceMemory: 32 }, 46], [{ cores: 16, deviceMemory: 8 }, 45],
        [{ cores: 16, deviceMemory: 16 }, 44], [{ cores: 4, deviceMemory: 8 }, 42],
        [{ cores: 4, deviceMemory: 32 }, 32], [{ cores: 16, deviceMemory: 32 }, 27],
        [{ cores: 8, deviceMemory: 4 }, 23], [{ cores: 20, deviceMemory: 16 }, 19],
        [{ cores: 12, deviceMemory: 4 }, 17], [{ cores: 20, deviceMemory: 8 }, 14],
        [{ cores: 4, deviceMemory: 4 }, 13], [{ cores: 16, deviceMemory: 4 }, 11],
        [{ cores: 2, deviceMemory: 8 }, 10], [{ cores: 2, deviceMemory: 16 }, 10],
    ],
};

/** 按权重采样。表的形状是 [[值, 权重], ...] —— 权重不必归一。 */
function weightedPick(table) {
    let total = 0;
    for (const [, w] of table) total += w;
    let r = Math.random() * total;
    for (const [v, w] of table) { r -= w; if (r <= 0) return v; }
    return table[table.length - 1][0];
}

/** 4G 机器的 V8 堆上限【是推断值】，其余用本机实测值。见 rigs 上方注释。 */
function heapFor(deviceMemory) {
    return deviceMemory <= 4 ? 2172649472 : 4395630592;
}

/** 一套完整的桌面画像。每次调用都重新随机 —— 绝不能缓存复用。 */
function winChromeParts() {
    const screen = weightedPick(WIN_CHROME.screens);
    const r = weightedPick(WIN_CHROME.rigs);
    const rig = { ...r, jsHeapSizeLimit: heapFor(r.deviceMemory) };
    const major = WIN_CHROME.chromeMajor;
    return {
        full: WIN_CHROME.fullVersion, major, screen, rig,
        dpr: weightedPick(WIN_CHROME.dprs),
        platformVersion: weightedPick(WIN_CHROME.platformVersions),
        // UA 恒定是【对的】：真 Chrome 150 的 UA 全都是 Chrome/150.0.0.0（UA 缩减）
        userAgent: `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 `
            + `(KHTML, like Gecko) Chrome/${WIN_CHROME.uaVersion} Safari/537.36`,
        // sec-ch-ua 的版本必须与 UA 的大版本一致，否则又是一处自相矛盾
        secChUa: `"Not;A=Brand";v="8", "Chromium";v="${major}", "Google Chrome";v="${major}"`,
    };
}

/**
 * 把画像工厂产出的基础对象改造成 Windows 桌面 Chrome 人格。
 *
 * ⚠ 这里是【重建】不是【打补丁】：基础对象里的屏幕/核数/navigator 抽样池原本是 iOS 的，
 *   逐个改容易漏，漏一个就是一处矛盾。所以设备侧字段一次性全覆盖。
 * ⚠ 必须是画像工厂返回前的【最后一步】：任何在它之后再往 profile 上贴字段的代码，
 *   都可能把自洽性破坏掉。
 */
function applyPersona(profile) {
    if (!profile) return profile;
    const w = winChromeParts();
    const headers = {
        ...(profile.headers || {}),
        'user-agent': w.userAgent,
        // 桌面 Chrome【就该】发 UA-CH 三件套，而且版本要与 UA 一致
        'sec-ch-ua': w.secChUa,
        'sec-ch-ua-mobile': '?0',              // 桌面是 ?0，不是 iPhone 的 ?1
        'sec-ch-ua-platform': '"Windows"',
        'accept-encoding': 'gzip, deflate, br, zstd',   // Chromium 123+ 有 zstd
    };

    const sentinel = {
        ...(profile.sentinel || {}),
        userAgent: w.userAgent,
        secChUa: w.secChUa,
        secChUaMobile: '?0',
        secChUaPlatform: '"Windows"',
        screenWidth: w.screen.w,
        screenHeight: w.screen.h,
        devicePixelRatio: w.dpr,
        hardwareConcurrency: w.rig.cores,
        deviceMemory: w.rig.deviceMemory,
        jsHeapSizeLimit: w.rig.jsHeapSizeLimit,
        navigatorVendor: 'Google Inc.',
        navigatorPlatform: 'Win32',
        userAgentDataPlatform: 'Windows',
        browserFamily: 'chrome',
        chromeMajor: w.major,
        chromeFullVersion: w.full,
    };
    // ⚠ iOS 专用的抽样池必须去掉：navigator 原型上抽到 iOS 才有的键，而 UA 说 Windows，
    //   那是新造的矛盾。让 sentinelFingerprint 回落到桌面默认池。
    delete sentinel.navigatorProtoSamples;
    delete sentinel.windowKeySamples;

    return { ...profile, userAgent: w.userAgent, headers, sentinel, persona: ARM_C };
}

/** 协议链路该用哪个 TLS 档。显式声明，别靠"恰好和默认相同"。 */
function personaTlsIdentifier() {
    return WIN_CHROME.tlsClientIdentifier;
}

/** 一行人话，给日志用 —— 每次注册都该打出来，否则数据没法归因。 */
function describe() {
    return `Win桌面Chrome（${WIN_CHROME.tlsClientIdentifier} ≈ 真 Chrome ${WIN_CHROME.chromeMajor}，`
        + 'UA/握手/UA-CH 三层同代，设备侧按真实分布随机）';
}

module.exports = {
    ARM_C, applyPersona, personaTlsIdentifier, describe,
    WIN_CHROME, winChromeParts, weightedPick,
};
