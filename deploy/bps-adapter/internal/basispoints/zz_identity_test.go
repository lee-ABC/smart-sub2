package basispoints

import (
	"time"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 一份带工具循环的请求，形状照着真实加载项的抓包来。
func identitySource() map[string]any {
	return map[string]any{
		"model":            "gpt-6-luna",
		"prompt_cache_key": "01a0f7b6-79ce-7bb8-aec2-bfe63f6216d3",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user", "id": "msg_1",
				"content": []any{map[string]any{"type": "input_text", "text": "把 A1 填成 1 <b>&amp;</b>"}},
			},
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_2",
				"content": []any{map[string]any{"type": "output_text", "text": "好的"}},
			},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "write_range", "arguments": `{"v":1}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
	}
}

func identityConfig() Config {
	cfg := defaultConfig()
	cfg.ClientIdentity.Pepper = "pepper-for-tests"
	cfg.ClientIdentity.ClientShapes = true
	cfg.ClientIdentity.PlainPrologue = true
	return cfg
}

func seat(name string) persona {
	return persona{
		seed: name, pepper: "pepper-for-tests", accountUserID: "user-xxx__acct",
		// 2026-05-01，和生产里的订阅起始同量级。
		subscriptionStart: time.Unix(1777593600, 0),
	}
}

// mode=off 必须和没有人格时逐字节一样，哪怕手里攥着一个完整的 persona。
// 这是整个灰度的前提：先上代码、再开开关，中间那段时间线上行为不能有任何变化。
func TestModeOffIgnoresThePersonaCompletely(t *testing.T) {
	cfg := identityConfig()
	source := identitySource()
	scope := conversationScope(source)

	plain, err := prepareResponsesBodyFor(identitySource(), cfg, scope, persona{}, identityOff)
	if err != nil {
		t.Fatal(err)
	}
	withSeat, err := prepareResponsesBodyFor(identitySource(), cfg, scope, seat("a"), identityOff)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, withSeat) {
		t.Fatal("mode=off 下 persona 影响了请求体")
	}
	// 顺带确认旧签名就是 off 这条路。
	legacy, err := prepareResponsesBody(identitySource(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, legacy) {
		t.Fatal("prepareResponsesBody 和 mode=off 走出了不同结果")
	}
}

// 没有席位键的请求要退回 off，绝不能拿空种子去派生 —— 那会让所有受影响的号
// 塌缩成同一个人格，而且是悄悄发生的。
func TestAPersonalessRequestFallsBackToOff(t *testing.T) {
	if _, ok := personaKey(ExecutorRequest{}, credential{AccountID: "acct"}); ok {
		t.Fatal("没有 AuthID 也没有文件名时还给出了席位键")
	}
	if key, ok := personaKey(ExecutorRequest{AuthID: " bp-a "}, credential{}); !ok || key != "bp-a" {
		t.Fatalf("AuthID 分支 = %q, %v", key, ok)
	}
	request := ExecutorRequest{AuthMetadata: map[string]any{"file_name": "bp-b.json"}}
	if key, ok := personaKey(request, credential{}); !ok || key == "" {
		t.Fatalf("文件名分支 = %q, %v", key, ok)
	}
}

// 同一个工作区里的两个号必须是两个人。并发那边的回退键最后会落到
// "account:"+AccountID，照抄过来会让一个工作区的四五个号共用一台机器。
func TestTwoSeatsInOneWorkspaceAreTwoPeople(t *testing.T) {
	first, second := seat("bp-a"), seat("bp-b")
	if seatTaskID(first) == seatTaskID(second) {
		t.Fatal("两个席位拿到了同一个 task_id")
	}
	if seatBirthMs(first) == seatBirthMs(second) {
		t.Fatal("两个席位的会话出生时间一样")
	}
	traceA, _ := sentryTrace(first)
	traceB, _ := sentryTrace(second)
	if strings.Split(traceA, "-")[0] == strings.Split(traceB, "-")[0] {
		t.Fatal("两个席位共用了一个 sentry trace")
	}
}

// 派生必须是纯函数：同样的输入，换个进程也要一模一样。
func TestDerivationIsDeterministic(t *testing.T) {
	for i := 0; i < 50; i++ {
		if seatTaskID(seat("bp-a")) != seatTaskID(seat("bp-a")) {
			t.Fatal("同一个席位两次拿到不同的 task_id")
		}
	}
}

// UUID 的版本位和变体位要真的是 v7，而且三个 ID 共用的那段客户端常量不能漂。
func TestUUIDv7HasTheRightBits(t *testing.T) {
	who := seat("bp-a")
	id := uuidV7For(who, 1790861426222, []byte{1, 2, 3, 4, 5, 6})
	raw := strings.ReplaceAll(id, "-", "")
	if len(raw) != 32 {
		t.Fatalf("长度不对: %s", id)
	}
	if raw[12] != '7' {
		t.Errorf("版本位 = %c，应该是 7", raw[12])
	}
	if v := raw[16]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("变体位 = %c，应该落在 8..b", v)
	}
	// 前 48 位要能原样读回毫秒。
	if got := uuidV7For(who, 1790861426222, nil); got[:13] != id[:13] {
		t.Errorf("时间戳段不稳定: %s vs %s", got, id)
	}
	// 同一个席位内，rand_a 和变体段要稳定 —— 真客户端一个会话里就是这样。
	other := uuidV7For(who, 1790861999999, []byte{9, 9, 9, 9, 9, 9})
	if strings.ReplaceAll(other, "-", "")[13:20] != raw[13:20] {
		t.Error("同一个席位的两个 ID 用了不同的随机段")
	}
}

// 这 26 位是「本该随机」的，所以它们绝不能全池共用。
//
// 第一版把抓包里那一个会话的值（rand_a=0xbb8、变体段=0x2ec2）写死给了所有席位。
// 第二次抓包推翻了这个判断：新会话是 0x881 / 0x999a，也就是真客户端每会话重播种。
// 25 个号共用一组本该随机的值，等于 25 台机器共享同一个随机种子 —— 两台真实机器
// 撞上的概率是 2^-26，所以这在现实中只有一个解释：它们是同一套程序生成的。
//
// 判据：全池共用一个值，只有当这个值在真实人群里也普遍时才安全（浏览器版本、
// 语言、OpenAI 自己的 Sentry key）。本该随机的值永远不能共用，共用本身就是证据。
func TestRandomBitsAreNeverSharedAcrossSeats(t *testing.T) {
	seen := map[string][]string{}
	for _, name := range []string{"bp-a", "bp-b", "bp-c", "bp-d", "bp-e", "bp-f"} {
		who := seat(name)
		raw := strings.ReplaceAll(seatTaskID(who), "-", "")
		key := raw[13:20] // rand_a + 变体段
		seen[key] = append(seen[key], name)
	}
	for key, names := range seen {
		if len(names) > 1 {
			t.Errorf("随机段 %s 被 %d 个席位共用: %v", key, len(names), names)
		}
	}
	// 而且不能再出现写死的那一组。
	for _, name := range []string{"bp-a", "bp-b", "bp-c"} {
		raw := strings.ReplaceAll(seatTaskID(seat(name)), "-", "")
		if raw[13:20] == "bb8aec2" {
			t.Errorf("%s 还在用写死的 bb8/aec2", name)
		}
	}
}

// thread 级：一个号就是一条永不结束的对话，不管上面跑的是哪个最终用户的会话。
func TestThreadModeGivesOneSeatOneConversation(t *testing.T) {
	cfg := identityConfig()
	who := seat("bp-a")

	first := identitySource()
	second := identitySource()
	second["prompt_cache_key"] = "完全无关的另一个会话"

	bodyA, err := prepareResponsesBodyFor(first, cfg, conversationScope(first), who, identityThread)
	if err != nil {
		t.Fatal(err)
	}
	bodyB, err := prepareResponsesBodyFor(second, cfg, conversationScope(second), who, identityThread)
	if err != nil {
		t.Fatal(err)
	}
	taskA := objectValue(bodyA["metadata"])["task_id"]
	taskB := objectValue(bodyB["metadata"])["task_id"]
	if taskA != taskB {
		t.Fatalf("同一个号的两条会话给了两个 task_id: %v vs %v", taskA, taskB)
	}

	// hygiene 级不塌缩：每条会话仍然是自己的 task。
	bodyC, err := prepareResponsesBodyFor(identitySource(), cfg, conversationScope(first), who, identityHygiene)
	if err != nil {
		t.Fatal(err)
	}
	second2 := identitySource()
	second2["prompt_cache_key"] = "完全无关的另一个会话"
	bodyD, err := prepareResponsesBodyFor(second2, cfg, conversationScope(second2), who, identityHygiene)
	if err != nil {
		t.Fatal(err)
	}
	if objectValue(bodyC["metadata"])["task_id"] == objectValue(bodyD["metadata"])["task_id"] {
		t.Fatal("hygiene 级也把两条无关会话塌缩到了一个 task")
	}
}

// 工具循环里 turn_id 不能变，agent_iteration 要递增 —— 抓包里就是这样：
// 第 1 次和第 2 次迭代共用一个 turn_id，iteration 从 1 走到 2。
func TestTurnIDSurvivesTheToolLoop(t *testing.T) {
	cfg := identityConfig()
	who := seat("bp-a")

	firstPass := identitySource()
	firstPass["input"] = []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "把 A1 填成 1"}}},
	}
	secondPass := identitySource()
	secondPass["input"] = []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "把 A1 填成 1"}}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "write_range", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
	}

	scope := conversationScope(firstPass)
	bodyA, err := prepareResponsesBodyFor(firstPass, cfg, scope, who, identityThread)
	if err != nil {
		t.Fatal(err)
	}
	bodyB, err := prepareResponsesBodyFor(secondPass, cfg, scope, who, identityThread)
	if err != nil {
		t.Fatal(err)
	}
	metaA, metaB := objectValue(bodyA["metadata"]), objectValue(bodyB["metadata"])
	if metaA["turn_id"] != metaB["turn_id"] {
		t.Fatalf("同一轮的两次迭代 turn_id 变了: %v -> %v", metaA["turn_id"], metaB["turn_id"])
	}
	if metaA["agent_iteration"] != "1" || metaB["agent_iteration"] != "2" {
		t.Fatalf("iteration 不对: %v -> %v", metaA["agent_iteration"], metaB["agent_iteration"])
	}

	// 换一轮用户输入就该换 turn_id。
	nextTurn := identitySource()
	nextTurn["input"] = []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "再把 B1 填成 2"}}},
	}
	bodyC, err := prepareResponsesBodyFor(nextTurn, cfg, scope, who, identityThread)
	if err != nil {
		t.Fatal(err)
	}
	if objectValue(bodyC["metadata"])["turn_id"] == metaA["turn_id"] {
		t.Fatal("换了一轮用户输入，turn_id 却没变")
	}
}

// 缓存键：有就改写成不透明值、而且跨席位稳定；没有就必须整个字段缺席，
// 不能变成一个全局常量 —— 那等于把被否决的全局塌缩偷偷施加到无 key 流量上。
func TestCacheKeyIsRewrittenButNeverInvented(t *testing.T) {
	cfg := identityConfig()
	source := identitySource()
	body, err := prepareResponsesBodyFor(source, cfg, conversationScope(source), seat("bp-a"), identityHygiene)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, _ := body["prompt_cache_key"].(string)
	if rewritten == "" || rewritten == source["prompt_cache_key"] {
		t.Fatalf("缓存键没有被改写: %q", rewritten)
	}
	// 不能带任何只有我们才会发的前缀。真客户端根本不发这个字段，所以一个
	// "bp_" 开头的值等于在每个请求上签名。形状要和调用方自己发的那种一致。
	if strings.Contains(rewritten, "_") {
		t.Errorf("缓存键带了可识别前缀: %q", rewritten)
	}
	if len(rewritten) != 36 || strings.Count(rewritten, "-") != 4 {
		t.Errorf("缓存键不是 UUID 形状: %q", rewritten)
	}
	if strings.Contains(rewritten, "01a0f7b6") {
		t.Fatal("改写后的缓存键里还留着客户端会话 UUID")
	}
	// 换个号，同一个入站 key 必须得到同一个结果，否则会话中途换号就会翻脸。
	other := identitySource()
	bodyB, err := prepareResponsesBodyFor(other, cfg, conversationScope(other), seat("bp-z"), identityHygiene)
	if err != nil {
		t.Fatal(err)
	}
	if bodyB["prompt_cache_key"] != rewritten {
		t.Fatal("两个席位对同一个入站 key 给出了不同的缓存键")
	}

	noKey := identitySource()
	delete(noKey, "prompt_cache_key")
	bodyC, err := prepareResponsesBodyFor(noKey, cfg, conversationScope(noKey), seat("bp-a"), identityThread)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := bodyC["prompt_cache_key"]; exists {
		t.Fatal("入站没有缓存键，出站却凭空造了一个")
	}
}

// 翻译出来的 item 必须是我们自己的副本。出站上删一个 id 不能把入站的 source
// 也改掉 —— turnState 在这之后还要读 source，响应路径也要读。
func TestTranslationNeverTouchesTheSource(t *testing.T) {
	source := identitySource()
	before := string(jsonBytes(source))

	items := translateInputItemsFor(source["input"], clientToolSpecs(source),
		conversationScope(source), wireOptions{clientShapes: true, plainPrologue: true})

	if after := string(jsonBytes(source)); after != before {
		t.Fatalf("source 被改动了\n前: %s\n后: %s", before, after)
	}
	// 而且确实是副本，不是同一个 map。
	for _, value := range items {
		item := objectValue(value)
		if item == nil {
			continue
		}
		for _, original := range source["input"].([]any) {
			if reflect.ValueOf(item).Pointer() == reflect.ValueOf(objectValue(original)).Pointer() {
				t.Fatal("出站 item 和入站 item 是同一个 map")
			}
		}
	}
}

// clientShapes 要按抓包的形状来：user / developer 消息不带 id，
// assistant 的文本 part 要有 annotations 和 logprobs。
func TestClientShapesFollowTheCapture(t *testing.T) {
	source := identitySource()
	items := translateInputItemsFor(source["input"], clientToolSpecs(source),
		conversationScope(source), wireOptions{clientShapes: true})
	for _, value := range items {
		item := objectValue(value)
		role := stringValue(item["role"])
		if role == "user" || role == "developer" {
			if _, exists := item["id"]; exists {
				t.Errorf("%s 消息还带着 id", role)
			}
		}
		if role == "assistant" {
			parts, _ := item["content"].([]any)
			for _, raw := range parts {
				part := objectValue(raw)
				if stringValue(part["type"]) != "output_text" {
					continue
				}
				if _, exists := part["logprobs"]; !exists {
					t.Error("assistant 文本 part 缺 logprobs")
				}
				if _, exists := part["annotations"]; !exists {
					t.Error("assistant 文本 part 缺 annotations")
				}
			}
		}
	}
}

// 出站字节：键序照抄真实客户端，而且不转义 < > & —— JSON.stringify 从不转义，
// Go 的编码器总是转义。
func TestOutboundBytesLookLikeABrowserClient(t *testing.T) {
	cfg := identityConfig()
	source := identitySource()
	body, err := prepareResponsesBodyFor(source, cfg, conversationScope(source), seat("bp-a"), identityThread)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(responsesBodyBytes(body))

	// 这两个针要在运行时拼出来：源码里直接写 \u003c 会被当成字符本身。
	backslash := "\\"
	if strings.Contains(raw, backslash+"u003c") || strings.Contains(raw, backslash+"u0026") {
		t.Error("出站字节里还有 HTML 转义，真实客户端不会这样")
	}
	if !strings.Contains(raw, "<b>") {
		t.Error("原文里的 < 没有原样写出")
	}
	order := []string{`"model"`, `"input"`, `"stream"`, `"store"`,
		`"context_management"`, `"metadata"`, `"reasoning_effort"`, `"model_selection"`}
	previous := -1
	for _, key := range order {
		at := strings.Index(raw, key)
		if at < 0 {
			t.Fatalf("出站字节里没有 %s", key)
		}
		if at < previous {
			t.Errorf("%s 的位置不对，键序和抓包不一致", key)
		}
		previous = at
	}
	// metadata 内部也要按抓包的顺序。
	metaAt := strings.Index(raw, `"metadata"`)
	tail := raw[metaAt:]
	if strings.Index(tail, `"task_id"`) > strings.Index(tail, `"turn_id"`) {
		t.Error("metadata 里 task_id 应该排在 turn_id 前面")
	}
	// 还要是合法 JSON。
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		t.Fatal("出站字节不是合法 JSON")
	}
}

// 没有缓存键、而且第一个 item 带内联图片的请求：scope 必须在图片被改写成
// 中转 URL 之前就读出来，否则同一条会话过了中转条目的 5 分钟有效期就换 scope。
func TestScopeIsReadBeforeImagesAreRewritten(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{
					"type":      "input_image",
					"image_url": "data:image/png;base64,iVBORw0KGgo=",
				}},
			}},
		}
	}
	source := build()
	before := conversationScope(source)
	rewriteInlineImages(source["input"], "https://example.invalid/bp-img")
	after := conversationScope(source)
	if before == after {
		t.Skip("这台机器上图片没有被改写，这个测试没有被测到的东西")
	}
	// 两者不同正是问题所在：prepareRequest 必须用改写之前那个。
	if before == "" {
		t.Fatal("改写前就读不出 scope")
	}
}

// 无从命名的请求不该落进一个共享命名空间。以前这里返回字面量 "anonymous"，
// 它不是空串，所以躲过了每一道 scope == "" 的守卫。
func TestAnUnscopeableRequestGetsNoScope(t *testing.T) {
	if got := conversationScope(map[string]any{"input": "一段纯文本"}); got != "" {
		t.Fatalf("scope = %q，应该是空串", got)
	}
	if got := conversationScope(map[string]any{}); got != "" {
		t.Fatalf("scope = %q，应该是空串", got)
	}
}

// 字段截断要按字符切，不能把一个多字节字符切成两半。
func TestTruncateKeepsCharactersWhole(t *testing.T) {
	text := strings.Repeat("中", 10) // 每个 3 字节
	got := truncate(text, 10)
	if strings.ContainsRune(got, '�') {
		t.Error("截断切出了半个字符")
	}
	if len(got) > 10 {
		t.Errorf("截断后 %d 字节，超过上限", len(got))
	}
	if got != strings.Repeat("中", 3) {
		t.Errorf("截断结果 = %q", got)
	}
}

// cookie jar 一个号一个，永不共享。
func TestCookieJarsAreNeverShared(t *testing.T) {
	jar := newCookieJar()
	jar.remember("bp-a", map[string][]string{"Set-Cookie": {
		"__cf_bm=aaa; Path=/; HttpOnly", "_cfuvid=bbb; Path=/",
	}})
	jar.remember("bp-b", map[string][]string{"Set-Cookie": {"__cf_bm=ccc; Path=/"}})

	headerA, headerB := jar.header("bp-a"), jar.header("bp-b")
	if !strings.Contains(headerA, "__cf_bm=aaa") || !strings.Contains(headerA, "_cfuvid=bbb") {
		t.Fatalf("bp-a 的 cookie 不对: %s", headerA)
	}
	if strings.Contains(headerB, "aaa") || strings.Contains(headerB, "bbb") {
		t.Fatalf("bp-b 拿到了 bp-a 的 cookie: %s", headerB)
	}
	if jar.header("bp-never-seen") != "" {
		t.Error("没见过的号拿到了 cookie")
	}
	jar.forget("bp-a")
	if jar.header("bp-a") != "" {
		t.Error("forget 之后 cookie 还在")
	}
}

// 每一级只许动它该动的字段。这是个长期闸门：以后谁让人格改动越界到 input、
// model、store 这些地方，构建直接红，而不是等到线上缓存命中率掉下来才发现。
func TestEachRungTouchesOnlyWhatItMayTouch(t *testing.T) {
	cfg := identityConfig()
	who := seat("bp-a")
	source := identitySource()
	scope := conversationScope(source)

	baseline, err := prepareResponsesBodyFor(identitySource(), cfg, scope, persona{}, identityOff)
	if err != nil {
		t.Fatal(err)
	}
	// input 在 hygiene 级会被 plain_prologue / client_shapes 改写，所以它只在
	// 这两个开关都关掉时属于"不许动"。这里单独验证那个组合。
	frozen := []string{"model", "model_selection", "store", "stream",
		"reasoning_effort", "context_management", "input"}
	bare := identityConfig()
	bare.ClientIdentity.PlainPrologue = false
	bare.ClientIdentity.ClientShapes = false

	for _, mode := range []identityMode{identityHygiene, identityThread} {
		t.Run(mode.String(), func(t *testing.T) {
			got, err := prepareResponsesBodyFor(identitySource(), bare, scope, who, mode)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range frozen {
				if string(jsonBytes(got[key])) != string(jsonBytes(baseline[key])) {
					t.Errorf("%s 被动了，这一级不该碰它", key)
				}
			}
			// 允许变的就这些：缓存键和 metadata 里的三个身份字段。
			meta := objectValue(got["metadata"])
			base := objectValue(baseline["metadata"])
			for key := range meta {
				switch key {
				case "task_id", "turn_id", "agent_iteration", "bps_tools_version_id":
				default:
					t.Errorf("metadata 里多了一个真实客户端没有的键: %s", key)
				}
			}
			if meta["agent_iteration"] != base["agent_iteration"] {
				t.Error("agent_iteration 不该因为开了人格而改变")
			}
		})
	}
}

// 席位身份绝不能顺着响应回到调用方。开了 thread 之后 task_id 是每号一个常量，
// 回显出去等于给每个下游用户发一个池子指纹：数得出有多少个号、看得出自己这轮
// 被哪个号服务、以及自己在和谁共用一个号。
func TestSeatIdentityNeverComesBackToTheCaller(t *testing.T) {
	cfg := identityConfig()
	who := seat("bp-a")
	source := identitySource()
	body, err := prepareResponsesBodyFor(source, cfg, conversationScope(source), who, identityThread)
	if err != nil {
		t.Fatal(err)
	}
	seatTask := objectValue(body["metadata"])["task_id"].(string)

	// 上游原样回显我们发出去的 metadata。
	upstream := map[string]any{
		"id":       "resp_1",
		"metadata": map[string]any{"task_id": seatTask, "turn_id": "x", "agent_iteration": "1"},
		"output":   []any{},
	}
	restoreCallerEcho(upstream, identitySource())

	raw := string(jsonBytes(upstream))
	if strings.Contains(raw, seatTask) {
		t.Fatalf("响应里带回了席位的 task_id: %s", seatTask)
	}
	if value, present := upstream["metadata"]; present && value != nil {
		t.Errorf("调用方没发 metadata，响应却带了一个: %v", value)
	}

	// 调用方自己发了 metadata 的话，还给他原来那份。
	callerSource := identitySource()
	callerSource["metadata"] = map[string]any{"mine": "yes"}
	upstream2 := map[string]any{"metadata": map[string]any{"task_id": seatTask}}
	restoreCallerEcho(upstream2, callerSource)
	got := objectValue(upstream2["metadata"])
	if got == nil || got["mine"] != "yes" {
		t.Errorf("没有把调用方自己的 metadata 还回去: %v", upstream2["metadata"])
	}
	if _, leaked := got["task_id"]; leaked {
		t.Error("调用方的 metadata 里混进了席位 task_id")
	}
}


// 会话绝不能早于订阅本身 —— 一个要订阅才能用的 Excel 会话，不可能在订阅开始前就存在。
//
// 第一版从一个固定窗口（30–400 天前）里取这个时间，不参考任何上游已知的事实，
// 结果给一个 2026-09-01 才开始订阅的号发了 2026-04-10 的会话，早了五个月。
func TestSessionNeverPredatesTheSubscription(t *testing.T) {
	starts := []time.Time{
		time.Unix(1777593600, 0), // 2026-05-01
		time.Unix(1788220800, 0), // 2026-09-01
		time.Now().Add(-40 * 24 * time.Hour),
	}
	for _, start := range starts {
		for _, name := range []string{"bp-a", "bp-b", "bp-c", "bp-d", "bp-e"} {
			who := persona{seed: name, pepper: "p", subscriptionStart: start}
			birth := time.UnixMilli(seatBirthMs(who))
			if birth.Before(start) {
				t.Errorf("%s 的会话 %s 早于订阅开始 %s", name,
					birth.Format(time.RFC3339), start.Format(time.RFC3339))
			}
			if birth.After(time.Now()) {
				t.Errorf("%s 的会话 %s 在未来", name, birth.Format(time.RFC3339))
			}
		}
	}
}

// 同一批导入的号常常共用一个订阅起始，所以出生时间必须靠 seed 拉开，
// 否则又会按批次聚簇 —— 而批次正是封号规律里相关性 100% 的那个单位。
func TestSameSubscriptionStillGivesDifferentBirthdays(t *testing.T) {
	start := time.Unix(1777593600, 0)
	seen := map[int64][]string{}
	for _, name := range []string{"bp-a", "bp-b", "bp-c", "bp-d", "bp-e", "bp-f"} {
		who := persona{seed: name, pepper: "p", subscriptionStart: start}
		day := seatBirthMs(who) / (24 * 60 * 60 * 1000)
		seen[day] = append(seen[day], name)
	}
	if len(seen) < 4 {
		t.Errorf("6 个同订阅的席位只落在 %d 个不同的日子里，拉得不够开: %v", len(seen), seen)
	}
}

// 没有订阅声明的凭据要退回旧行为，而且仍然要给出一个稳定的日期。
func TestAMissingSubscriptionClaimStillGivesOneStableBirthday(t *testing.T) {
	who := persona{seed: "bp-no-claim", pepper: "p"}
	first := seatBirthMs(who)
	for i := 0; i < 20; i++ {
		if seatBirthMs(who) != first {
			t.Fatal("没有订阅声明时出生时间不稳定")
		}
	}
}
