const path = require('path');
const fs = require('fs');
const { normalizeEmailPick } = require('./emailSelection');

const configPath = path.join(__dirname, '..', 'config.json');

// 读取配置文件
function loadConfig() {
    if (!fs.existsSync(configPath)) {
        console.error(`[Config] 配置文件不存在: ${configPath}`);
        return {};
    }

    try {
        const content = fs.readFileSync(configPath, 'utf8');
        return JSON.parse(content);
    } catch (error) {
        console.error('[Config] 解析配置文件失败:', error.message);
        return {};
    }
}

const config = loadConfig();

function boundedInteger(value, fallback, minimum, maximum) {
    const parsed = Number(value);
    if (!Number.isInteger(parsed)) return fallback;
    return Math.min(maximum, Math.max(minimum, parsed));
}

const foargeDispatchBase = boundedInteger(config.foargeDispatchBase, 12, 1, 64);
const foargeDispatchMax = Math.max(
    foargeDispatchBase,
    boundedInteger(config.foargeDispatchMax, 16, 1, 64),
);
const foargeSubmitReserve = boundedInteger(
    config.foargeSubmitReserve,
    Math.min(4, foargeDispatchBase - 1),
    0,
    foargeDispatchBase - 1,
);
const foargeWriteAgentMaxSockets = Math.max(
    foargeDispatchMax,
    boundedInteger(config.foargeWriteAgentMaxSockets, 16, 1, 64),
);
const foargeDispatchMode = ['create_first', 'link_first'].includes(config.foargeDispatchMode)
    ? config.foargeDispatchMode
    : (config.foargeCreateBeforeUpi === false ? 'link_first' : 'create_first');

// ── 住宅代理供应商（方言层）────────────────────────────────────────────────
// 一家供应商 = 一份凭据模板，地区/会话在运行时派生（见 src/proxyDialects）。
// 各家模板同时存放在 config.proxyVendors 里，切换只改 proxyVendor 一个字段，来回切不用重填。
// ★ proxyVendors 缺失时 resolveFlatProxy 会回退到旧的扁平 proxy* 字段 → 老配置行为零变化。
const { resolveFlatProxy, normalizeLaneMap, normalizeTtlMap } = require('./proxyDialects/resolveTemplate');
const PROXY_VENDOR_NAMES = require('./proxyDialects').NAMES;

const proxyVendor = PROXY_VENDOR_NAMES.includes(String(config.proxyVendor || '').toLowerCase())
    ? String(config.proxyVendor).toLowerCase()
    : '';   // 空 = 未显式声明，交给方言层从凭据自身嗅探
const proxyVendors = (config.proxyVendors && typeof config.proxyVendors === 'object' && !Array.isArray(config.proxyVendors))
    ? config.proxyVendors
    : {};
const proxyTemplate = resolveFlatProxy(proxyVendors, proxyVendor, config);

/* ── B8-5：Statsig 重放基准【依赖】节奏闸门，这条依赖写成可执行的一条规则 ──────
 *
 * 【依赖是什么】㉔ chatgpt Statsig 那一族现在按 batch.atMs 真的等（B8-5），
 *   一轮多出来的墙钟就是分批计划的跨度（模拟 2 万次：min 928 / p50 1681 /
 *   max 2384ms；扣掉本来就要花的那几发 HTTP，实际增量约 1.3 秒）。
 *   它【落在 backendAnon→providers 这一段里】（㉔ 的调用点在三连 backend-anon
 *   之后、providers 之前）。B7 之后这一段走 paceSince：目标间隔 target 从浏览器
 *   分布里采，实际再等 wait = max(0, target − already)，所以多花的时间会先去
 *   顶掉 wait —— 是【部分扣除】。
 *
 * ⚠⚠ 【这里必须更正一个流传中的说法】"净成本 0"是【过度断言】，实测不成立：
 *   这一段的 already（本段里已经花掉的时间）实测 n=52 committed
 *   （logs/protocol-flow，口径：三连最后一发 /backend-anon/checkout_pricing_config
 *     到 providers 之前最后一发 /ces/*，与节奏原语无关所以 B7 前的语料也算数）
 *     min 3.46 / p25 6.89 / p50 7.60 / p75 8.40 / max 13.96 秒；
 *   而 target 的采样上界只有 9.28 秒（knots 0.22/3.25/5.11/6.50/9.28）。
 *   ⇒ P(target > already=7.60s) = 15.1%，E[max(0,target−already)] = 0.127s；
 *     再加 1.3 秒之后 E 掉到 0.007s ——【净增约 1.18 秒/轮】，不是 0。
 *     只有 already 落在低端（3.46s 那一档）时才扣得掉大半（1.3s 里扣 0.82s）。
 *   复跑：node tools/telemetry_hop_pacing.js 看 ① 的 backendAnon→providers；
 *        already 的下界按上面那个口径自己从抓包里减。
 *
 * 【那为什么还要有这条开关级依赖】因为"部分扣除"是【只有节奏闸门开着才存在】的：
 *   闸门一关，wait 恒为 0，这一段连那 0.127 秒都不会扣；更要紧的是这一段
 *   从此没有 target、没有 ceilingMs、没有 overshoot 记账 —— 多出来的墙钟
 *   在任何一把尺子上都看不见。所以：关 ㉔ 可以（不发这一族），
 *   单独关 paceSince 不可以（㉔ 会被这道闸拦下）。
 *
 * ⚠ 拦下的方式刻意是"㉔ 整块不发 + 记账 + 大声打日志"，不是静默降级也不是抛异常：
 *   · 静默降级会让"多付了 1 秒"这件事看不见 —— 而依赖存在的全部理由就是这个成本；
 *   · 抛异常会因为一个纯配置矛盾毁掉一轮注册，代价与收益不成比例。
 *
 * ⚠ 纯函数：不读 config、不读时钟、不打日志。probe 侧要把 CLI 的 --chatgpt-statsig
 *   一起折进来（CLI 能绕过 config），所以它【也】调这一个函数，而不是另写一份判断。
 *
 * @param {{paceSinceEnabled?:boolean, statsigRequested?:boolean}} o
 * @returns {{paceSinceEnabled:boolean, statsigRequested:boolean, enabled:boolean,
 *            blocked:boolean, code:string, reason:string}}
 */
function resolveStatsigReplayGate(o = {}) {
    // 默认值不对称是刻意的：节奏闸门默认【开】（关它要显式写 false），
    // ㉔ 默认【关】（开它要显式写 true）—— 与两者各自的灰度状态一致。
    const paceSinceEnabled = o.paceSinceEnabled !== false;
    const statsigRequested = o.statsigRequested === true;
    const blocked = statsigRequested && !paceSinceEnabled;
    return Object.freeze({
        paceSinceEnabled,
        statsigRequested,
        enabled: statsigRequested && paceSinceEnabled,
        blocked,
        code: blocked ? 'STATSIG_REPLAY_NEEDS_PACE_SINCE' : '',
        reason: blocked
            ? 'protocolTelemetryChatgptStatsigEnabled 依赖 protocolPaceSinceEnabled：'
                + '㉔ 现在按 batch.atMs 重放（计划跨度 p50 约 1.7s）。节奏闸门一关，'
                + '这段墙钟一点都扣不掉，而且 backendAnon→providers 段再没有 target/上界/超标记账，'
                + '多出来的时间在任何一把尺子上都看不见。要回滚节奏闸门，请连同 ㉔ 一起关。'
            : '',
    });
}

// ★ 两个开关在这里【一次性】解算完，导出的就是解算后的真值 ——
//   pipeline / GUI 读到的与 probe 实际执行的是同一个结论。
const statsigReplayGate = resolveStatsigReplayGate({
    paceSinceEnabled: config.protocolPaceSinceEnabled !== false,
    statsigRequested: config.protocolTelemetryChatgptStatsigEnabled === true,
});

