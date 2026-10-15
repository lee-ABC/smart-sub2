package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

// 管理路由的端到端测试。宿主回调整个替掉，所以这里不碰网络、不碰 CPA，
// 但走的是真正的路由分发、真正的 quotaFetch、真正的写回。

const h15UsageBody = `{"plan_type":"Business Premium","rate_limit":{"allowed":true,` +
	`"limit_reached":false,"secondary_window":{"used_percent":61,` +
	`"limit_window_seconds":604800,"reset_after_seconds":518400}}}`

type h15Host struct {
	usageCalls int
	authGets   int
	credential string
}

func (h *h15Host) call(method string, payload any, out any) error {
	switch method {
	case "host.auth.list":
		return h15Decode(map[string]any{"files": []map[string]any{
			{"auth_index": "7", "name": "a.json", "label": "a@example.com", "provider": Provider},
			{"auth_index": "9", "name": "b.json", "label": "b@example.com", "provider": Provider},
			{"auth_index": "3", "name": "claude.json", "label": "c@example.com", "provider": "claude"},
		}}, out)
	case "host.auth.get":
		h.authGets++
		return h15Decode(map[string]any{"json": json.RawMessage(h.credential)}, out)
	case "host.http.do":
		h.usageCalls++
		return h15Decode(upstreamResponse{StatusCode: 200, Body: []byte(h15UsageBody)}, out)
	}
	return nil
}

func h15Decode(value any, out any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func h15Service(t *testing.T) (*Service, *h15Host) {
	t.Helper()
	host := &h15Host{credential: `{"type":"oai-basispoints","access_token":"t",` +
		`"account_id":"acct-1","email":"a@example.com"}`}
	service := &Service{
		cfg:    defaultConfig(),
		host:   host.call,
		ledger: newCostLedger(),
	}
	// 账本里先有这个号，写回才有地方落。
	service.ledger.record(usageTarget{Key: "bp-a", File: "a.json", Label: "a@example.com"},
		5, true, false, tokenUsage{})
	return service, host
}

func h15Get(t *testing.T, service *Service, path string, query map[string][]string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(managementRequest{Method: "GET", Path: path, Query: query})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.managementHandle(raw)
	if err != nil {
		t.Fatalf("%s 返回错误: %v", path, err)
	}
	status, _ := response["StatusCode"].(int)
	body, _ := response["Body"].([]byte)
	return status, body
}

// 带 file= 就只问这一个号，而且只问一次上游。
//
// 这是整件事的要点：以前打开那一页会把全部凭据挨个查一遍，几十个令牌在同一秒从
// 同一个地址敲同一个接口。断言的是调用次数，不是返回内容——内容对了而调用了 48
// 次，问题一点没解决。
func TestSingleCredentialQuotaCallsUpstreamExactlyOnce(t *testing.T) {
	service, host := h15Service(t)
	quotaCache = map[string]quotaSnapshot{}

	status, body := h15Get(t, service, "/x/quota", map[string][]string{
		"file": {"a.json"}, "format": {"json"}})
	if status != 200 {
		t.Fatalf("状态码 %d，正文 %s", status, body)
	}
	if host.usageCalls != 1 {
		t.Errorf("上游调用了 %d 次，应当恰好 1 次", host.usageCalls)
	}

	var result quotaRefreshResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("返回的不是 quotaRefreshResult: %v / %s", err, body)
	}
	if result.UsedPercent != 61 {
		t.Errorf("已用 = %v, want 61", result.UsedPercent)
	}
	if result.Plan != "Business Premium" {
		t.Errorf("套餐 = %q", result.Plan)
	}
	if !result.Noted {
		t.Error("没有写回账本")
	}

	// 写回必须落在账本已有的那条上，不能新开一条。
	view := service.ledger.view()
	if len(view) != 1 {
		t.Fatalf("账本有 %d 条，应当还是 1 条", len(view))
	}
	account := view["bp-a"]
	if len(account.Segments) != 1 || account.Segments[0].Last.UsedPercent != 61 {
		t.Errorf("写回没落到已有记录上: %#v", account.Segments)
	}
}

