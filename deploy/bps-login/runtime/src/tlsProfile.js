'use strict';
// TLS 指纹档案的选择。
//
// 【问题】（2026-08-13）
//   我们的 UA 声称 CriOS/150，而 TLS 握手用的是 `chrome_131` 的档案 —— 差 19 个大版本。
//   JA3/JA4 指纹库是【带版本标签】的：UA 说 150、握手落在 131 上，是 CF / Akamai
//   最标准的一条 join 规则，而且覆盖率天然 100%（每个连接都有，没有任何良性解释）。
//
//   ★★ 2026-08-16 更正：这里原来写「参照系 Roxy 是真 Chromium，它的握手与它自己的
//      UA 是自洽的。我们不是。」—— 后半句对，前半句是错的，而且会把人带沟里。
//      实测浏览器路（chrome-launcher 起的【真 Chrome】，5000+ 账号 0 封号、稳定拿 0 元）
//      发出去的 UA 是：
//        Mozilla/5.0 (iPhone; CPU iPhone OS 26_2 like Mac OS X) AppleWebKit/605.1.15
//        (KHTML, like Gecko) CriOS/150.0.7871.61 Mobile/15E148 Safari/537.36
//      —— 桌面 Chromium 内核 + iPhone UA。iOS 上所有浏览器都被强制走 WebKit/系统网络栈，
//      真 CriOS 的握手长得像 Safari；所以【浏览器路的握手和它的 UA 同样不自洽】。
//      两条路有完全一样的平台矛盾，而浏览器那条毫无问题 ⇒ 这个矛盾实测无害。
//
//      ⚠ 别再去追 safari_ios_* 档（DLL 里有 safari_ios_15_5/15_6/16_0/17_0）。
//        平台补对了，版本差反而从 6 年拉到「UA 说 iOS 26、握手是 iOS 17」的 3 年，
//        换来的是一个已被证明无害的问题。这条路 2026-08-16 试过，结论是不值得。
//
// 【为什么之前卡在 131】
//   node-tls-client 的 JS enum 封顶在 chrome_131。但 clientIdentifier 只是一个字符串
//   传给 Go 引擎 —— vendor/tls-client-64.dll 里实际有到 **chrome_146**
//   （实测字符串表里有 chrome_133 / chrome_144 / chrome_146）。
//   绕过 enum 直接传字符串即可，实测 chrome_146 请求正常（HTTP 200，916KB）。
//
// 【为什么生产路径不动】
//   chrome_131 在 accountDetector / rtProtocolClient / sessionTokenRefresh 等 6 处硬编码，
//   那些链路上有 5000+ 个 0 封号的账号在跑。换掉它们是【没有证据支持的高风险变更】——
//   要换也该单独灰度，不能夹带在协议注册的修复里。
//
// ⚠ 诚实说明：换 TLS 档是【更接近真相】，但【没有证据证明它是封号原因】。
//   当时的依据是"2026-08-13 修好 turnstile/so/sec-fetch/头序之后账号照样被停用"。
//   ★★ 2026-08-25 更新：那次观测样本是 **n=3**（3 个号死了 2 个）。
//      现在 `tools/survival_report.js --by=method` 是 **413 个协议号、存活 100%、被封 0**，
//      "账号照样被停用"这个前提【已经不成立】。别再拿它当论据推导别的结论。
//      （但下面这句仍然成立：换档只是消除一个已知的不一致，不该被当作解药。）
//   ⚠ 上面这段写于选 chrome_146 的时期；实际默认档后来改成了 chrome_144，见下。

const cfg = require('./config');

// 生产链路一直在用的。5000+ 账号、0 封号的那批就是这个。不要改。
const PRODUCTION = 'chrome_131';

// 协议注册链路用的。可用 config 的 protocolTlsClientIdentifier 覆盖。
// DLL 里可选：chrome_103…131 / chrome_133 / chrome_144 / chrome_146（封顶 146，没有 150）
//
// ★★ 2026-08-14 从 chrome_146 改成 chrome_144，依据是【实测】不是版本号。
//   当初选 146 的理由是"UA 报 CriOS/150，就选 DLL 里最高的那个"——
//   **版本号接近 ≠ 指纹接近**，这个推断是错的。
//
//   实测方法：本地起 TCP 前置，把 ClientHello 原始字节截下来解扩展列表，
//   我们各档与【本机真 Chrome 150.0.7871.187】打同一个靶子（脚本见
//   scratchpad/stable.js，做法：net 前置 → tls 终止 → h2c 内层）。
//   ⚠ 只能比【集合】不能比顺序：Chrome 从 110 起每次连接都打乱扩展顺序，
//     所以 JA3（顺序敏感）本来就不该稳定，那不是缺陷。
//
//   结果（各 5 次连接、集合 5/5 恒定，真 Chrome 10 次 10/10 恒定）：
//     真 Chrome 150   扩展 17 个，含 application_settings_new(17613)，不发 0xca34
//     chrome_146      **5/5 都多发一个 0xca34(51764)** —— 真 Chrome 0/10
//     chrome_144      **扩展集合与真 Chrome 150 逐个相同** ✔  ← 选它
//     chrome_133      集合相同，但 ALPN 多报 h3（真 Chrome 只报 h2/http1.1）
//     chrome_131      发旧版 application_settings(17513)，真 Chrome 用新版
//
//   也就是说：协议链路此前【每一个连接】都在发一个真 Chrome 从不发的 TLS 扩展，
//   覆盖率 100%、零良性解释。这与头线序是同一类问题，只是在更下面一层。
//
// ⚠ 同样的诚实说明仍然成立：这只是"消除一个已知的不一致"，
//   没有证据证明它就是封号原因。别把它当解药。
const PROTOCOL_DEFAULT = 'chrome_144';