module.exports = {
    stateBackend: config.stateBackend === 'sqlite' ? 'sqlite' : 'json',
    stateDatabasePath: path.resolve(config.stateDatabasePath || path.join(__dirname, '..', 'pipeline_state.sqlite')),
    apiLegacyStatusEnabled: config.apiLegacyStatusEnabled !== false,
    apiV2AccountPageSize: boundedInteger(config.apiV2AccountPageSize, 50, 10, 100),
    // 注册并发是软目标，不是代码级硬上限。运行时会在 1..target 之间按依赖健康度伸缩。
    registerConcurrency: Math.max(1, parseInt(config.registerConcurrency, 10) || 1),
    registerAdaptiveConcurrency: config.registerAdaptiveConcurrency === true,
    registerMinConcurrency: Math.max(1, parseInt(config.registerMinConcurrency, 10) || 1),
    registerBrowserConcurrency: parseInt(config.registerBrowserConcurrency, 10) >= 0
        ? (parseInt(config.registerBrowserConcurrency, 10) === 0 ? 0 : Math.max(1, parseInt(config.registerBrowserConcurrency, 10)))
        : 0, // 0=按目标并发自动计算
    emailPreflightAutoReplenish: config.emailPreflightAutoReplenish !== false,
    emailPreflightConcurrency: Math.max(1, parseInt(config.emailPreflightConcurrency, 10) || 5),
    emailPreflightIntervalMs: Math.max(1000, parseInt(config.emailPreflightIntervalMs, 10) || 5000),
    registerRoxyConcurrency: parseInt(config.registerRoxyConcurrency, 10) >= 0 ? parseInt(config.registerRoxyConcurrency, 10) : 0, // 0=按目标并发自动计算
    registerMailConcurrency: parseInt(config.registerMailConcurrency, 10) >= 0 ? parseInt(config.registerMailConcurrency, 10) : 0,
    registerSmsConcurrency: parseInt(config.registerSmsConcurrency, 10) >= 0 ? parseInt(config.registerSmsConcurrency, 10) : 0,
    registerCircuitOpenAfter: parseInt(config.registerCircuitOpenAfter, 10) > 0 ? parseInt(config.registerCircuitOpenAfter, 10) : 3,
    registerCircuitOpenMs: parseInt(config.registerCircuitOpenMs, 10) > 0 ? parseInt(config.registerCircuitOpenMs, 10) : 60000,
    registerCircuitCloseAfter: parseInt(config.registerCircuitCloseAfter, 10) > 0 ? parseInt(config.registerCircuitCloseAfter, 10) : 3,
    registerAttemptTimeoutMs: parseInt(config.registerAttemptTimeoutMs, 10) > 0 ? parseInt(config.registerAttemptTimeoutMs, 10) : 8 * 60 * 1000,
    registerLeaseTimeoutMs: parseInt(config.registerLeaseTimeoutMs, 10) > 0 ? parseInt(config.registerLeaseTimeoutMs, 10) : 10 * 60 * 1000,
    registerMaxRetries: parseInt(config.registerMaxRetries, 10) >= 0 ? parseInt(config.registerMaxRetries, 10) : 6,
    registerEventLoopLagLimitMs: parseInt(config.registerEventLoopLagLimitMs, 10) > 0 ? parseInt(config.registerEventLoopLagLimitMs, 10) : 250,
    registerMemoryPressurePercent: parseInt(config.registerMemoryPressurePercent, 10) > 0 ? parseInt(config.registerMemoryPressurePercent, 10) : 90,
    // ★ 2026-08-04 P4 实测：browserService 的两个页面轮询循环
    //   （completeProfile / oauthLoginAndAuthorize 的后续步骤循环）是「先无条件 SLEEP
    //   pollIntervalMs，再去读页面」。这两个循环覆盖的两个阶段
    //   （identifier_submitted→otp_verified 34.4% + otp_verified→profile_submitted 36.0%）
    //   合计吃掉注册总墙钟的 70.4%，而它们的 p05 几乎等于 p50
    //   （32.5s vs 35.1s）—— 典型的"由固定等待主导、与实际情况无关"的指纹。
    //   24 份 registration-flow 捕获实测：整条注册链路有 44%（单次中位 41%）的墙钟
    //   处于"连续 ≥2.5s 零网络活动"状态。
    //
    //   原来这两个值是 browserService 里的硬编码字面量（3000 / 20），没有任何配置路径。
    //   这里把它们提出来，**默认值与原硬编码完全一致，行为零变化**，目的是让调优
    //   不需要改代码。
    //
    //   ⚠ 两个值必须一起调：总耐心窗口 ≈ maxRounds × pollIntervalMs。只把间隔从
    //   3000 调到 1000 而不动 maxRounds=20，窗口会从 60s 缩到 20s，本来能等到的页面
    //   会被判死。例如想快而不减耐心：pollIntervalMs=1000 + maxRounds=60。
    //   调完看 [Browser] 那行 "轮询 N 轮 / 空等 Xs" 汇总日志验证效果。
    registerPollIntervalMs: parseInt(config.registerPollIntervalMs, 10) >= 0 ? parseInt(config.registerPollIntervalMs, 10) : 3000,
    registerMaxRounds: parseInt(config.registerMaxRounds, 10) > 0 ? parseInt(config.registerMaxRounds, 10) : 20,
    // ★ 自适应轮询：把上面那句「闭眼睡满 registerPollIntervalMs 再读页面」换成
    //   「每 registerPollQuietStepMs 瞄一眼，连续两次页面签名一致就认为稳定、立刻往下走」。
    //   **耐心上限不变**：仍然最多等 registerPollIntervalMs，只是不再盲等满。
    //   最短等待 = 2 × step（默认 700ms），保证至少观察到一次"稳定"。
    //
    //   默认 false = 行为与改动前逐字节一致。开关走 liveConfig 热读，
    //   改 config.json 秒级生效、秒级回退，**不用重启**。
    //   开了之后看 /api/v2/health 的 browserPoll.*.savedPct 验证真实收益
    //   （关着时该值恒为 0，这本身就是"零变化"的自证）。
    registerAdaptivePolling: config.registerAdaptivePolling === true,
    registerPollQuietStepMs: parseInt(config.registerPollQuietStepMs, 10) > 0 ? parseInt(config.registerPollQuietStepMs, 10) : 350,
    // ★★★ 2026-08-11 对账恢复路径的推进观察窗口（emailRegistrationReconciler）。
    //   轮数保持原硬编码 30 不动；墙钟耐心是【另加】的一条线，两者取长。
    //   之所以要墙钟线：代理劣化时提交后的文档响应实测 24.2s，而 30×500ms≈15s 盖不住，
    //   飞行中的 evaluate 撞上导航 commit 被销毁 → page_unavailable → 无预算对账循环。
    //   ⚠ 只许往长了调。调短 = 拿耐心换速度，慢号直接变废号（0e266f7/9673275 的教训）。
    registerReconcileAdvanceRounds: parseInt(config.registerReconcileAdvanceRounds, 10) > 0 ? parseInt(config.registerReconcileAdvanceRounds, 10) : 30,
    //   ★ 2026-08-14 我一度把它从 45000 改成 120000，理由是「authorize 实测 43.1~130.7s，
    //     45 秒盖不住」。**那个理由是错的，已改回 45000。**
    //     43~131 秒是从【第一次提交】算起的，而第一次提交落进了 ?email= 死胡同页；
    //     真正走通的是【重来那一次】—— 从那次点击到 authorize 只要 10.3 ~ 27.7 秒
    //     （5 份成功抓包：10.3 / 11.6 / 11.9 / 10.3 / 27.7）。45 秒本来就够。
    //     在一个永远不会动的页面上从 45 秒延到 120 秒，只是让必死的等待更长。
    registerReconcileAdvanceMs: parseInt(config.registerReconcileAdvanceMs, 10) >= 0 ? parseInt(config.registerReconcileAdvanceMs, 10) : 45000,
    // ★★★ 2026-08-14 首次注册路径（browserService.submitSignupEmail）提交后的耐心预算。
    //   上面那三条是 08-11 给【对账恢复】路径补的；主路径当时没动，一直是
    //   3s + CF + 2s + 7×2s ≈ 19 秒。
    //
    //   【为什么必须加长 —— 数据在这里】
    //   把 logs/registration-flow/ 的抓包按「提交后那次 GET /auth/login?email=… 的
    //   文档导航」对齐（该导航 38 份实证 navigation=true / resourceType=document，
    //   成功与失败样本里都有，是这个表单正常的提交方式）：
    //       文档回来       成功样本 n=5   min 13.7s  p50 40.4s  max 82.6s
    //       走到 authorize                    43.1s ~ 130.7s
    //   19 秒连 p50 都盖不住。到点后主路径会 reload+重填+重点，
    //   把仍在飞的那次导航亲手打断，下一轮再从头等一次几十秒 —— 三轮必死。
    //   实拍表现：登录页渲染完、输入框空的、URL 挂着 ?email=、然后什么都不发生。
    //
    //   ⚠ 与 registerReconcileAdvanceMs 同一条红线：只许往长了调。
    //     调短 = 拿耐心换速度，慢号直接变废号（0e266f7 / 9673275 的教训）。
    //   Total 是跨 3 轮共享的总预算，防止把 8 分钟的 attempt 预算全耗在这一步。
    // ★★★ 2026-08-15 改动一：点提交之前等【JS 接管提交按钮】的预算。
    //   老代码硬编码 15 秒，而实测这个页面自己加载要 13.7~82.6 秒 ——
    //   15 秒等于赌它在前 15 秒内加载完，埋点实测约一半赌输（35 样本里 17 次没接管）。
    //   没接管就点 → 退化成浏览器原生表单 GET → 掉进 `?email=` 死胡同页（76%）。
    //   接管了再点，死胡同率降到 11%。
    //   registerHydrateReloadWaitMs 是【重载之后】的第二次等待：静态资源已进缓存，快得多，
    //   所以预算可以短。两次都等不到就照点（兜底 = 改动前的行为，下限不会更糟）。
    // ★★★ 2026-08-15 等【验证码输入框】渲染出来的预算（enterSmsCode）。
    //   老代码是固定 SLEEP(2000) 然后扫一遍 —— 和邮箱那边同一个错误，只是晚了一页：
    //   拿一个拍脑袋的固定时长代替"是否就绪"的判断。页面资源实测要 13.7~82.6 秒。
    //   扫不到就"退回键盘盲打"，把码敲进空气里再提交 —— 代价是【烧掉一个验证码】
    //   （OpenAI 记一次失败尝试，离 max_check_attempts 更近一步）。
    //   改成轮询后：找到就立刻走（比原来干等 2 秒还快），找不到才等满预算。
    // ★★★★★ 2026-08-15 恢复机制总开关。**默认关**，是用户明确的决定，别随手打开。
    //
    //   【关掉的依据】
    //     · 实测成功率 14%（4/29；分日 08-12 8%、08-13 0%、08-14 33%），
    //       而同期新注册一次成的成功率是 89%；
    //     · 它占的是【和新注册同一批】的并发槽位而且被优先认领 ——
    //       2026-08-15 实测全天「新注册认领 0 次 / 恢复认领 12 次」，5 个槽位全被吃光；
    //     · 积压数 ≥ 并发数时 registrationAdmission 会把新号创建整个封死；
    //     · 本轮排查出的缺陷绝大多数来自它：误把空白文档当"已推进"、
    //       第二套重复的邮箱提交实现（没有死胡同检测）、在验证码页上反复烧码。
    //
    //   【关掉之后的行为】阶段 ≥ identifier_submitted 的 attempt 一律直接判死，
    //   并且把它的邮箱【禁用】而不是放回池子 —— 那个邮箱已经提交给 OpenAI 了，
    //   远端可能有半成品账号，再抽回去用会撞 "account already exists"。
    //
    //   ⚠ 这【不影响】已经拿到 accessToken 的号：那种走 commitOnly 分支，
    //     跟远端对账是两回事，照常落库。
    //   ⚠ 重新打开之前，先把恢复路径本身修好（见 docs 与本轮的红线测试），
    //     否则会原样退回"僵尸吃满槽位、新注册 0 个"的状态。
    registerRecoveryEnabled: config.registerRecoveryEnabled === true,
    registerOtpFieldWaitMs: parseInt(config.registerOtpFieldWaitMs, 10) > 0 ? parseInt(config.registerOtpFieldWaitMs, 10) : 30000,
    registerHydrateWaitMs: parseInt(config.registerHydrateWaitMs, 10) > 0 ? parseInt(config.registerHydrateWaitMs, 10) : 45000,
    registerHydrateReloadWaitMs: parseInt(config.registerHydrateReloadWaitMs, 10) > 0 ? parseInt(config.registerHydrateReloadWaitMs, 10) : 25000,
    registerEmailAdvanceMs: parseInt(config.registerEmailAdvanceMs, 10) > 0 ? parseInt(config.registerEmailAdvanceMs, 10) : 150000,
    registerEmailAdvanceRetryMs: parseInt(config.registerEmailAdvanceRetryMs, 10) > 0 ? parseInt(config.registerEmailAdvanceRetryMs, 10) : 60000,
    registerEmailAdvanceTotalMs: parseInt(config.registerEmailAdvanceTotalMs, 10) > 0 ? parseInt(config.registerEmailAdvanceTotalMs, 10) : 180000,
    // ★★★ 2026-08-11 对账重试的两条上界（详见 registerErrors.decideRegistrationRetry）。
    //   改动前：reconciliation 走 persistentRetry，retryCount 不自增、重试预算恒为真，
    //   registerMaxRetries 被完全绕开，唯一出口是撞上 business_terminal/resource_invalid。
    //   实拍后果：reg_c21c62cf 从 08-08 12:51 活到 08-11 04:00（63.1 小时 / 对账 26 轮 /
    //   成号 0 个），而 RECONCILING 又在 registrationAdmission 的准入阻塞集合里 ——
    //   一个僵尸就把整个注册区块的新号创建封死。
    //   ⚠ 两条闸缺一不可：次数闸挡不住「每轮间隔 70s、一天 1200 轮」的慢性泄漏，
    //     年龄闸才是兜底。设成 0 = 关掉该条闸（回到改动前的无限对账）。
    //
    //   ★★★ 默认值是【实测标定】出来的，不是拍脑袋。口径：logs/registration-events-2026-0*.jsonl
    //     里 4789 个最终 committed 的 attempt。别凭感觉调小，会静默掉号。
    //       · 对账轮次分布 p50=0 p95=1 p99=5 max=15
    //         → 上限 8 轮误杀 0.29%、10 轮 0.15%、12 轮 0.08%、【16 轮 0.00%】
    //         取 16：已知样本零误杀，且连续运行时 16 轮 × 约 70s 退避 ≈ 19 分钟就能埋掉
    //         快速空转的僵尸（实拍 reg_c21c62cf 烧到 26 轮）。这条是主力闸。
    //       · 发生过对账且最终成功的号占 9.58%，它们最后一次对账时的年龄
    //         p50=0.09h p95=1.39h p99=17.35h max=61.99h
    //         → 上限 6h 误杀 0.27%、12h 0.23%、【24h 0.08%】、48h 0.02%
    //         取 24h：只当慢性泄漏的兜底（次数少但拖很久的那种），正常连续运行时
    //         几乎永远轮不到它开火 —— 主力是上面那条次数闸。
    registerMaxReconciliationRetries: parseInt(config.registerMaxReconciliationRetries, 10) >= 0 ? parseInt(config.registerMaxReconciliationRetries, 10) : 16,
    registerMaxAttemptAgeMs: parseInt(config.registerMaxAttemptAgeMs, 10) >= 0 ? parseInt(config.registerMaxAttemptAgeMs, 10) : 24 * 60 * 60 * 1000,
    upgradeConcurrency: parseInt(config.upgradeConcurrency, 10) || 1,

    // ── 协议注册区块（2026-08-16）──────────────────────────────────────────────
    // 产出的号并入同一个账号库，靠 registerMethod:'protocol' 区分，不做隔离。
    // 单开区块只是为了独立的并发/开关与独立统计（协议的 CF 403 特性与浏览器路完全不同）。
    protocolEnabled: config.protocolEnabled !== false,             // 总开关，默认开
    // ⚠ 使用者明确要求【不设上限】—— 这里不做 Math.min 封顶。
    //   并发越高消耗邮箱/代理越快，而邮箱自动补货已被关掉；池空时区块会自己停并记日志。
    protocolConcurrency: Math.max(1, parseInt(config.protocolConcurrency, 10) || 3),
    // ★ CF 403 的【原地重发】上限，按跳分开 —— 三跳耗尽的代价差两个数量级：
    //     ① 登录页  耗尽后落到「换出口」，几乎免费（2026-08-16 实测 2 次耗尽 0 失败）
    //     ⑤ authorize 耗尽直接 throw，整轮死。实测第1发 0/9、第2发 8/9 ⇒ 老的 2 只买到
    //       一次真正可用的尝试、余量为 0（配对检验 p=0.008）
    //     ⑪ callback / ⑫ session 耗尽 = 孤儿号（⑩ 已建号）+ 邮箱被降级 + 白跑一整轮
    //   走 config 而不是 env：探针每个号开头都 liveConfig.refreshIfChanged()，
    //   所以改 config.json 下一个号就生效、不用重启；env 是进程级的，界面上也够不着。
    //   （env PROBE_CF_INPLACE_MAX 仍保留，用于 CLI 单跑时整体 override。）
    protocolCfResendLogin: Math.max(0, parseInt(config.protocolCfResendLogin, 10) >= 0 ? parseInt(config.protocolCfResendLogin, 10) : 10),
    protocolCfResendAuthorize: Math.max(0, parseInt(config.protocolCfResendAuthorize, 10) >= 0 ? parseInt(config.protocolCfResendAuthorize, 10) : 10),
    protocolCfResendCallback: Math.max(0, parseInt(config.protocolCfResendCallback, 10) >= 0 ? parseInt(config.protocolCfResendCallback, 10) : 10),
    // ★ ⑫ 从 10 降到 5（2026-08-16）：实测重发深度战果是
    //     第1次过 13 · 第2次过 7 · 第3次过 1 · 第4次及以后 0（约 7 次机会一次没成）。
    //   而 ⑫ 现在后面还接着"换出口兜底"，把 40 秒花在从没成功过的第 6~10 发上
    //   只是拖延到达兜底的时间。5 已经把观测到的全部成功深度（1~3）都覆盖住，还多留两发余量。
    protocolCfResendSession: Math.max(0, parseInt(config.protocolCfResendSession, 10) >= 0 ? parseInt(config.protocolCfResendSession, 10) : 5),
    // 可选遥测的独立预算（按导航阶段计）：超时只记 telemetry partial/skipped，
    // 不拖住注册主干；报告会把等待时间、超时和预算跳过分开统计。
    // ★ B7-4 并发扇出：探针页内三处并发点（资源加载 / 在飞槽位记账 / ⑳ awe 批次）
    //   共用的唯一并发数。默认 10 是【拍的、没有语料依据、待实测】——
    //   语料只给了两个端点（我们 p50=6，浏览器 p50=245），中间怎么取没有依据。
    //   完整理由、调参方法（跑 6/8/10/12 各若干轮，量墙钟 / 4xx 率 / 突发度分布）
    //   与回滚路径，见 src/pageAssets.js 的 DEFAULT_FANOUT_CONCURRENCY 头注。
    //   ⚠ 这里的默认值必须和 pageAssets 的 DEFAULT_FANOUT_CONCURRENCY 同步；
    //     test_page_assets.js 有一条红线在逐字比对，改一处漏一处会红。
    protocolFanoutConcurrency: boundedInteger(config.protocolFanoutConcurrency, 8, 1, 64),
    // ── B7 节奏闸门的回滚开关（默认开）────────────────────────────────────
    //   置 false = 每一段的 paceSince 只算计划、只打日志、【不等】。
    //   ⚠ 这【不是】回到 B7 之前：那时是 pace() 纯睡眠，那个原语已经删掉了
    //     （src/protocolPacing.js 的 sleepFor 头注写着 probe 一处都不再调它）。
    //     关掉它等于"整条链路完全不排节奏"，跳间隔会全档掉到我们自己的裸耗时。
    //   ⚠ 关它会连带把 ㉔ chatgpt Statsig 一起关掉 —— 见 resolveStatsigReplayGate。
    protocolPaceSinceEnabled: statsigReplayGate.paceSinceEnabled,
    protocolTelemetryRequestTimeoutMs: boundedInteger(config.protocolTelemetryRequestTimeoutMs, 8000, 1000, 60000),
    // 协议注册完成后给账号设置的统一密码。空串=不设密码（该步骤整段跳过）。
    // ★ 值放 config.json（已 gitignore），不进源码 —— 密码进 git 历史就再也拿不掉了。
    protocolAccountPassword: String(config.protocolAccountPassword || '').trim(),
    // 协议注册成功后是否自动硬化（开 2FA + 设密码 + cookie 刷新 AT，进程内跑）。
    // 默认开（跟 protocolEnabled 一样用 !== false 惯例）；设 false 关。
    protocolAutoHarden: config.protocolAutoHarden !== false,
    // ★只落完整号（2026-09-22，默认关）：开启后，协议注册落库时先进【暂存态 hardening】(对全系统隐身)，
    //   等 2FA+密码+刷AT 全补齐才转 registered(现身)；硬化最终失败的号直接删除(丢弃，接受烧邮箱)。
    //   关闭=现状：⑫拿到AT就以 registered 落库(号永不丢)、硬化在库内补。改动大、动注册路径，先关着测。
    protocolLandCompleteOnly: config.protocolLandCompleteOnly === true,
    // 硬化里 web 设密码的换出口重试次数：第一发 GET /api/auth/csrf 是冷请求、约 89% 吃 CF 403，
    // 换几个出口就有机会过（失败全在发码前，重试干净）。默认 4，范围 1~8。
    hardenWebPasswordTries: boundedInteger(config.hardenWebPasswordTries, 4, 1, 8),
    // 硬化改为【独立后台 sweep】（不再内联在注册 worker 里——内联会占槽 ~几分钟/号，50 并发下注册吞吐直接塌）。
    // 注册 worker 只打 needsHarden 标记就返回；下面三项控制后台 sweep：
    hardenConcurrency: boundedInteger(config.hardenConcurrency, 4, 1, 16),   // 后台硬化并发（默认 4；别太高，否则又把 OpenAI/收码/代理打满）
    hardenSweepInterval: parseInt(config.hardenSweepInterval, 10) || 30000,  // 硬化扫描间隔 ms（默认 30s）
    hardenMaxFails: boundedInteger(config.hardenMaxFails, 6, 1, 20),         // 单号硬化失败上限（到顶放弃；每失败一次指数退避）
    protocolTelemetryBudgetMs: boundedInteger(config.protocolTelemetryBudgetMs, 30000, 1000, 120000),
    // OTP AWE 增量规划目前只写本地 ledger，不发送新增请求；必须显式开启，
    // 且 planner 仍会要求至少 3 份同画像浏览器证据。默认关闭，避免把
    // 两份样本里的重复 transport 误当成第二条业务事件。
    protocolTelemetryOtpIncrementalEnabled: config.protocolTelemetryOtpIncrementalEnabled === true,
    // 真正发送单个 OTP validate resource 的第二道独立开关。即使上面的
    // planner 评估开关打开且证据 ready，这里不显式设 true 也只能写
    // prepared_not_sent，避免诊断配置意外变成网络发送。
    protocolTelemetryOtpIncrementalDispatchEnabled: config.protocolTelemetryOtpIncrementalDispatchEnabled === true,
    // chatgpt 侧的 Statsig 实例（client-nb0q… @ 3.33.1）。线上是两个 Statsig
    // 项目，我们长期只发了 login-web 那个；这个占真人 rgstr 流量的 53%。
    // 曝光值全部读自登录页 bootstrap 的 statsigPayload，不猜；负载解析不出
    // 或自检不过就整块不发。默认关闭 —— 这是第一次往主链路加这一族请求，先灰度。
    // ★★★ B8-5：这里导出的是【解算后】的值，不是原始配置 ——
    //   protocolPaceSinceEnabled=false 时它恒为 false（见 resolveStatsigReplayGate）。
    protocolTelemetryChatgptStatsigEnabled: statsigReplayGate.enabled,
    // ★ 原始诉求也导出来：probe 还要把 CLI 的 --chatgpt-statsig 折进去再过一次同一道闸，
    //   拿解算后的值当输入会让"被闸拦下"这件事自己把自己抹掉（拦下 → false → 下次判定
    //   变成"根本没请求过" → 不再报矛盾）。
    protocolTelemetryChatgptStatsigRequested: statsigReplayGate.statsigRequested,
    // 账本位：GUI/pipeline 要能看见"这一族是被开关矛盾拦下的"，而不是"没开"。
    protocolStatsigReplayBlocked: statsigReplayGate.blocked,
    // 真下 JS bundle 才能如实报那 8 条资源性能事件。与上面那个分开：
    // 只开曝光不开 bundle 是一个合法组合，曝光那 44 条不依赖它。
    //
    // ★ 代价实测下来【基本是 0】，不是当初估的 4.4MB/号：清单声明的 24 个 JS
    //   模块，登录页 HTML 的 modulepreload 已经全下过了，真正要补的只有 2 个
    //   CSS（实测 324ms）。两个开关的【请求足迹完全相同】—— 开不开 bundle 都是
    //   那 2 条 rgstr，区别只在正文里是 54 还是 62 个事件。
    //   ⚠ 上限仍是 26 个资源：某次 modulepreload 没覆盖全、或前面的下载失败时，
    //     补下的量会涨回去。所以这是"通常为 0、最坏 4.4MB"，不是"恒为 0"。
    protocolTelemetryChatgptBundleEnabled: config.protocolTelemetryChatgptBundleEnabled === true,
    // 重发之间的退避：第 k 次等 base*k + rand(jitter)，封顶 capMs。
    // ★ 必须递进，不能沿用老的固定 400+rand(500)：10 次重发若都挤在 5 秒内，
    //   就是朝同一个 URL 连打 10 发 403 —— 真实浏览器不会这样，那个形态本身就是特征。
    //   递进还顺带把「要两发」和「要两秒」这两个假说分开了（老数据里两者完全共线：
    //   k=1 落在 403 后约 0.5s、k=2 落在约 1.5~2.0s，拟合得一样好）。
    protocolCfResendBackoffMs: Math.max(50, parseInt(config.protocolCfResendBackoffMs, 10) || 600),
    protocolCfResendJitterMs: Math.max(0, parseInt(config.protocolCfResendJitterMs, 10) >= 0 ? parseInt(config.protocolCfResendJitterMs, 10) : 500),
    protocolCfResendCapMs: Math.max(200, parseInt(config.protocolCfResendCapMs, 10) || 4000),
    // ★ ⑫ 拿不到 accessToken 时，换一个干净住宅出口、用 ⑪ 存下的 session cookie 直接换 AT。
    //   只在失败路径上触发，成功就把一个【孤儿号】救成正常号。
    //   ⚠ 这跟"用换出口代替原地重发"是两回事，结论也相反 —— 见 probe 里那段注释：
    //     代替原地重发是拿好牌换烂牌（70.6% vs 28%）；用尽之后再换是纯增量，下限是现状。
    //   置 false 退回老行为（⑫ 拿不到就直接判孤儿号）。
    protocolCookieFallback: config.protocolCookieFallback !== false,
    // 兜底要【换几个出口】。换出口才是这个兜底的全部意义 —— 同一个出口再试多少次都一样
    //   （实测 ⑫ 连打 10 发全是 352~597ms 的边缘快速拒绝）。
    // ⚠ refreshOnce 自带的 retries 参数【不换出口】（它每次都传同一个 proxyUrl），
    //   所以轮换必须由探针那个外层循环做，别把这两个数搞混。
    // ⚠ cookie 本身失效（SESSION_EXPIRED）时会提前停 —— 那跟出口无关，换一百个也一样。
    protocolCookieFallbackExits: Math.max(1, parseInt(config.protocolCookieFallbackExits, 10) || 5),
    rtConcurrency: parseInt(config.rtConcurrency, 10) || 6,
    // 跨池占位策略：strict=老行为(任何跨池占位都算冲突) / queued_ok=只排队不算冲突 / plus_override=付费号无视一切占位。
    // 归一化交给 rtAutomation.normalizeRtConflictPolicy（非法值回落 plus_override），这里只负责把键透出来 ——
    // 漏了这一行等于 rtAutomation.js:11 声明的「strict 留作回滚」根本按不下去，改 config.json 毫无反应且无报错。
    rtConflictPolicy: ['strict', 'queued_ok', 'plus_override'].includes(config.rtConflictPolicy) ? config.rtConflictPolicy : 'plus_override',
    rtAutoExtractEnabled: config.rtAutoExtractEnabled === true,
    rtAutoPlanMode: ['plus', 'free', 'both'].includes(config.rtAutoPlanMode) ? config.rtAutoPlanMode : 'plus',
    rtAutoDesiredState: ['running', 'paused'].includes(config.rtAutoDesiredState) ? config.rtAutoDesiredState : 'paused',
    rtAutoScanIntervalMs: Math.max(1000, parseInt(config.rtAutoScanIntervalMs, 10) || 3000),
    rtAutoMaxAttempts: parseInt(config.rtAutoMaxAttempts, 10) > 0 ? parseInt(config.rtAutoMaxAttempts, 10) : 3,
    // ═══ RT 失效巡检（2026-08-01/02）═══
    //
    // 背景实测（不是推测，三条都在真号上测过）：
    //   · 已作废的 refresh_token 打 auth.openai.com/oauth/token → 401 refresh_token_invalidated
    //   · 同一批号的 access_token（**JWT 尚未过期**）打 chatgpt.com/backend-api
    //     → note='token_invalidated'：会话被吊销时服务端会拒掉还没到期的 AT
    //   · RT 换取**会轮转**（196→211 字符，旧的作废），响应里的 earliest_refresh_at ≈ 9 天后
    //
    // 所以分两级：一级用**免费只读**的 AT 探活高频扫（不碰 RT），二级只在一级报警时
    // 用 RT 换取确诊一次。定期全量刷 RT 是不行的 —— 对方给的最小间隔就是 9 天。
    rtRevivePatrolEnabled: config.rtRevivePatrolEnabled === true,
    // 一级间隔。下限 60s 是护栏：这批号规模小（实测账号总览里有 RT 的只有几十个），
    // 但探活要出网，别让人手滑填成 1 秒。
    rtRevivePatrolIntervalMs: boundedInteger(config.rtRevivePatrolIntervalMs, 600000, 60000, 6 * 3600 * 1000),
    // 一级每轮最多探多少个（探活内部已有 TLS 并发 8，这里限的是每轮批量）
    rtRevivePatrolBatch: boundedInteger(config.rtRevivePatrolBatch, 40, 1, 200),
    // ── 无 RT 死号 relogin 巡检（提链探出 token 失效、又没 refresh_token 的号，走邮箱重登拿 at）
    // ★ 默认关：它会真的出网发邮箱验证码、烧代理流量，得由人决定什么时候开。
    rtReloginDeadPatrolEnabled: config.rtReloginDeadPatrolEnabled === true,
    // 间隔下限 60s 同上，是防手滑护栏。
    rtReloginDeadPatrolIntervalMs: boundedInteger(config.rtReloginDeadPatrolIntervalMs, 600000, 60000, 6 * 3600 * 1000),
    // 每轮限批：每号一封邮箱码 + 一条独立代理会话，放太大会把 OTP 投递打限流。
    rtReloginDeadBatch: boundedInteger(config.rtReloginDeadBatch, 15, 1, 100),
    // 二级：一级报 token_invalidated 后，用 RT 换取确诊一次。★ 关掉的话只信一级（会有误判风险）
    rtReviveConfirmEnabled: config.rtReviveConfirmEnabled !== false,
    // 探到失效后自动送去重拿 RT。★ 默认关 —— 开着的话半夜探到就会自己开浏览器跑，
    //   而拿 RT 是账号级敏感操作，得由人决定什么时候开。
    rtReviveAutoRescue: config.rtReviveAutoRescue === true,
    // 自动重拿的并发（独立于手动那次的 rtConcurrency，免得一改就互相影响）
    rtReviveRescueConcurrency: boundedInteger(config.rtReviveRescueConcurrency, 3, 1, 16),
    // 同一个号连续救几次还不成就停下等人，别无限烧浏览器和出口
    rtReviveMaxRescuePerAccount: boundedInteger(config.rtReviveMaxRescuePerAccount, 3, 1, 10),
    rtClaimLeaseMs: boundedInteger(config.rtClaimLeaseMs, 45000, 15000, 300000),
    rtClaimHeartbeatMs: boundedInteger(config.rtClaimHeartbeatMs, 10000, 5000, 30000),
    // RT worker 启动错峰间隔（毫秒）。并发拉起的 worker 依次隔开这么久再真正开工（预热 GET），
    //   避免几十个子进程在同一刻从同一个 JP 代理池 + 同一套 TLS 指纹齐射 auth.openai.com、
    //   自己把自己打成一次 CF zone 挑战波（详见 src/rtLaunchGate.js 的完整复盘）。
    //   只错开开工时刻，不限并发数。默认 600ms；0=关闭（退回同一刻齐射的老行为）。
    //   坏窗口时可临时调大（如 1500）把发车拉得更稀；改 config.json 即热生效，不用重启。
    //   上限 10s 是护栏——再大会让冷启动补位慢到肉眼可见，而收益早已饱和。
    rtLaunchStaggerMs: boundedInteger(config.rtLaunchStaggerMs, 600, 0, 10000),
    pixConcurrency: parseInt(config.pixConcurrency, 10) || 3,
    // SMS Provider
    smsProvider: String(config.smsProvider || 'herosms').trim().toLowerCase(),
    // 取号排队上限(秒)：NO_NUMBERS 不是「没货」而是「这一瞬没轮到」——号分时释放，接码平台官网「购买」就是排队轮询等号
    // （实测 SMSBower 哥伦比亚 providerIds 池排队 35s/7 次才拿到号）。原来只重试 5×3s≈15s 就判死 → 大量假失败。
    smsQueueWaitSec: parseInt(config.smsQueueWaitSec, 10) >= 0 ? parseInt(config.smsQueueWaitSec, 10) : 120,

    // HeroSMS
    heroSmsApiKey: config.heroSmsApiKey,
    heroSmsService: config.heroSmsService || 'dr',
    heroSmsCountry: parseInt(config.heroSmsCountry, 10) || 16,
    // ★ 2026-07-17 修 bug:这俩之前漏导出 → createSmsProvider 读到 undefined → maxPrice/operator 从没传给
    //   getNumberV2 → 空调用被 HeroSMS 判 NO_NUMBERS(getNumberV2 必须带 maxPrice)→ 触发 120s 假排队。
    //   实测:带 maxPrice=0.0524 秒取号($0.0516);不带→NO_NUMBERS。补上导出即恢复秒取。
    heroSmsMaxPrice: parseFloat(config.heroSmsMaxPrice) || 0,
    heroSmsOperator: String(config.heroSmsOperator || '').trim(),   // 运营商精选(逗号分隔),空=不限

    // 5sim
    fiveSimApiKey: config.fiveSimApiKey || '',
    fiveSimCountry: config.fiveSimCountry || 'england',
    fiveSimOperator: config.fiveSimOperator || 'any',
    fiveSimProduct: config.fiveSimProduct || '',

    // GrizzlySMS
    grizzlySmsApiKey: config.grizzlySmsApiKey || '',
    grizzlySmsService: config.grizzlySmsService || 'dr',
    grizzlySmsCountry: parseInt(config.grizzlySmsCountry, 10) || 16,

    // SMSBower（当前生效值；预设切换时由前端把所选预设写入这三项）
    smsBowerApiKey: config.smsBowerApiKey || '',
    smsBowerService: config.smsBowerService || 'dr',
    smsBowerCountry: parseInt(config.smsBowerCountry, 10) || 187,
    smsBowerMaxPrice: parseFloat(config.smsBowerMaxPrice) || 0,
    smsBowerProviderIds: String(config.smsBowerProviderIds || '').trim(),   // 精准指定号池 provider_id（逗号分隔，如 "2236,3243,3253"=黄金档）；空=不限，靠 maxPrice/全池取。走 getNumberV2 的 providerIds 参数
    // 多套可切换预设：[{name,service,country,maxPrice}]；smsBowerActivePreset = 当前选中的 name
    smsBowerPresets: Array.isArray(config.smsBowerPresets) ? config.smsBowerPresets : [],
    smsBowerActivePreset: config.smsBowerActivePreset || '',

    // SMSPool（REST 接码站；用数字国家/服务 id：US=1、OpenAI/ChatGPT=671，见 /country|/service/retrieve_all）
    // ★ 同 heroSms 踩坑：这些字段必须显式导出，否则 createSmsProvider 读到 undefined
    smsPoolApiKey: config.smsPoolApiKey || '',
    smsPoolService: String(config.smsPoolService || '671').trim(),   // 默认 OpenAI/ChatGPT
    smsPoolCountry: String(config.smsPoolCountry || '1').trim(),     // 默认 United States
    smsPoolMaxPrice: parseFloat(config.smsPoolMaxPrice) || 0,        // 价格上限($)，0=不限
    smsPoolPool: String(config.smsPoolPool || '').trim(),            // 指定号池(数字/名字)，空=自动
    // SMSPool 号码复用池：一号最多绑 N 个账号、每次绑定后冷却 M 分钟、号存活 H 小时（用户实测：3绑定/1h冷却/~120h）
    smsPoolReuse: config.smsPoolReuse !== false,                     // 默认开
    smsPoolMaxBinds: parseInt(config.smsPoolMaxBinds, 10) > 0 ? parseInt(config.smsPoolMaxBinds, 10) : 3,
    smsPoolCooldownMin: parseInt(config.smsPoolCooldownMin, 10) >= 0 ? parseInt(config.smsPoolCooldownMin, 10) : 60,
    smsPoolLifespanHours: parseFloat(config.smsPoolLifespanHours) > 0 ? parseFloat(config.smsPoolLifespanHours) : 120,

    // Outlook
    outlookClientId: config.outlookClientId || '9e5f94bc-e8a4-4e73-b8be-63364c29d753',

    // Mail Provider
    mailProvider: String(config.mailProvider || 'legacy').trim().toLowerCase(),

    // Legacy 临时邮箱
    mailBaseUrl: config.mailBaseUrl || '',
    mailAdminPassword: config.mailAdminPassword,
    mailSitePassword: config.mailSitePassword || '',
    mailDomain: config.mailDomain || '',

    // YYDS Mail
    yydsMailBaseUrl: config.yydsMailBaseUrl || 'https://mailapi.example.com/v1',
    yydsMailApiKey: config.yydsMailApiKey || '',
    yydsMailDomain: config.yydsMailDomain || '',
    yydsMailSubdomain: config.yydsMailSubdomain || '',

    // 代理：这 5 个从「直读」改为「按当前供应商模板派生」。
    // 在 config.js 这一层派生（而不是 proxyConfig.js）是刻意的：index.js 的两处 legacy 入口
    // 直接读 config.proxyHost/Username/Password、绕开 proxyConfig，在这里派生能让它们跟着一起切，
    // 不会出现「主流程切了、legacy 还在旧家」的分叉。
    proxyHost: proxyTemplate.host,
    proxyPort: proxyTemplate.port,
    proxyUsername: proxyTemplate.username,
    proxyPassword: proxyTemplate.password,
    proxyType: proxyTemplate.type,
    // 供应商切换（GUI「住宅代理供应商」面板写入）
    proxyVendor,                                                    // '' = 从凭据嗅探
    proxyVendors,                                                   // { cliproxy:{...}, kookeey:{...}, iproyal:{...} }
    proxyVendorByLane: normalizeLaneMap(config.proxyVendorByLane, PROXY_VENDOR_NAMES),   // 分链路灰度
    proxyTtlByLane: normalizeTtlMap(config.proxyTtlByLane),         // 粘性时长：注册 30m / 促销 0
    proxyMissAlertThreshold: boundedInteger(config.proxyMissAlertThreshold, 20, 1, 1000),
    // 注册出口区域：'JP'(日本，默认) / 'BR'(巴西) / 'PH'(菲律宾) / 'US'(美国)。仅影响注册 Roxy 出口，与 RT 代理隔离；GUI 可一键切换
    // ★ 这里**有意不做白名单**：buildRegisterProxy 把地区直接交给方言层派生，三家供应商都支持任意二字母区码，
    //   加地区只需在 GUI 的 REGION_LABEL 里加一行。写死白名单只会让下次加地区时多一个要改的地方。
    registerRegion: String(config.registerRegion || 'JP').toUpperCase(),
    // 各区域注册代理映射 { JP:{host,port,username,password}, BR:{...} }；空则回退全局 proxy*
    registerProxies: (config.registerProxies && typeof config.registerProxies === 'object' && !Array.isArray(config.registerProxies)) ? config.registerProxies : {},
    // 注册出口自检：建窗后读一次真实出口国家，不是 registerRegion 就换会话重开。默认开。
    // ★ 必须在【建窗之后】读、且从【窗口内部】读 —— roxyProvider.createWindowRaw 内部做 rotateSession，
    //   出口 IP 到建窗那一刻才定下来，建窗前探到的是另一个会话的另一个 IP，探得再准也不作数。
    registerVerifyEgress: config.registerVerifyEgress !== false,
    // 出口不匹配时最多换几次会话重开窗口。cliproxy 的 PH 单次命中约 80%，3 次全败约 0.8%。
    registerEgressRetries: boundedInteger(config.registerEgressRetries, 3, 1, 8),

    // ★★★ Sentinel SDK 自动跟版（2026-08-25）。
    //   SDK 版本号会被发出去三处：sentinel/req 的 referer、p 载荷的 [5] 槽、RT 的预热请求。
    //   OpenAI 换版后不跟：旧版还能用 ⇒ 只有我们还报一个没人用的版本号（覆盖率 100% 的
    //   聚类特征）；旧版被下架 ⇒ 注册大面积失败且第一时间查不出原因。
    //   语料实证 86 份浏览器抓包里同时存在 20260810913b(54)/20260219f9f6(20)，换版真会发生。
    //
    //   ⚠ 必须 === true 才开。默认 false ＝ 用代码里钉死的那一对（版本号 + sentinel/sdk.js），
    //     与本项不存在时逐字节等价 —— 生产 RT 链路 5000+ 号 0 封号靠的就是钉死那一对。
    //   ⚠ 【这两行不能少】config.js 是显式白名单：只写进 config.json 而不在这里映射，
    //     require('./config') 读到的是 undefined，开关看着开了其实没开。
    //     2026-08-25 第一次接线时就踩了这个坑。
    sentinelSdkAutoUpdate: config.sentinelSdkAutoUpdate === true,
    // 跟版探测的最小间隔，默认 1 小时。换版是天级事件，探太勤只是白送足迹。
    sentinelSdkCheckIntervalMs: boundedInteger(config.sentinelSdkCheckIntervalMs, 3600000, 60000, 86400000),

    // OAuth
    oauthClientId: config.oauthClientId || 'app_EMoamEEZ73f0CkXaXp7hrann',
    oauthRedirectPort: parseInt(config.oauthRedirectPort, 10) || 1455,
    tokenOutputDir: config.tokenOutputDir || '',
    tokenOutputDirs: Array.isArray(config.tokenOutputDirs)
        ? config.tokenOutputDirs.filter(Boolean)
        : [],

    // 浏览器
    useChrome: config.useChrome !== false,
    chromePath: config.chromePath || 'google-chrome-stable',

    // CPA (CLIProxyAPI) 上传
    cpaBaseUrl: config.cpaBaseUrl || 'http://localhost:8317/v0/management',
    cpaManagementKey: config.cpaManagementKey || '',
    cpaUploadEnabled: config.cpaUploadEnabled === true,

    // 升级流程（前端可改，pipeline.js 每个任务热读取）
    cdk: config.cdk || '',
    upgradeApiUrl: config.upgradeApiUrl || 'https://dkl118.shop/api/uploads/at',
    upgradeRetryDelayMs: parseInt(config.upgradeRetryDelayMs, 10) || 8000,
    upgradeTimeoutMs: parseInt(config.upgradeTimeoutMs, 10) || 120000,

    // UPI Hub（upi.mengko.net）自动升级 Plus：喂 accessToken→余额扣0.6→网站自动重试到成功→升Plus
    upiCookie: config.upiCookie || '',                                  // gpt_upi_public_user=...（UI 粘贴；空则回退 upi_session.json）
    upiUntilSuccess: config.upiUntilSuccess !== false,                  // 提交时开"自动重试到成功"（默认开）
    upiPollIntervalMs: parseInt(config.upiPollIntervalMs, 10) || 25000, // 轮询任务状态间隔
    upiReconcileIntervalMin: parseInt(config.upiReconcileIntervalMin, 10) || 15,  // 权威对账(翻带邮箱历史,补标已Plus漏标号)周期,分钟
    upiAutoDetect: config.upiAutoDetect !== false,                      // 订单完成后自动检测套餐确认 Plus（默认开）
    upiMaxActivePerAccount: parseInt(config.upiMaxActivePerAccount, 10) || 2, // 每个 UPI 账号同时进行中上限（实测 premium_og=2；服务器 "can run up to N extract"）
    upiAutoPublish: config.upiAutoPublish !== false,                    // 二维码生成后自动发布扫码订单（默认开，关键功能；网站那个 toggle 是浏览器本地的，本系统不依赖它、每次提交都强制带上）
    upiPostCompleteWaitSec: parseInt(config.upiPostCompleteWaitSec, 10) || 120,  // 订单完成(扫码付款成功)后，先等这么久再开始检测 Plus（Plus 要过一会儿才生效，默认2分钟）
    upiCompleteGiveupMin: parseInt(config.upiCompleteGiveupMin, 10) || 30,       // 订单完成后持续复检的最长时长，超过仍非 Plus 才判失败（默认30分钟，宽松防误判）
    upiExtractTimeoutMin: parseInt(config.upiExtractTimeoutMin, 10) || 30,        // 单个任务(还没"订单完成")最多跑这么久，超了自动取消、换账号/出口节点重试（默认30分钟）
    upiTimeoutCooldownMin: parseInt(config.upiTimeoutCooldownMin, 10) || 5,       // 超时取消后该账号冷却这么久再重排（让其它账号先上）
    upiMaxTimeoutRetries: parseInt(config.upiMaxTimeoutRetries, 10) || 5,         // 同一账号超时重试上限，超过判失败（防对死号无限重试）
    upiPostSpendWaitSec: parseInt(config.upiPostSpendWaitSec, 10) || 10,          // 真扣款(SCAN_ORDER_SPEND)后等这么久再检测 Plus（升级真完成才扣款，默认10秒）
    upiScanFailMaxRetries: parseInt(config.upiScanFailMaxRetries, 10) || 8,       // 扫码退款(扫码方问题，非账号责)→退回待升级重排的上限，超过才搁置
    upiNoOrderMaxFails: parseInt(config.upiNoOrderMaxFails, 10) || 3,             // 「未发单失败」(连二维码都没发就 failed=账号侧问题)的小额重试上限；超过且后端在正常出单→判「不可升级」，防同一坏号每十几秒反复提交
    upiQueueStuckMin: parseInt(config.upiQueueStuckMin, 10) || 3,                 // status=queued 一直 0% 卡死超过这么久(分钟)就当未发单失败(UPI 好号秒级开跑、并发满5;还卡 queued=被静默打回，多半"无免费资格")，按上限走→不可升级。好号是 running 不会被误判

    // Roxy 指纹浏览器接管（B 方案：每号建/删 profile，注册/RT 走 Roxy 窗口）
    useRoxy: config.useRoxy === true,
    roxyApiBase: config.roxyApiBase || 'http://127.0.0.1:50000',
    roxyApiKey: config.roxyApiKey || '',
    roxyWorkspaceId: parseInt(config.roxyWorkspaceId, 10) || 0,
    roxyCoreVersion: config.roxyCoreVersion || '150',        // RoxyChrome 内核号，填 Roxy 客户端「内核」下拉里已装的版本（如 150/149/…；官方 API 文档页是旧的、别信它的 135 上限）
    roxyOs: config.roxyOs || 'Windows',
    roxyOsVersion: config.roxyOsVersion || '11',
    roxyHeadless: config.roxyHeadless !== false,

    // RT 提取浏览器模式：'chrome'(默认；普通无痕 Chrome + 随机日本指纹 + 每次换 JP 家宽 SOCKS5 出口 + 拿完删库) / 'roxy'(退回 Roxy 指纹窗)。
    // 只管 RT，注册仍走上面的全局 useRoxy（互不影响，可自由切换）
    rtBrowserMode: String(config.rtBrowserMode || 'chrome').trim().toLowerCase(),
    // RT 拿号时是否本机直连（不挂日本家宽代理，直接用本机 IP）。★默认开：只有显式写 false 才回退挂日本家宽代理。
    // 仅对 Chrome 模式生效；Roxy 模式忽略（Roxy 自带代理注入）
    // 缺省 = 本机直连（`!== false`）。家宽代理是**可选项**，要用得显式写 rtNoProxy:false。
    //
    // ★ 这条当天来回改过一次，把结论记下来免得下次又绕：
    //   一度怀疑「同一本机 IP 反复授权」是 RT 被吊销的原因，于是把缺省改成走代理。
    //   后来查明 RT 掉是**官方那边的原因**，和走不走代理无关。而实测对比很悬殊：
    //     走代理  ：约 40 分钟拿到 9 个，失败 16（大量 ERR_CONNECTION_CLOSED / 页面卡死看门狗）
    //     本机直连：约 11 分钟拿到 66 个，失败 7
    //   住宅代理的链路质量才是这条流程的实际瓶颈，所以缺省回到直连。
    //
    // ★ 真正和出口有关、且**必须保留**的是另外两条修复（别因为这里改回直连就以为它们没用了）：
    //   ① rtBrowserTask / pipeline 把 browserService.effectiveProxy 传给 OAuthService
    //      —— 挂代理时浏览器和换 token 必须同一个轮换会话；
    //   ② oauthService 删掉了「代理连不上就偷偷切直连」。
    //   直连模式下这两条是恒等变换（proxy 全程为 null），一旦有人把代理开回来它们立刻生效。
    rtNoProxy: config.rtNoProxy !== false,
    // RT 普通 Chrome 是否使用新无头模式。默认有头以保持 Turnstile/OAuth 稳定；Roxy 模式忽略。
    rtChromeHeadless: config.rtChromeHeadless === true,
    // 普通 Chrome 从拉起进程到 Page/CDP 初始化完成的硬超时，避免停在 about:blank 无限占槽。
    rtBrowserLaunchTimeoutMs: boundedInteger(config.rtBrowserLaunchTimeoutMs, 60_000, 15_000, 180_000),
    // 父进程 RT worker 的业务无进展看门狗。心跳只代表子进程还活着，
    // 不代表 OAuth/邮箱请求仍在推进；超过该时长没有 progress 就中止并让队列重排。
    // 0 可显式关闭（不建议生产关闭）。默认 10 分钟，覆盖最长邮箱验证码等待。
    rtWorkerProgressWatchdogMs: boundedInteger(
        config.rtWorkerProgressWatchdogMs,
        600_000,
        0,
        1_800_000,
    ),
    // RT 业务可保持高并发，但 Chrome/CDP 初始化单独限流，避免瞬时启动风暴拖死主进程。
    rtBrowserLaunchConcurrency: boundedInteger(config.rtBrowserLaunchConcurrency, 2, 1, 4),
    // 主线程延迟超过该值时暂缓认领新 RT，已有任务不受影响。
    rtEventLoopLagLimitMs: boundedInteger(config.rtEventLoopLagLimitMs, 750, 100, 5000),
    // 持久化清理失败达到上限后转 dead-letter，避免失效 Roxy/SMS 任务永久占用主线程。
    cleanupOutboxMaxAttempts: boundedInteger(config.cleanupOutboxMaxAttempts, 12, 1, 100),
    // 未确认远端已关闭的 Roxy dead-letter 仍占浏览器槽；低频复探直到 101/close/delete 确认。
    cleanupOutboxRoxyDeadLetterProbeMs: boundedInteger(
        config.cleanupOutboxRoxyDeadLetterProbeMs,
        60_000,
        5_000,
        24 * 60 * 60 * 1000,
    ),
    // RT 宽容验证模式：默认(false)强制 SMS，验证码走 WhatsApp 就退号换新；开(true)则 WhatsApp/SMS 都接受，不因 WhatsApp 退号（接码平台若能收 WhatsApp 即可用，收不到则轮询超时才失败）
    rtTolerantVerify: config.rtTolerantVerify === true,
    // 🔬 研究开关：抓 RT 的 OAuth 登录流量到 logs/authflow/（只记 方法/路径/query键[脱敏]/状态，标挑战资源），用于评估"哪些步骤能切纯 HTTP、Turnstile 卡哪一步"。默认关。
    rtCaptureAuthFlow: config.rtCaptureAuthFlow === true,
    // ★ rtUseProtocol（B2 混合协议）已于 2026-08-14 移除 —— 长期为 false、无人使用，
    //   且与纯协议共用 RtProtocolClient，留着只会让每次改 RT 都要多想一条死分支。
    // 🔬 深抓档：额外记各端点请求/响应 body(自动脱敏密码/OTP/token/授权码/cookie/邮箱/手机)，用于设计纯协议客户端所需的字段 schema。默认关。
    rtCaptureAuthBodies: config.rtCaptureAuthBodies === true,
    // 🔬 C 架构 纯协议 RT：全程无浏览器，Sentinel 由 Node vm 跑真 sdk.js 算(src/sentinelClient.js)，
    //   authorize→邮箱OTP→(按需绑号)→consent→拿code→换token 全走 HTTP(src/rtPureProtocolTask.js)。
    //   默认关。开了会把 rtWorkerHost 的 runnerModule 换成 rtPureProtocolTask.js（改 pipeline.js 分支需重启）。
    //   ★Sentinel 依赖 sentinel/sdk.js，会随 OpenAI 更新腐坏；失效时关掉本开关即可退回全浏览器链路。
    rtPureProtocol: config.rtPureProtocol === true,
    // 纯协议的出口选择（独立于 rtNoProxy，因为纯协议没有浏览器，出口语义和 B2/浏览器不同）：
    //   'follow' = 跟随 rtNoProxy（默认，行为与其他链路一致）
    //   'proxy'  = 强制走代理池（实测配置：日本住宅出口，成功率最高）
    //   'direct' = 强制本机直连
    // 只在 rtPureProtocol=true 时生效，不影响 B2/全浏览器路径。
    rtPureProxyMode: ['follow', 'proxy', 'direct'].includes(String(config.rtPureProxyMode || '').toLowerCase())
        ? String(config.rtPureProxyMode).toLowerCase() : 'follow',
    // 协议链路 add-phone/send 撞 fraud_guard 时的**换号预算**（见 rtProtocolClient.js:175 的完整复盘）：
    //   rtProtoMaxFraudStreak = 连续几个号被 fraud_guard 拒发就中止换号、整条会话作废重建
    //   rtProtoMaxFraudTotal  = 同一会话内被 fraud_guard 拒发的号**总数**上限（不被其它类型的拒绝清零）
    // 3/4 是 2026-08-10 把原值 1 放宽后的取值：撞第一次不再作废会话，而是原地换号继续
    //   （实测同会话换号 1.0s vs 丢弃重建 88.4s）。上限 7 是硬约束——必须 < maxPhoneTries(8)，
    //   否则会滑到 PHONE_ALL_REJECTED 那条无人分类、直接转人工的路径上。
    // ★效果不满意就把 rtProtoMaxFraudStreak 改回 1 存盘，行为立刻退回原样，不用改代码。
    rtProtoMaxFraudStreak: boundedInteger(config.rtProtoMaxFraudStreak, 3, 1, 7),
    rtProtoMaxFraudTotal: boundedInteger(config.rtProtoMaxFraudTotal, 4, 1, 7),
    // 号**已经绑上**、但等码阶段翻车（120s 收不到码 / 码不对）时，允许在同一会话内换几次号。
    // 0 = 退回 2026-08-10 之前的行为（翻车即整条会话作废重建）。
    // 默认 2（= 最多 3 个号走到等码阶段）：每次翻车的代价是一次 120s 轮询，而作废重建要 ~170s
    //   还要再赔一封邮箱验证码，所以在会话内换号总是更划算；上限压在 2 是为了别让一个账号
    //   在等码上耗掉 8×120s。实测 living_methane8y@icloud.com 2026-08-10 一天内被"收不到码"
    //   打掉 4 次会话、最终 blocked 转人工，就是这个开关要解决的场景。
    rtProtoMaxOtpSwaps: boundedInteger(config.rtProtoMaxOtpSwaps, 2, 0, 5),
    // fraud_guard 全局熔断（父进程跨账号统计，见 src/fraudGuardBreaker.js 的完整说明）：
    //   窗口内被 fraud_guard 判死的号数达到阈值 ⇒ 自动把上面那两个预算**拧回 1**，
    //   即退回 2026-08-10 之前的保守行为；窗口内记录老化后自动恢复，无需人工复位。
    // 12/10min 的取值依据：零星期的实际频率是 3 天 11 次（≈0.15 次/小时），
    //   而 2026-08-03 全量风控那轮是一个批次 219 次 —— 两者差三个数量级，阈值放在中间很安全。
    // 阈值设为 0 = 关闭熔断（不建议：放宽预算的前提就是有这道闸）。
    // 协议链路撞 Cloudflare 挑战（/oauth/authorize 被 403）时，「换出口 IP 再试」的次数上限。
    // ★2026-08-11 之前这个值是死代码：rtPureProtocolClient 只读不赋，恒等于 4，改不了。
    // 实测逐次通过率 84%→54%→29%→42%（换出口确实有效，大多数一次就过），
    //   但 CF 是成片来的：08-11 的 9 次会话前 5 次全成、后 4 次全败，集中在几分钟的窗口里，
    //   那个窗口里 4 次预算全部打穿、4 个账号被误判终态。坏窗口时可以临时调大到 6~8 硬扛过去。
    // 调大的代价是每个账号在坏窗口里多耗时间（退避 2s/4s/6s… 递增），不是烧钱；调回 4 即恢复原行为。
    rtProtoCfMaxAttempts: boundedInteger(config.rtProtoCfMaxAttempts, 4, 1, 10),
    rtProtoFraudBreakerThreshold: boundedInteger(config.rtProtoFraudBreakerThreshold, 12, 0, 500),
    rtProtoFraudBreakerWindowMs: boundedInteger(config.rtProtoFraudBreakerWindowMs, 10 * 60 * 1000, 60_000, 6 * 60 * 60 * 1000),

    // 可选阶段开关（注册恒开，不在此列）
    upgradeEnabled: config.upgradeEnabled === true,
    rtEnabled: config.rtEnabled === true,

    // 注册号码源：'sms'=接码平台（默认，原流程）；'longphone'=长效号码池
    registerSource: String(config.registerSource || 'sms').trim().toLowerCase(),
    // 注册取号范围（邮箱注册专用，「指定抽取」）：
    //   { mode:'all' } 不限，默认，与本功能上线前的行为完全一致
    //   { mode:'type', type:'icloud'|'outlook' } 只抽某一类邮箱
    //   { mode:'batch', batch:'导入时手填的批次名' } 只抽某一批导入的邮箱
    // ★这个范围同时约束「预检补仓」「注册准入」「预留取号」三处，判据统一在 src/emailSelection.js。
    registerEmailPick: normalizeEmailPick(config.registerEmailPick),
    // 注册协议观测：只记录匿名网络 metadata/schema；不保存请求值、响应 body、cookie/token 值。默认关闭。
    registerCaptureMetadata: config.registerCaptureMetadata === true,
    // ★★★ 2026-08-11 带值抓包。协议化实现必须知道 client_id / state / device_id / ccaps /
    //   csrfToken 这些参数的【真实值】—— 只记参数名抄不出请求。
    //   ⚠ 开着时抓包文件包含足以重放一次注册请求的材料。cookie / authorization / password /
    //     验证码 在采集侧强制脱敏（src/registrationFlowCapture.js 的三份精确名单）。
    //   ⚠ 研究结束后【务必关掉】，别让它长期开着攒敏感文件。
    registerCaptureValues: config.registerCaptureValues === true,
    // 浏览器注册的看门狗（毫秒）。**0 = 由 pipeline 从 registerAttemptTimeoutMs 推**
    //   （尝试预算 + 2 分钟），让它永远晚于预算 —— 它抓的是"卡死"不是"慢"。
    // ⚠ 2026-09-08 之前 pipeline 里写死 360000，而尝试预算是 480000，
    //   安全阀比预算紧 2 分钟；出口劣化后注册涨到 6~7 分钟正好落进夹缝被误杀。
    //   下界 60s 是防手滑，不是调参范围。
    registerWatchdogMs: boundedInteger(config.registerWatchdogMs, 0, 0, 30 * 60 * 1000),
    // 注册默认密码（所有新号统一用；留空则每号随机）
    registerPassword: config.registerPassword || '',
    // 长效域内号（前导 0）默认归属国家拨号码，日本 = 81
    longPhoneCountryCode: String(config.longPhoneCountryCode || '81'),

    // iDEAL 荷兰长链提取（Python sidecar @ 9200）。proxy=前段 checkout 走 JP 出口；provider 阶段 sidecar 自动把 region-JP 换成 region-NL+fresh sid 出荷兰
    longLinkServiceUrl: config.longLinkServiceUrl || 'http://127.0.0.1:9200',
    longLinkProxy: config.longLinkProxy || '',                                  // 前段(checkout) JP base 代理；空则用 sidecar 自身 env
    longLinkCheckoutRegion: String(config.longLinkCheckoutRegion || 'JP'),
    longLinkProviderRegion: String(config.longLinkProviderRegion || 'NL'),
    longLinkUrlType: String(config.longLinkUrlType || 'ideal').toLowerCase(),    // 'ideal'(默认)=pay.ideal.nl 最终付款页(sidecar 已跟随 pm-redirects 的302拿到=provider_redirect_url)；'stripe'=pm-redirects.stripe.com/authorize 中间授权链
    longLinkChainStrategy: config.longLinkChainStrategy || '',                  // 默认【单链路 JP→NL】(空串=sidecar 走单链，和桌面工具默认一致)。★别用 dual_ideal：双链并发抢同一号会触发 Stripe/OpenAI 反欺诈→判死 cs(金额跳1901 / approve blocked)→提取必败
    longLinkMaxAutoRetries: parseInt(config.longLinkMaxAutoRetries, 10) >= 0 ? parseInt(config.longLinkMaxAutoRetries, 10) : 2,   // Node 侧对可挽救错误自动重试上限
    longLinkSidecarAttempts: String(config.longLinkSidecarAttempts || '10'),    // 传给 sidecar env OPENAI_PAY_CHECKOUT_ATTEMPTS（默认 5→加大到 10）
    longLinkSidecarMaxConcurrent: Math.max(1, parseInt(config.longLinkSidecarMaxConcurrent, 10) || 14),   // sidecar 同时能跑几个 job（传 env OPENAI_PAY_MAX_CONCURRENT_JOBS）。★2026-07-17 去掉内置上限(原 Math.min 32)——实际并发由 UI「并发」栏(池提交端)自己控制，此值设高(如1000)即"不设限"、永不成为瓶颈；真实天花板=本机资源+cliproxy套餐。改后须重启 sidecar 才生效
    // ★绕 $0-setup 风控开关：去 ideal 100%off 促销 → 走真实金额 PaymentIntent(出 redirect，不被 generic_decline)。改后须重启 pipeline
    longLinkIdealNoPromo: config.longLinkIdealNoPromo === true,
    longLinkMaxAcceptableMinor: parseInt(config.longLinkMaxAcceptableMinor, 10) || 50,   // 金额闸(minor)：去促销后真实金额约 2300(€23)，须抬到 >2300 才放行；默认 50 只放行接近 $0 的
    longLinkConcurrency: parseInt(config.longLinkConcurrency, 10) || 3,
    upipayConcurrency: parseInt(config.upipayConcurrency, 10) || 3,
    upipayPollIntervalMs: Math.max(1000, parseInt(config.upipayPollIntervalMs, 10) || 5000),

    // ═══ 韩国 Kakao 代付（kakao.546789.shop）═══
    // 只把【本机已提好的链】+【支付 CDK】交给对方，由真人工作台扫码付。提链是本机自己做的，
    // 对方的提链接口（要 access_token）明确不接 —— 见 KAKAO_BLOCK_PLAN_20260731.md §3。
    kakaoPayEnabled: config.kakaoPayEnabled !== false,
    kakaoPayApiBase: String(config.kakaoPayApiBase || '').trim(),               // 空=用客户端内置默认 https://kakao.546789.shop
    kakaoPaySubmitCdk: String(config.kakaoPaySubmitCdk || '').trim(),           // ★支付 CDK，不是提链 CDK（两者额度独立、接口不同）
    kakaoPayPollIntervalMs: Math.max(3000, parseInt(config.kakaoPayPollIntervalMs, 10) || 10000),
    // 这两个上限是对方 OpenAPI 里写死的 maxItems，配大了会被 422 拒；客户端还会再夹一次，这里只允许调小
    kakaoPaySubmitBatch: boundedInteger(config.kakaoPaySubmitBatch, 10, 1, 10),
    kakaoPayStatusBatch: boundedInteger(config.kakaoPayStatusBatch, 100, 1, 100),
    // 僵尸闸：非终态挂这么久就不再轮询（用本机时钟算）。实测链约 27 分钟过期，45 留足余量；
    // 它同时兜住「对方加了我们不认识的 state」——那种情况 isTerminal 恒 false，没这道闸会永远轮询下去。
    kakaoPayStuckMin: boundedInteger(config.kakaoPayStuckMin, 45, 5, 720),
    // ★ Plus 宽限期：单子已 completed 但对方还没回 account_plus 时，继续问它多久。
    //   没有这个的话，转终态那一轮没拿到 Plus 就永远拿不到了（activeIds 只收非终态）。
    kakaoPayPlusGraceMin: boundedInteger(config.kakaoPayPlusGraceMin, 20, 1, 240),
    // 支付成功后本机复检套餐。★ 结论落 kakaoPayPlanDetected/kakaoPayLocalPlus，
    //   与对方的 accountPlus/kakaoPayPlusVerified 是两路独立信源，互相印证，不许合并。
    //   这四个旋钮显式声明 —— CDK 那套（cdkDetectFirstDelayMs 等）就是没声明，
    //   config.json 里改了也不生效，人只能去读源码才知道有这回事。
    kakaoPayAutoDetectPlus: config.kakaoPayAutoDetectPlus !== false,
    kakaoPayDetectFirstDelayMs: boundedInteger(config.kakaoPayDetectFirstDelayMs, 8000, 0, 120000),
    kakaoPayDetectGapMs: boundedInteger(config.kakaoPayDetectGapMs, 10000, 1000, 120000),
    kakaoPayDetectMaxTries: boundedInteger(config.kakaoPayDetectMaxTries, 6, 1, 30),
    // 单次检测的墙钟。detectAccounts 里的 bs.launch() 挂住时，复检那个纯内存的 async 循环
    // 会永久卡死 —— 卡死等于这个号的复检永远不会有结论，且没人会发现。
    kakaoPayDetectTimeoutMs: boundedInteger(config.kakaoPayDetectTimeoutMs, 90000, 5000, 600000),
    // 漏网回捞：复检是一次性边沿触发，进程在那约 1 分钟窗口里重启就永久丢了。
    // 这个扫描器从落盘字段推导 due，重启后自动补上。必须限批，否则重启瞬间几十个号一起开检。
    kakaoPayMissedPlusIntervalMs: boundedInteger(config.kakaoPayMissedPlusIntervalMs, 60000, 10000, 900000),
    kakaoPayMissedPlusBatch: boundedInteger(config.kakaoPayMissedPlusBatch, 4, 1, 20),
    // ★ 额度对账：对方的 state 会变成 'unknown'（它把单子忘了），而额度是钱、不会忘。
    //   reserved 归零 = 这张卡名下都落定了；used 增量 = 成了几笔。
    //   这是「我们停止追问之后发生的成交」唯一的找回路径 —— 实盘丢过 100 笔。
    //   2 分钟一轮：每张有在途单的卡一次 GET，不算频繁。
    kakaoPayQuotaReconcileMs: boundedInteger(config.kakaoPayQuotaReconcileMs, 120000, 20000, 1800000),
    // ★ RT 僵尸认领回收间隔。进程被强杀时 RT 工人来不及释放认领，账号会永久卡在
    //   rtJobStatus='extracting'（选号器直接跳过这种），界面显示「提取中」但没人在跑。
    //   原来只在启动时回收一次，租约在启动之后过期的就没人管了。
    rtStaleClaimSweepMs: boundedInteger(config.rtStaleClaimSweepMs, 60000, 10000, 900000),

    // ═══ 韩国提炼（Kakao 长链提取，走本机 sidecar）═══
    // ★ 命名：提炼是 kakaoLink*，代付是 kakaoPay*。裸 kakao* 前缀不指代任何东西。
    // ★ 供应商跟随全局 proxyVendor（GUI「流程设置→网络」切换器），本组**不含任何 vendor 字段**。
    kakaoLinkEnabled: config.kakaoLinkEnabled !== false,
    // 两个覆盖项默认空 = 从全局供应商模板派生（proxyConfig.SIDECAR_LANES 负责打地区）。
    // 填了 = 临时钉死某个代理串，排障用。★ 漏了这两行就是 PayPal 2026-07-29 那个 bug 重演：
    //   provider 读到 undefined → body 里没有代理字段 → sidecar 回落到写死的池子，换家换区全失效。
    kakaoLinkProxy: String(config.kakaoLinkProxy || '').trim(),
    kakaoLinkPromoProxy: String(config.kakaoLinkPromoProxy || '').trim(),
    kakaoLinkCheckoutRegion: String(config.kakaoLinkCheckoutRegion || 'KR').toUpperCase(),
    kakaoLinkPromoRegion: String(config.kakaoLinkPromoRegion || 'VN').toUpperCase(),

    // ── 菲律宾 GCash（2026-08-02，GCASH_BLOCK_PLAN Phase 0：只有资格探针，没有提链池）──
    // 同 kakaoLink：这两个覆盖项漏掉就是 PayPal 2026-07-29 那个 bug 重演（provider 读 undefined
    // → body 里没代理字段 → sidecar 回落写死池子，换家换区全失效）。所以探针阶段就先备好。
    gcashProxy: String(config.gcashProxy || '').trim(),
    gcashCheckoutRegion: String(config.gcashCheckoutRegion || 'PH').toUpperCase(),
    // 并发不设硬上限（同 UPI）：sidecar 侧不 gate，真上限就是这个值，面板改了立即生效。
    kakaoLinkConcurrency: Math.max(1, parseInt(config.kakaoLinkConcurrency, 10) || 5),
    kakaoLinkPollIntervalMs: Math.max(1000, parseInt(config.kakaoLinkPollIntervalMs, 10) || 5000),
    kakaoLinkStuckMin: boundedInteger(config.kakaoLinkStuckMin, 10, 2, 120),
    // ★ 撞机次数 = 2。实测依据：7-31 那轮 7 个号各撞 12 次全 blocked（84 次调用只有 1 次异常，
    //   即代理没拖后腿）；三轮共 ~120 次 approve 只出 1 条链，且那条是**第 1 次**就撞开的。
    //   如果 blocked 是「每次独立 5% 通过」，84 次全败的概率只有 1.3% —— 所以它是账号级判定，
    //   同号多撞无用。给 2 而不是 1（Pix 就是 1）只为留一次瞬态容错。
    kakaoLinkApproveAttempts: boundedInteger(config.kakaoLinkApproveAttempts, 2, 1, 20),
    kakaoLinkRebuildRounds: boundedInteger(config.kakaoLinkRebuildRounds, 1, 1, 6),
    // 开跑前自检真实出口国家。★ 必须做：实测 iproyal 有 ~20% 静默串区（密码写着 -kr- 却落地美国机房），
    //   而 proxy_for_region(required=True) 只校验「字符串层面能不能打上地区」，看不出实际落地在哪。
    //   撞机次数降到 2 之后，一次坏出口浪费的不再是 1/12 次尝试，而是一整个号的 12 步投入。
    kakaoLinkEgressVerify: config.kakaoLinkEgressVerify !== false,
    kakaoLinkEgressMaxRetries: boundedInteger(config.kakaoLinkEgressMaxRetries, 3, 0, 10),
    kakaoLinkPromoSession: config.kakaoLinkPromoSession === 'same' ? 'same' : 'new',   // ★ same 会让促销静默失效
    kakaoLinkPromoAtCreate: config.kakaoLinkPromoAtCreate === true,                    // ★ false = Path A（全价建单→事后注入）
    kakaoLinkPromoId: String(config.kakaoLinkPromoId || 'plus-1-month-free').trim(),
    kakaoLinkCfAttempts: boundedInteger(config.kakaoLinkCfAttempts, 5, 1, 20),
    kakaoLinkPollRedirectSec: boundedInteger(config.kakaoLinkPollRedirectSec, 120, 20, 600),
    kakaoLinkRoundTimeoutSec: boundedInteger(config.kakaoLinkRoundTimeoutSec, 240, 60, 900),
    // ★ 默认 0：自动重试等于自动烧号。撞机次数由 ApproveAttempts 控制，外面别再套一层。
    kakaoLinkMaxAutoRetries: boundedInteger(config.kakaoLinkMaxAutoRetries, 0, 0, 5),

    // ═══ 提炼 → 代付 自动推送（桥接层）═══
    // ★ 前缀用 kakaoPush*：这层跨两个后端（本机 sidecar 提链 / 第三方代付），
    //   既不属于 kakaoLink* 也不属于 kakaoPay*，单独一个前缀才不产生新歧义。
    // ★ 默认开。之所以敢默认开：服务方计费口径是**支付成功才扣**，
    //   推空、推过期、重复推都不花钱，最坏只是占一下工作台工位。
    kakaoPushEnabled: config.kakaoPushEnabled !== false,
    kakaoPushIntervalMs: Math.max(1000, parseInt(config.kakaoPushIntervalMs, 10) || 5000),
    kakaoPushBatch: boundedInteger(config.kakaoPushBatch, 10, 1, 10),   // 代付接口 payment_urls maxItems=10
    // 链 27 分钟过期（实测 expires_at - submitted_at ≈ 1608s）。留 2 分钟余量。
    // ★ 出链率实测 82%、单号 40-60 秒 —— 链产出很快，「放在手里过期」是本层的主要风险，
    //   不是次要风险。扫描器 5 秒一轮，正常出链后 5 秒内就推走，这道闸只是兜底。
    kakaoPushMaxAgeMin: boundedInteger(config.kakaoPushMaxAgeMin, 25, 1, 27),
    kakaoPushMinQuota: boundedInteger(config.kakaoPushMinQuota, 1, 0, 1000),
    // ★ 2026-07-31 起【不再用次数上限】做终止条件 —— 链 25 分钟就会被超龄闸打成 expired，
    //   时间本身就是更准的上界；次数上限只会在链还剩十几分钟时提前放弃。
    //   这个键保留是为了老配置不报错，代码里已经没有读它的地方。
    kakaoPushMaxAttempts: boundedInteger(config.kakaoPushMaxAttempts, 3, 1, 10),
    // 单号退避基数：30s → 60s → 120s …… 封顶 32 倍（约 16 分钟），超龄闸接管
    kakaoPushRetryBackoffMs: Math.max(5000, parseInt(config.kakaoPushRetryBackoffMs, 10) || 30000),
    // ═══ 全局熔断（对方停止接单 / 限流 / 5xx）═══
    // ★ 这是全局状态，不是某个号的问题。按号各自退避的话，10 个号会各撞 10 次，
    //   把对方本来就满的队列继续砸。熔断期间整条推送线一个请求都不发。
    kakaoPushBreakerBaseMs: Math.max(5000, parseInt(config.kakaoPushBreakerBaseMs, 10) || 30000),
    kakaoPushBreakerMaxMs: Math.max(10000, parseInt(config.kakaoPushBreakerMaxMs, 10) || 300000),
    // Server-side UPI admission feeder. Disabled until explicitly enabled from the UPI panel.
    upipayAutoExtractEnabled: config.upipayAutoExtractEnabled === true,
    upipayAutoScope: config.upipayAutoScope === 'qualified' ? 'qualified' : 'block',
    upipayAutoDesiredState: config.upipayAutoDesiredState === 'paused' ? 'paused' : 'running',
    upipayAutoFillLimit: Math.max(0, parseInt(config.upipayAutoFillLimit, 10) || 0), // 0 follows upipayConcurrency
    upipayAutoScanIntervalMs: Math.max(1000, parseInt(config.upipayAutoScanIntervalMs, 10) || 2500),
    upipayAutoFoargeBackpressure: config.upipayAutoFoargeBackpressure === true,
    upipayAutoDeleteInvalid: config.upipayAutoDeleteInvalid === true,
    // Candidate admission is selection-only: rules define what can be selected, while the
    // durable one-shot batch in pipeline_state.json defines what may actually be admitted.
    upipayAutoCandidateMode: 'manual_pool',
    upipayAutoCandidatePolicy: config.upipayAutoCandidatePolicy
        && typeof config.upipayAutoCandidatePolicy === 'object'
        && !Array.isArray(config.upipayAutoCandidatePolicy)
        ? config.upipayAutoCandidatePolicy
        : null,
    longLinkPollIntervalMs: parseInt(config.longLinkPollIntervalMs, 10) || 5000,
    longLinkStuckMin: parseInt(config.longLinkStuckMin, 10) || 12,   // extracting 超过这么久还查不到 job=僵尸(多为关服务/重启后 sidecar job 丢失)→自动标失败可重试。正常提取几分钟出终态，12分钟保守不误杀

    // 长链自动补单器：开启后后台把区块里「从没提取过」且有 token 的号自动排进提取队列（不碰 done/extracting/failed）。
    longLinkAutoExtractEnabled: config.longLinkAutoExtractEnabled === true,                                   // 总开关，默认关（需手动开，避免一开就大批提取扣代理/触发风控）
    longLinkAutoFillLimit: Math.max(0, parseInt(config.longLinkAutoFillLimit, 10) || 0),                      // 每轮在途上限(queueLonglink+extracting 不超过它)；0=跟随 longLinkConcurrency
    longLinkAutoScanIntervalMs: Math.max(1000, parseInt(config.longLinkAutoScanIntervalMs, 10) || 3000),     // 自动补单扫描间隔

    // ── 高并发：多进程 sidecar 分摊单进程 GIL（并发固定 = longLinkConcurrency，不做自适应）──
    longLinkSidecarInstances: Math.max(1, Math.min(12, parseInt(config.longLinkSidecarInstances, 10) || 1)), // 起几个 sidecar 进程（端口 9200,9201,...）。单进程 GIL 顶不住高并发，多进程分摊。默认 1(兼容旧)

    // iDEAL 长链「拿到后自动送去外部提取 API」(ideal-manage /uapi)。拿到 pay.ideal.nl 长链 → 自动 POST /uapi/submitTask 下单。
    idealUploadEnabled: config.idealUploadEnabled !== false,                                        // 功能总开关（默认开；无 key 时自动不跑）
    idealUploadAuto: config.idealUploadAuto !== false,                                              // 拿到长链后自动送（默认开）；关掉则只能手动「送API」
    idealUploadApiBase: String(config.idealUploadApiBase || 'https://ideal-manage.231978.xyz').replace(/\/+$/, ''),
    idealUploadApiKey: config.idealUploadApiKey || '',                                              // X-API-Key（sk-...）；空则功能不启用
    idealUploadPollIntervalMs: Math.max(2000, parseInt(config.idealUploadPollIntervalMs, 10) || 8000),   // 后台自动送 sweep 间隔
    idealUploadTaskPollIntervalMs: Math.max(5000, parseInt(config.idealUploadTaskPollIntervalMs, 10) || 12000),   // 送出后回查任务成功/失败/退款的轮询间隔（API 建议 ≥5s）
    idealUploadBatchSize: Math.max(1, Math.min(50, parseInt(config.idealUploadBatchSize, 10) || 20)),    // 每轮最多送几个（API 单次上限 50）
    idealUploadMaxRetries: parseInt(config.idealUploadMaxRetries, 10) >= 0 ? parseInt(config.idealUploadMaxRetries, 10) : 3,   // 送失败(非余额/非无效链)自动重试上限
    idealUploadRequireIdealHost: config.idealUploadRequireIdealHost !== false,                      // 只送 https://pay.ideal.nl 域名的链（API 强约束；默认开，避免整单被拒）
    idealUploadSince: String(config.idealUploadSince || ''),                                        // 自动送水位线(ISO)：只自动送 idealAt >= 此刻的「新」链；留空=全部都自动送。手动「送API」不受此限。防启用瞬间把历史已拿链全刷掉扣爆余额
    // 外部 API 任务成功后自动回查账号是否升级成 Plus（付款到套餐生效有几秒~几十秒延迟，故延迟+重试）
    idealUploadAutoDetectPlus: config.idealUploadAutoDetectPlus !== false,                          // 默认开
    idealUploadPlusFirstDelayMs: Math.max(1000, parseInt(config.idealUploadPlusFirstDelayMs, 10) || 8000),   // 成功后首次检测前的等待
    idealUploadPlusGapMs: Math.max(3000, parseInt(config.idealUploadPlusGapMs, 10) || 15000),                // 每次重试间隔
    idealUploadPlusMaxTries: Math.max(1, parseInt(config.idealUploadPlusMaxTries, 10) || 6),                 // 最多检测几次（还没变 Plus 就放弃，记 free）

    longLinkAutoStart: config.longLinkAutoStart !== false,                       // pipeline 启动时自动拉起 sidecar（默认开 → 一键启动，无需单独开服务）
    longLinkSidecarDir: config.longLinkSidecarDir || '',                         // sidecar 项目 app 目录；空则不自启
    longLinkSidecarPython: config.longLinkSidecarPython || '',                   // sidecar 的 python.exe；空则用 <dir>/.venv/Scripts/python.exe
    deepDetectConcurrency: parseInt(config.deepDetectConcurrency, 10) || 6,      // 细检(0元试用)并发：跨账号并发安全，瓶颈在共享代理吞吐；默认 6，可在总览工具栏 ± 调（6-10 甜点位，太高易「待定」）

    // 注册后自动细检：后台扫描器定时补齐「没细检过」的号的 0 元试用资格（不内联、不拖慢注册）
    autoTrialCheck: config.autoTrialCheck !== false,                             // 总开关，默认开
    autoTrialCheckConcurrency: parseInt(config.autoTrialCheckConcurrency, 10) || 5,   // 自动细检并发（默认 5）
    autoTrialCheckInterval: parseInt(config.autoTrialCheckInterval, 10) || 30000,      // 扫描间隔 ms（默认 30s）

    // 注册后自动 momo 资格检测：越南 MoMo 只对约 2% 的号开放（服务端按号固定的 A/B 分组）。
    // ★ 默认【关】——与 autoTrialCheck 相反：momo 检测每号要建 checkout+init 两个请求，
    //   且越南出口 403 敏感（约 1/4 的 IP 干净），确认区块可用后再手动打开，避免默认就烧代理。
    autoMomoCheck: config.autoMomoCheck === true,                                  // 总开关，默认关
    autoMomoCheckConcurrency: parseInt(config.autoMomoCheckConcurrency, 10) || 3,  // 检测并发（默认 3，403 敏感要保守）
    autoMomoCheckInterval: parseInt(config.autoMomoCheckInterval, 10) || 60000,    // 扫描间隔 ms（默认 60s，比细检慢一倍）
    // ★ 取号顺序与失败归因（CF 拦截强弱本就随时间波动，家宽出口 IP 质量也参差 ——
    //   环境类失败是本项目的常态，必须按常态设计，不能按异常设计）
    momoCheckFreshWindowMin: parseInt(config.momoCheckFreshWindowMin, 10) || 1440, // 「新号」窗口：注册后多少分钟内算新号（默认 24h）
    momoCheckFreshQuota: Number(config.momoCheckFreshQuota) || 0.7,                // 新号占每轮名额的比例（其余给老号补白，防饿死）
    momoCheckMaxBlocked: parseInt(config.momoCheckMaxBlocked, 10) || 12,           // 单号连续被环境拦多少次后暂时搁置（可「重置待定」复活）
    momoCheckBackoffMaxMin: parseInt(config.momoCheckBackoffMaxMin, 10) || 15,     // 整轮零结论时的全局熔断退避上限（分钟）
    momoConcurrency: parseInt(config.momoConcurrency, 10) || 3,                    // MoMo 提链池并发
    paypalConcurrency: parseInt(config.paypalConcurrency, 10) || 3,                // PayPal 提链池并发（pipeline.js 启动处早就在读，一直读到 undefined 走兜底 3）
    // ── PayPal 同步链路的进度旁路（2026-07-29）──
    // 同步 probe 没有 job 可查，进度靠 probeId 从 sidecar 内存表增量取。
    paypalProgressPollMs: parseInt(config.paypalProgressPollMs, 10) || 3000,       // 进度轮询间隔（要驱动「实时日志」，比别的池快一档）
    paypalNoProgressWarnMs: parseInt(config.paypalNoProgressWarnMs, 10) || 90000,  // 静默多久才吭一声（取代原来每 60s 无条件的空心跳）
    // 日志密度：round=每轮+每次失败(约 24 条/号，默认) | step=每一步(约 190 条/号，仅排查用) | quiet=只更新表格里的 stage 不打日志。
    // ★别默认开 step：cursorLogs 是【全池共享】的环形缓冲，11 并发 × 190 条会把注册/升级/RT 的日志全挤掉。
    paypalLogVerbosity: ['round', 'step', 'quiet'].includes(String(config.paypalLogVerbosity || '').toLowerCase())
        ? String(config.paypalLogVerbosity).toLowerCase() : 'round',
    // ── PayPal 代理接线（2026-07-29 补）──
    // 此前这几个键**一个都不在白名单里**，paypalProvider.js 读到的恒是 undefined，于是 body 里
    // 从来没有代理字段 → sidecar 回落到 th_proxy_pool.txt 那 30 行写死的 cliproxy 串。
    // 用户在 GUI 里切 kookeey、改地区，对 PayPal 全部无效 —— 这就是「IP 池只有 30 个」的根因。
    // 两条线各自可覆盖、可改区（业务约束：全程 TH；优惠注入区将来要能手动切 TH↔JP）。
    paypalProxy: String(config.paypalProxy || '').trim(),                          // 主线路覆盖串（留空=按供应商模板+地区派生）
    paypalCheckoutRegion: String(config.paypalCheckoutRegion || 'TH').toUpperCase(), // 主线路出口地区
    paypalPromoProxy: String(config.paypalPromoProxy || '').trim(),                // 优惠注入覆盖串
    paypalPromoRegion: String(config.paypalPromoRegion || 'TH').toUpperCase(),     // ★优惠注入出口地区，手动切（TH/JP/…）
    paypalBillingCountry: String(config.paypalBillingCountry || 'DE').toUpperCase(), // 账单国（config.json 里早就写了 DE，之前读不到）
    paypalTargetAmount: String(config.paypalTargetAmount != null ? config.paypalTargetAmount : '0'), // 金额闸门，非 0 被 sidecar 拒
    paypalMaxAutoRetries: parseInt(config.paypalMaxAutoRetries, 10) >= 0 ? parseInt(config.paypalMaxAutoRetries, 10) : 2,
    // ── PayPal 审批（后半端：消费提炼产出的 ba_token，真实 Chrome 跑完签约）──
    // ★并发默认 2 而不是跟提炼一样 85：每个任务要一个 Chrome(~300-500MB)，且每次租号真花钱。
    ppApproveEnabled: config.ppApproveEnabled !== false,                            // 池启用总开关
    ppApproveConcurrency: parseInt(config.ppApproveConcurrency, 10) > 0 ? parseInt(config.ppApproveConcurrency, 10) : 2,
    // ★不自动重试：审批一旦跑到建号那步就产生了真实副作用（账号已建、号已花钱），
    //   失败原因又多半是系统性的（卡/选择器），自动重试只会成倍烧钱。失败停下等人看。
    ppApproveAuto: config.ppApproveAuto === true,                                  // 提炼出链后自动入审批队列（默认关）
    ppApproveSmsVendor: String(config.ppApproveSmsVendor || 'hero').toLowerCase(),  // hero | bower
    ppApproveSmsMaxPrice: Number(config.ppApproveSmsMaxPrice) > 0 ? Number(config.ppApproveSmsMaxPrice) : 0,
    ppApproveOtpWaitSec: parseInt(config.ppApproveOtpWaitSec, 10) > 0 ? parseInt(config.ppApproveOtpWaitSec, 10) : 240,
    ppApproveExitTries: parseInt(config.ppApproveExitTries, 10) > 0 ? parseInt(config.ppApproveExitTries, 10) : 4,
    ppApproveShowWindow: config.ppApproveShowWindow === true,                       // 调试用：显示浏览器窗口
    ppApproveExtensionDir: String(config.ppApproveExtensionDir || 'D:\\XMX\\XM\\paypal-autofill'),
    momoPollIntervalMs: parseInt(config.momoPollIntervalMs, 10) || 5000,           // 后台轮询间隔
    momoStuckMin: parseInt(config.momoStuckMin, 10) || 12,                         // 僵尸判定：单次提链最长可达 60s+（内部换 8 次出口），12 分钟保守不误杀
    momoMaxAutoRetries: parseInt(config.momoMaxAutoRetries, 10) || 2,              // 可重试类失败的自动重试上限
    // ── MoMo 资格独占 ──
    // 检出 momoEligible='yes' 即把号从长链/Pix/PayPal/UPI 四个提取区块摘出，且不再被它们选中。
    // 理由：全池仅约 2% 有资格，被别的渠道消耗掉（提链→foarge 代付→变 Plus）后 MoMo 链永久
    // 提不出来（上游恒回 "User is already paid"）。实测已有 9 个被吃进去、1 个确定性浪费。
    momoExclusiveEnabled: config.momoExclusiveEnabled !== false,                   // 总开关(默认开)
    momoExclusiveAllowForce: config.momoExclusiveAllowForce !== false,             // 手动后门总闸，置 false 可彻底焊死
    momoExclusiveSweepMs: parseInt(config.momoExclusiveSweepMs, 10) || 300000,     // 存量清扫周期(提链中的号要等它跑完才能摘)
    momoCheckoutRegion: config.momoCheckoutRegion || 'VN',                         // checkout 阶段出口地区
    momoProviderRegion: config.momoProviderRegion || 'VN',                         // provider 阶段出口地区
    momoServiceUrl: config.momoServiceUrl || '',                                   // 空则回落 longLinkServiceUrl

    // ── 菲律宾 GCash 提链（2026-08-04）──
    // ★ 与其它五条线的根本差异：处理器是 **Adyen** 不是 Stripe，会话是 oaics_ 不是 cs_。
    //   资格判据是 checkout 回包的 custom_payment_methods（cpmt_），**不是** payment_method_types
    //   —— 后者恒为 ["card","link"]，只看它必然得出"没有 GCash"的错误结论（已踩过一整轮）。
    gcashEnabled: config.gcashEnabled !== false,                                   // 区块总开关（默认开）
    gcashConcurrency: parseInt(config.gcashConcurrency, 10) || 3,                  // 提链池并发（起步 3，稳定后再调）
    gcashPollIntervalMs: parseInt(config.gcashPollIntervalMs, 10) || 5000,         // 后台轮询间隔
    gcashStuckMin: parseInt(config.gcashStuckMin, 10) || 12,                       // 僵尸判定：正常提链约 24s，12 分钟保守不误杀
    // 默认 4：sidecar 步骤内换出口连打之后仍失败时，池侧再给几次带抖动退避的整单重排。
    gcashMaxAutoRetries: parseInt(config.gcashMaxAutoRetries, 10) >= 0 ? parseInt(config.gcashMaxAutoRetries, 10) : 4,
    // 建单/confirm/start 的出口，必须 PH：2026-08-06 起出口国须与账单国一致，
    // US 出口会被拒 "Billing country must match request country"（8-04「US/PH/JP 都能」已失效）。
    gcashCheckoutRegion: String(config.gcashCheckoutRegion || 'PH').toUpperCase(),
    // ★ 促销注入专用出口，必须与上面分开：同区 PH 挂不上 plus-1-month-free，
    //   金额打不到 0 就白提。与 iDEAL 的 VN、UPI 的 JP 是同一类「魔法区」。
    //   面板可实时切换（写 config.json → liveConfig 按文件签名逐出缓存 → 下一单即生效，不用重启）。
    //   ★ 必须校验两位国家码再放行：sidecar 的 normalize_proxy_region 对非法值返回空串，
    //     于是促销跳会静默回落到环境变量，面板显示的地区和实际跑的地区对不上 ——
    //     那正是「改了促销地区完全不生效」这一类问题最难查的形态。乱值一律退回已验证的 JP。
    gcashPromoRegion: (/^[A-Za-z]{2}$/.test(String(config.gcashPromoRegion || '').trim())
        ? String(config.gcashPromoRegion).trim().toUpperCase() : 'JP'),
    gcashPromoProxy: String(config.gcashPromoProxy || '').trim(),                  // 促销跳覆盖串（留空=按供应商模板派生）
    gcashServiceUrl: config.gcashServiceUrl || '',                                 // 空则回落 longLinkServiceUrl
    // ═══ 印尼 GoPay（2026-08-10 接入）═══
    // ★ 必须进白名单：不在这儿列出来的键，proxyConfig 那边读到的恒为 undefined ——
    //   2026-07-29 的 paypalProxy 就是这么静默失效的（覆盖串永远没生效，出口一直是
    //   sidecar 自带的 th_proxy_pool.txt 那 30 条钉死的串，「换供应商」对它完全无效）。
    gopayProxy: String(config.gopayProxy || '').trim(),                            // 主线路覆盖串（留空=按供应商模板派生 ID）
    gopayCheckoutRegion: String(config.gopayCheckoutRegion || 'ID').toUpperCase(), // 建单/init/taxes/PM/confirm/approve 全程出口
    // 促销注入专用出口，与主线路分开。乱值退回已验证的 TH（理由同上面 gcashPromoRegion 那段）
    gopayPromoRegion: (/^[A-Za-z]{2}$/.test(String(config.gopayPromoRegion || '').trim())
        ? String(config.gopayPromoRegion).trim().toUpperCase() : 'TH'),
    gopayPromoProxy: String(config.gopayPromoProxy || '').trim(),                  // 促销跳覆盖串（留空=按供应商模板派生）
    // 扫码激活是长驻任务，和上面的几十秒提链池分开限流。界面可以实时修改并持久化。
    // ★ 不设内置上限（同 upipayConcurrency / longLinkSidecarMaxConcurrent 的既定做法）：
    //   真正的天花板是本机资源和代理套餐，由使用者在面板上自己判断，代码不替他封顶。
    //   只保留下限 1 —— 那是「有效值」而不是「限额」，0 或负数会让池永远不出队。
    gcashProtocolConcurrency: Math.max(1, parseInt(config.gcashProtocolConcurrency, 10) || 5),
    gcashBrowserConcurrency: Math.max(1, parseInt(config.gcashBrowserConcurrency, 10) || 2),
    gcashProtocolQrExpireSec: Math.max(60, Math.min(3600, parseInt(config.gcashProtocolQrExpireSec, 10) || 1800)),
    // GCash 页面自身实测 expireSeconds=300；多留 30 秒只用于接住边缘回跳，不冒充二维码更长寿。
    gcashBrowserWaitMs: Math.max(60000, Math.min(900000, parseInt(config.gcashBrowserWaitMs, 10) || 330000)),
    gcashBrowserEgressRetries: Math.max(1, Math.min(8, parseInt(config.gcashBrowserEgressRetries, 10) || 3)),
    gcashChromePath: String(config.gcashChromePath || '').trim(),
    gcashActivationPollIntervalMs: Math.max(1000, parseInt(config.gcashActivationPollIntervalMs, 10) || 2000),
    gcashVerifyPollIntervalMs: Math.max(5000, parseInt(config.gcashVerifyPollIntervalMs, 10) || 15000),
    gcashVerifyMaxTries: Math.max(1, Math.min(12, parseInt(config.gcashVerifyMaxTries, 10) || 6)),

    // 历史数据保留策略：registration_attempts / token_write_intents 原本只增不减，
    // 库越大 exportLegacyState/全表读越慢，是「越跑越卡」的长期来源。
    // ★只清理已落终态(committed/failed/cancelled)且超过保留期的行；非终态/在租约的一律不动。
    // 设 0 = 关闭该表的清理。默认 attempts 留 7 天、token intents 留 3 天。
    registrationRetentionDays: config.registrationRetentionDays === 0
        ? 0 : (parseInt(config.registrationRetentionDays, 10) || 7),
    tokenIntentRetentionDays: config.tokenIntentRetentionDays === 0
        ? 0 : (parseInt(config.tokenIntentRetentionDays, 10) || 3),
    stateRetentionIntervalMs: parseInt(config.stateRetentionIntervalMs, 10) || 3600000,   // 清理周期，默认 1 小时

    // foarge CDK 代付(2026-07-19)。UPI 出货→自动送 foarge 大厅工人扫码→号变 Plus
    // ★ 这些字段必须显式在这里导出,否则 getConfig() 返 undefined → UI 开关反弹、pipeline 判断永远走 fallback 分支
    foargeApiBase: config.foargeApiBase || 'https://foarge.com/api/publisher/v1',
    foargeApiTimeoutMs: parseInt(config.foargeApiTimeoutMs, 10) > 0 ? parseInt(config.foargeApiTimeoutMs, 10) : 30000,
    foargeReadSocketTimeoutMs: parseInt(config.foargeReadSocketTimeoutMs, 10) > 0 ? parseInt(config.foargeReadSocketTimeoutMs, 10) : 12000,
    foargeWriteSocketTimeoutMs: parseInt(config.foargeWriteSocketTimeoutMs, 10) > 0 ? parseInt(config.foargeWriteSocketTimeoutMs, 10) : 65000,
    foargeReadTotalTimeoutMs: parseInt(config.foargeReadTotalTimeoutMs, 10) > 0 ? parseInt(config.foargeReadTotalTimeoutMs, 10) : 15000,
    foargeWriteTotalTimeoutMs: parseInt(config.foargeWriteTotalTimeoutMs, 10) > 0 ? parseInt(config.foargeWriteTotalTimeoutMs, 10) : 70000,
    foargeReadMaxAttempts: parseInt(config.foargeReadMaxAttempts, 10) > 0 ? Math.min(5, parseInt(config.foargeReadMaxAttempts, 10)) : 1,
    foargeConnectTimeoutMs: parseInt(config.foargeConnectTimeoutMs, 10) > 0 ? parseInt(config.foargeConnectTimeoutMs, 10) : 8000,
    foargeQueueTimeoutMs: parseInt(config.foargeQueueTimeoutMs, 10) > 0 ? parseInt(config.foargeQueueTimeoutMs, 10) : 8000,
    foargeTotalTimeoutMs: parseInt(config.foargeTotalTimeoutMs, 10) > 0 ? parseInt(config.foargeTotalTimeoutMs, 10) : 30000,
    foargeMaxSockets: parseInt(config.foargeMaxSockets, 10) > 0 ? parseInt(config.foargeMaxSockets, 10) : 8,
    foargeAgentMaxSockets: parseInt(config.foargeAgentMaxSockets, 10) > 0 ? parseInt(config.foargeAgentMaxSockets, 10) : 12,
    // 二阶段调度器。legacy foargeMaxSockets 保持 8，避免旧进程热加载 config.js 后半升级；
    // 新 pipeline 只读下面的新字段，从 12 路起步，达到验收条件后升到 16 路。
    foargeDispatchBase,
    foargeDispatchMax,
    foargeSubmitReserve,
    foargeReadConcurrency: parseInt(config.foargeReadConcurrency, 10) > 0 ? parseInt(config.foargeReadConcurrency, 10) : 4,
    foargeReconcileConcurrency: parseInt(config.foargeReconcileConcurrency, 10) > 0 ? parseInt(config.foargeReconcileConcurrency, 10) : 2,
    foargeWriteAgentMaxSockets,
    foargeReadAgentMaxSockets: parseInt(config.foargeReadAgentMaxSockets, 10) > 0 ? parseInt(config.foargeReadAgentMaxSockets, 10) : 4,
    foargeCapacityRefreshMs: parseInt(config.foargeCapacityRefreshMs, 10) > 0 ? parseInt(config.foargeCapacityRefreshMs, 10) : 5000,
    foargeCapacityStaleMs: parseInt(config.foargeCapacityStaleMs, 10) > 0 ? parseInt(config.foargeCapacityStaleMs, 10) : 15000,
    foargeGateRefreshMs: parseInt(config.foargeGateRefreshMs, 10) > 0 ? parseInt(config.foargeGateRefreshMs, 10) : 15000,
    foargeGateDecisionTtlMs: parseInt(config.foargeGateDecisionTtlMs, 10) > 0 ? parseInt(config.foargeGateDecisionTtlMs, 10) : 30000,
    foargeActivePollIntervalMs: parseInt(config.foargeActivePollIntervalMs, 10) > 0 ? parseInt(config.foargeActivePollIntervalMs, 10) : 5000,
    foargeActivePollBatchSize: parseInt(config.foargeActivePollBatchSize, 10) > 0 ? parseInt(config.foargeActivePollBatchSize, 10) : 8,
    foargeActiveSnapshotIntervalMs: parseInt(config.foargeActiveSnapshotIntervalMs, 10) > 0 ? parseInt(config.foargeActiveSnapshotIntervalMs, 10) : 10000,
    foargeReadySnapshotIntervalMs: parseInt(config.foargeReadySnapshotIntervalMs, 10) > 0 ? parseInt(config.foargeReadySnapshotIntervalMs, 10) : 2000,
    foargeReadyProbePerTaskLimit: parseInt(config.foargeReadyProbePerTaskLimit, 10) > 0 ? parseInt(config.foargeReadyProbePerTaskLimit, 10) : 8,
    foargeReadyFallbackPollMs: parseInt(config.foargeReadyFallbackPollMs, 10) > 0 ? parseInt(config.foargeReadyFallbackPollMs, 10) : 30000,
    foargePendingPollMs: parseInt(config.foargePendingPollMs, 10) > 0 ? parseInt(config.foargePendingPollMs, 10) : 15000,
    foargeAssignedPollMs: parseInt(config.foargeAssignedPollMs, 10) > 0 ? parseInt(config.foargeAssignedPollMs, 10) : 10000,
    foargeReadyProbeDelaysMs: Array.isArray(config.foargeReadyProbeDelaysMs)
        ? config.foargeReadyProbeDelaysMs.map(v => Math.max(100, parseInt(v, 10) || 0)).filter(Boolean).slice(0, 10)
        : [500, 1500, 3500, 7500, 15500],
    foargeAuditPollIntervalMs: parseInt(config.foargeAuditPollIntervalMs, 10) > 0 ? parseInt(config.foargeAuditPollIntervalMs, 10) : 120000,
    foargePromotionMinSamples: parseInt(config.foargePromotionMinSamples, 10) > 0 ? parseInt(config.foargePromotionMinSamples, 10) : 100,
    foargePromotionMinAgeMs: parseInt(config.foargePromotionMinAgeMs, 10) > 0 ? parseInt(config.foargePromotionMinAgeMs, 10) : 1800000,
    foargePromotionFreezeMs: parseInt(config.foargePromotionFreezeMs, 10) > 0 ? parseInt(config.foargePromotionFreezeMs, 10) : 1800000,
    foargeAdaptiveWindowMs: parseInt(config.foargeAdaptiveWindowMs, 10) > 0 ? parseInt(config.foargeAdaptiveWindowMs, 10) : 300000,
    foargeCounterSyncMinAgeMs: parseInt(config.foargeCounterSyncMinAgeMs, 10) >= 0 ? parseInt(config.foargeCounterSyncMinAgeMs, 10) : 5000,
    foargeCounterConvergenceMinSnapshots: parseInt(config.foargeCounterConvergenceMinSnapshots, 10) >= 2 ? parseInt(config.foargeCounterConvergenceMinSnapshots, 10) : 5,
    foargeCounterConvergenceMinAgeMs: parseInt(config.foargeCounterConvergenceMinAgeMs, 10) >= 0 ? parseInt(config.foargeCounterConvergenceMinAgeMs, 10) : 30000,
    foargePostSafeRetries: parseInt(config.foargePostSafeRetries, 10) >= 0 ? parseInt(config.foargePostSafeRetries, 10) : 2,
    foargeReconcileAttempts: parseInt(config.foargeReconcileAttempts, 10) > 0 ? parseInt(config.foargeReconcileAttempts, 10) : 3,
    foargeSubmitStableReads: parseInt(config.foargeSubmitStableReads, 10) > 0 ? parseInt(config.foargeSubmitStableReads, 10) : 2,
    foargeReconcileDelayMs: parseInt(config.foargeReconcileDelayMs, 10) >= 0 ? parseInt(config.foargeReconcileDelayMs, 10) : 1500,
    foargeListMaxPages: parseInt(config.foargeListMaxPages, 10) > 0 ? parseInt(config.foargeListMaxPages, 10) : 50,
    foargeReconcileListMaxPages: parseInt(config.foargeReconcileListMaxPages, 10) > 0 ? parseInt(config.foargeReconcileListMaxPages, 10) : 3,
    foargeLinkTtlSec: parseInt(config.foargeLinkTtlSec, 10) > 0 ? parseInt(config.foargeLinkTtlSec, 10) : 300,
    foargeMinSubmitTtlSec: parseInt(config.foargeMinSubmitTtlSec, 10) > 0 ? parseInt(config.foargeMinSubmitTtlSec, 10) : 180,
    // New work can either reserve FOARGE first or extract the short-lived link first.
    foargeDispatchMode,
    foargeCreateBeforeUpi: foargeDispatchMode === 'create_first',
    foargePrecreateLeaseMs: parseInt(config.foargePrecreateLeaseMs, 10) >= 300000
        ? parseInt(config.foargePrecreateLeaseMs, 10) : 300000,
    foargePrecreateMaxWaitMs: parseInt(config.foargePrecreateMaxWaitMs, 10) >= 300000
        ? parseInt(config.foargePrecreateMaxWaitMs, 10) : 600000,
    // 0 follows the operator-controlled FOARGE max write limit.
    foargePrecreateMaxOutstanding: parseInt(config.foargePrecreateMaxOutstanding, 10) > 0
        ? Math.min(64, parseInt(config.foargePrecreateMaxOutstanding, 10)) : 0,
    // Readiness reads are isolated from ordinary gate/capacity reads because FOARGE can take >60s.
    foargeReadyReadTimeoutMs: parseInt(config.foargeReadyReadTimeoutMs, 10) >= 30000
        ? Math.min(180000, parseInt(config.foargeReadyReadTimeoutMs, 10)) : 90000,
    foargeAutoDispatch: config.foargeAutoDispatch !== false,                     // 出货即自动送 foarge(默认开)
    foargeAutoQrRefresh: config.foargeAutoQrRefresh === true,                    // QR 过期自动重提(默认关,自动重提易恶性循环)
    foargeQrMaxRefresh: parseInt(config.foargeQrMaxRefresh, 10) || 2,            // 单号 QR 自动重刷上限(超上限强制放弃)
    // ★ 自动补送(把失败/等待号后台重新送 foarge)。默认关 —— UPI 长链仅 5min 有效,补送老链接=送死链。
    //   开启时也只补送 upipayAt 在 foargeResendMaxAgeSec(默认60s)内的号;新号即时送和手动送不受此限。
    foargeAutoResend: config.foargeAutoResend === true,                          // 自动补送总开关(默认关,前端可开)
    foargeResendMaxAgeSec: parseInt(config.foargeResendMaxAgeSec, 10) || 60,     // 自动补送只放行链接年龄 ≤ 此秒数的号
    foargePollIntervalMs: parseInt(config.foargePollIntervalMs, 10) || 10000,    // foarge task 状态轮询间隔
    foargePoolRefreshIntervalMs: parseInt(config.foargePoolRefreshIntervalMs, 10) || 60000,
    foargePlusVerifyMaxMin: parseInt(config.foargePlusVerifyMaxMin, 10) || 30,   // paid_pending_plus 后最长复查时间(分钟)
    // ── QR 过期/取消后的「其实已付款」复查 ──
    // foarge 判死的号里实测有 1.7% 其实已经是 Plus，全靠人工重提才偶然发现（平均埋 13.7 小时）。
    // 延迟取 11 分钟：实测付款延迟从进大厅起算 max 10.67 分钟(n=637)，11 分钟覆盖 100%。
    foargeMissedPlusEnabled: config.foargeMissedPlusEnabled !== false,           // 总开关(默认开)
    foargeMissedPlusSweepMs: parseInt(config.foargeMissedPlusSweepMs, 10) || 60000,    // 轮询周期
    foargeMissedPlusGraceMs: parseInt(config.foargeMissedPlusGraceMs, 10) || 360000,   // QR 死后多久做第 1 次复查(6 分钟)
    foargeMissedPlusRetryGapMs: parseInt(config.foargeMissedPlusRetryGapMs, 10) || 1200000,  // 第 2 次顺延(20 分钟)
    foargeMissedPlusMaxChecks: parseInt(config.foargeMissedPlusMaxChecks, 10) || 2,    // 查几次就放弃
    foargeMissedPlusMaxAgeMs: parseInt(config.foargeMissedPlusMaxAgeMs, 10) || 2592000000,  // 锚点超过 30 天不查
    foargeMissedPlusBatch: parseInt(config.foargeMissedPlusBatch, 10) || 4,            // 每轮限量，防止存量号齐射抢代理
    foargeMissedPlusRequireAssigned: config.foargeMissedPlusRequireAssigned === true,  // 收紧到「有接单记录」才查(默认否)
    foargePlusVerifyIntervalMs: parseInt(config.foargePlusVerifyIntervalMs, 10) || 30000,  // Plus 复查间隔
    foargePlusOvertimeVerifyIntervalMs: parseInt(config.foargePlusOvertimeVerifyIntervalMs, 10) || 300000,
    // foargePbkPool 直接从 config.json 读(foargePbkPool.js 模块自己 fs.readFileSync);不必在此列
};

// ★ 挂在 module.exports 【之后】而不是写进那个字面量里：
//   test_config_contract.js 用"缩进 4 空格的 key:"这条正则刮配置键名，
//   把一个函数混进去会让它被当成一个配置项。
module.exports.resolveStatsigReplayGate = resolveStatsigReplayGate;