// 不带参数的那一页必须一次上游都不发。
func TestQuotaPageDoesNotSweepTheInventory(t *testing.T) {
	service, host := h15Service(t)
	quotaCache = map[string]quotaSnapshot{}

	status, body := h15Get(t, service, "/x/quota", nil)
	if status != 200 {
		t.Fatalf("状态码 %d", status)
	}
	if host.usageCalls != 0 {
		t.Errorf("渲染列表页发了 %d 次上游请求，应当是 0 次", host.usageCalls)
	}
	text := string(body)
	for _, want := range []string{`data-file="a.json"`, `data-file="b.json"`, "刷新额度"} {
		if !strings.Contains(text, want) {
			t.Errorf("页面里没有 %q", want)
		}
	}
	if strings.Contains(text, "claude.json") {
		t.Error("列表页把别的 provider 的凭据也画出来了")
	}
}

// format=raw 原样转发上游正文，面板那张卡片才能照常解析。
func TestRawFormatReturnsUpstreamBodyVerbatim(t *testing.T) {
	service, _ := h15Service(t)
	quotaCache = map[string]quotaSnapshot{}

	status, body := h15Get(t, service, "/x/quota", map[string][]string{
		"auth_index": {"7"}, "format": {"raw"}})
	if status != 200 {
		t.Fatalf("状态码 %d，正文 %s", status, body)
	}
	if string(body) != h15UsageBody {
		t.Errorf("转发的正文被改过了:\n得到 %s\n想要 %s", body, h15UsageBody)
	}
}

// 不认识的号要给出能看懂的 JSON 错误，而不是一页 HTML 或一个空的 200。
func TestUnknownCredentialAnswersWithJSONError(t *testing.T) {
	service, host := h15Service(t)
	status, body := h15Get(t, service, "/x/quota", map[string][]string{"file": {"nope.json"}})
	if status != 404 {
		t.Errorf("状态码 = %d, want 404", status)
	}
	if host.usageCalls != 0 {
		t.Errorf("查不到凭据却还是发了 %d 次上游请求", host.usageCalls)
	}
	var result quotaRefreshResult
	if err := json.Unmarshal(body, &result); err != nil || result.Error == "" {
		t.Errorf("正文不是带 error 的 JSON: %s", body)
	}
}

// 守护开关：读的是配置，不是别的地方。
func TestGuardRouteReportsTheConfiguredSwitch(t *testing.T) {
	service, _ := h15Service(t)

	status, body := h15Get(t, service, "/x/guard", nil)
	if status != 200 {
		t.Fatalf("状态码 %d", status)
	}
	var state struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("不是 JSON: %s", body)
	}
	if !state.Enabled {
		t.Error("默认应当是开着的：配置里没有这个键时守护必须有武装")
	}

	cfg := service.config()
	cfg.DowngradeGuardEnabled = false
	service.mu.Lock()
	service.cfg = cfg
	service.mu.Unlock()

	_, body = h15Get(t, service, "/x/guard", nil)
	if err := json.Unmarshal(body, &state); err != nil || state.Enabled {
		t.Errorf("关掉之后仍然报开着: %s", body)
	}
}

// 30 秒缓存必须仍然挡得住连点。
func TestSecondRefreshInsideTheCacheWindowDoesNotHitUpstream(t *testing.T) {
	service, host := h15Service(t)
	quotaCache = map[string]quotaSnapshot{}

	for i := 0; i < 3; i++ {
		if status, body := h15Get(t, service, "/x/quota", map[string][]string{
			"file": {"a.json"}, "format": {"json"}}); status != 200 {
			t.Fatalf("第 %d 次状态码 %d，正文 %s", i+1, status, body)
		}
	}
	if host.usageCalls != 1 {
		t.Errorf("连点三次发了 %d 次上游请求，应当只有 1 次", host.usageCalls)
	}
}
