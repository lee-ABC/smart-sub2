package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

// h17：上游会把接单账号的资料名塞进模型上下文，模型于是用那个名字称呼调用方、
// 写进落款和提交作者。这里守的是「那句说明在每一条开场分支上都在、关掉就都不在」。

func nameGuardSources() map[string]map[string]any {
	user := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
	}}
	tool := map[string]any{
		"type": "function", "name": "exec_command", "description": "Run a shell command.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
			"required":   []any{"cmd"},
		},
	}
	lite := append([]any{map[string]any{
		"type": "additional_tools", "tools": []any{tool},
	}}, user...)
	return map[string]map[string]any{
		"no-tools":         {"model": "gpt-6-astra", "input": user},
		"tool-catalog":     {"model": "gpt-6-astra", "input": user, "tools": []any{tool}},
		"additional-tools": {"model": "gpt-6-astra", "input": lite},
		"with-instructions": {
			"model": "gpt-6-astra", "input": user,
			"instructions": "The signed-in customer is Alice Chen.",
		},
	}
}

// developerTexts 取出请求体里所有 developer 消息的文字，按出现顺序。
func developerTexts(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input = %#v, want a list", body["input"])
	}
	var texts []string
	for _, raw := range items {
		item := objectValue(raw)
		if stringValue(item["type"]) != "message" || stringValue(item["role"]) != "developer" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, part := range content {
			texts = append(texts, stringValue(objectValue(part)["text"]))
		}
	}
	return texts
}

func TestAccountNameGuardIsOnEveryPrologueBranch(t *testing.T) {
	for _, plain := range []bool{false, true} {
		for name, source := range nameGuardSources() {
			cfg, who, mode := defaultConfig(), persona{}, identityOff
			if plain {
				// 生产就是这一档：thread + plain_prologue。
				cfg, who, mode = identityConfig(), seat("a"), identityThread
			}
			body, err := prepareResponsesBodyFor(source, cfg, conversationScope(source), who, mode)
			if err != nil {
				t.Fatalf("%s plain=%v: %v", name, plain, err)
			}
			texts := developerTexts(t, body)
			carrying := 0
			for _, text := range texts {
				carrying += strings.Count(text, accountNameGuard)
			}
			if carrying != 1 {
				t.Errorf("%s plain=%v: 说明出现了 %d 次，应该恰好 1 次", name, plain, carrying)
			}
		}
	}
}

// 调用方自己的 instructions 必须排在说明前面、且原样保留：说明里那句
// 「developer 指令给的名字才是对的」指的就是它。
func TestAccountNameGuardLeavesCallerInstructionsFirstAndIntact(t *testing.T) {
	source := nameGuardSources()["with-instructions"]
	body, err := prepareResponsesBodyFor(source, defaultConfig(), conversationScope(source), persona{}, identityOff)
	if err != nil {
		t.Fatal(err)
	}
	texts := developerTexts(t, body)
	if len(texts) < 2 {
		t.Fatalf("developer 消息只有 %d 条，应该至少有调用方指令和协议说明两条", len(texts))
	}
	if texts[0] != "The signed-in customer is Alice Chen." {
		t.Fatalf("第一条 developer 消息被改了：%q", texts[0])
	}
	if !strings.HasSuffix(texts[1], accountNameGuard) {
		t.Fatal("说明应该接在协议说明那条消息的末尾")
	}
}

// 关掉开关后，请求体必须和没有这项功能时一模一样（不多条目、不留痕迹）。
func TestAccountNameGuardOffLeavesNoTrace(t *testing.T) {
	for name, source := range nameGuardSources() {
		on := defaultConfig()
		off := defaultConfig()
		off.HideAccountName = false

		withGuard, err := prepareResponsesBodyFor(source, on, conversationScope(source), persona{}, identityOff)
		if err != nil {
			t.Fatal(err)
		}
		without, err := prepareResponsesBodyFor(source, off, conversationScope(source), persona{}, identityOff)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(without)
		if strings.Contains(string(raw), "authenticated runtime user context") {
			t.Errorf("%s: 开关关了，请求体里还有那句说明", name)
		}
		if a, b := len(withGuard["input"].([]any)), len(without["input"].([]any)); a != b {
			t.Errorf("%s: 开关改变了 input 条目数（开 %d / 关 %d）", name, a, b)
		}
		// 去掉那句话之后，两边应当逐字节相同。
		rawOn, _ := json.Marshal(withGuard)
		quoted, _ := json.Marshal(" " + accountNameGuard)
		stripped := strings.Replace(string(rawOn), string(quoted[1:len(quoted)-1]), "", 1)
		if stripped != string(raw) {
			t.Errorf("%s: 除了那句说明之外，开关还改了别的东西", name)
		}
	}
}

func TestAccountNameGuardDefaultsOnAndFollowsConfig(t *testing.T) {
	if !defaultConfig().HideAccountName {
		t.Fatal("默认应该是开")
	}
	cfg := defaultConfig()
	if err := unmarshalYAML([]byte("hide_account_name: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HideAccountName {
		t.Fatal("config.yaml 里写了 false，开关却还开着")
	}
	// 没写这一项的 config.yaml 不能把默认值冲掉。
	cfg = defaultConfig()
	if err := unmarshalYAML([]byte("timeout_seconds: 1800\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.HideAccountName {
		t.Fatal("config.yaml 没提这一项，默认的开却没了")
	}
}

// 这句话全池每个请求都带。它不能自曝「这是中转」——plainPrologue 花了力气去掉的
// 正是这类话；也不能出现任何账号自己的信息。
func TestAccountNameGuardDoesNotAnnounceARelay(t *testing.T) {
	lower := strings.ToLower(accountNameGuard)
	for _, word := range []string{"relay", "proxy", "service account", "billed", "external", "pool", "seat", "credential"} {
		if strings.Contains(lower, word) {
			t.Errorf("说明里出现了 %q", word)
		}
	}
	if !strings.Contains(lower, "authenticated runtime user context") {
		t.Error("说明必须点名上游那一段；泛化措辞实测会把调用方自己给的名字也压掉")
	}
	if !strings.Contains(lower, "developer instructions or the user in this conversation") {
		t.Error("说明必须明确放行调用方指令和用户自己给的名字")
	}
}