function protocolClientIdentifier() {
    const override = String(cfg.protocolTlsClientIdentifier || '').trim();
    if (override) return override;
    // ★ TLS 档由人格统一给出（src/personaArm.js），不在这里各说各的。
    //   ⚠ 【不能】单独把这里改成别的档：TLS 和 UA/UA-CH 必须同代同平台，
    //     只换一层是制造一个新的、更罕见的矛盾。要换就三层一起换，改 personaArm。
    const persona = require('./personaArm').personaTlsIdentifier();
    return persona || PROTOCOL_DEFAULT;
}

module.exports = { PRODUCTION, PROTOCOL_DEFAULT, protocolClientIdentifier };

// ─────────────────────────────────────────────────────────────────────────────
// ★★ 2026-09-26：Cloudflare 开始按 TLS 指纹下挑战 —— 上面那份「越接近真 Chrome 越好」的
//    结论，在【能不能过 CF】这件事上**不成立了**。
//
//    实测（本机直连、IP 固定、唯一变量是档位，打 auth.openai.com/log-in，每档 6 次；
//    DLL-only 的几档用字符串直传，绕 enum，方法同上面那段）：
//        chrome_131  过 0/6      chrome_133  过 0/6
//        chrome_144  过 0/6      chrome_146  过 1/6     ← 08-14 挑出来的那几档，今天全军覆没
//        chrome_117  过 6/6
//    生产侧同一天的账（logs/pipeline-console-2026-09-26.log）：chrome_131 的 252 个会话里
//    233 次吃 CF 403（98%）；换 chrome_117 之后预热 4/4 都是 [200,200]。
//    teamctl（curl_cffi，另一套实现、另一个仓库）同日同样的形状：chrome145 过 0/8、chrome142 过 8/8。
//
// ⚠⚠ 这里有一个**没法两全**的取舍，必须摆明：
//      · chrome_144：扩展集合与真 Chrome 150 逐个相同（08-14 实测），但今天 100% 被 CF 挑战；
//      · chrome_117：能过 CF，但比真 Chrome 老 33 代，application_settings 之类的码点必然是旧的
//        —— 也就是说它在「像不像真浏览器」这件事上**比 144 差**。
//    我们选了前者，理由只有一个：过不去时一个 RT 都取不到。
//    ★ 别把它读成「117 更安全」。它只是**今天的 CF 规则**放行的那一档。规则一变就要重新量，
//      量法见上面 08-14 那段（本机直连、固定 IP、只换这一个变量、每档打 6 次）。
//
// ★ 为什么这里返回**字符串**而不是 tls.ClientIdentifier[名字]：JS enum 封顶在 chrome_131，
//   查表会把 chrome_133/144/146 静默降级成别的档 —— 那正是上面那段特意绕开的坑。
const RT_DEFAULT = 'chrome_117';

// 量过确实会被 CF 挑战的，配置里写了也挡掉：一个手滑不该把整条链打死。
const RT_KNOWN_CHALLENGED = new Set(['chrome_131', 'chrome_131_psk', 'chrome_133', 'chrome_144', 'chrome_124', 'chrome_120', 'chrome_116_PSK_PQ']);

/**
 * 生产 RT / 探活 / sentinel / 换 session 这几条链路装哪个档。回**字符串**。
 * 可用 config 的 rtTlsClientIdentifier 覆盖（⚠ 该键需在 src/config.js 的导出里放行才生效）。
 */
function rtClientIdentifier() {
    const override = String(cfg.rtTlsClientIdentifier || '').trim();
    if (override && !RT_KNOWN_CHALLENGED.has(override)) return override;
    return RT_DEFAULT;
}

module.exports.RT_DEFAULT = RT_DEFAULT;
module.exports.RT_KNOWN_CHALLENGED = RT_KNOWN_CHALLENGED;
module.exports.rtClientIdentifier = rtClientIdentifier;
