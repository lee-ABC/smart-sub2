package basispoints

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func h15WriteCharge(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "charge.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	original := chargeFilePathOverride
	chargeFilePathOverride = path
	t.Cleanup(func() { chargeFilePathOverride = original })
}

// 扣费数据必须带着它自己的年龄一起交出去。
//
// 这条和额度那条是同一个教训：一个关于钱的数字悄悄停止更新时，它看起来和一个
// 刚更新过的数字一模一样，而且因为它不再跳动，反而更像是可信的。采集器挂了、
// 主站库连不上、容器被重启——这些都只表现为"文件不再变新"，不表现为报错。
func TestChargeRouteReportsHowOldTheFigureIs(t *testing.T) {
	h15WriteCharge(t, `{"pool":{"today":{"charged_usd":12.5}},"credentials":{}}`)
	service, _ := h15Service(t)

	status, body := h15Get(t, service, "/x/charge", nil)
	if status != 200 {
		t.Fatalf("状态码 %d：%s", status, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("不是 JSON: %s", body)
	}
	if _, ok := payload["age_seconds"]; !ok {
		t.Error("没有带上 age_seconds，页面就无从知道这个数是不是已经不动了")
	}
	pool, _ := payload["pool"].(map[string]any)
	if pool == nil {
		t.Fatalf("原始内容没有原样带过来: %s", body)
	}
}

// 没有文件时要说清楚，而不是交一个空壳让页面显示 $0.00。
func TestChargeRouteSaysWhenThereIsNoFile(t *testing.T) {
	original := chargeFilePathOverride
	chargeFilePathOverride = filepath.Join(t.TempDir(), "does-not-exist.json")
	t.Cleanup(func() { chargeFilePathOverride = original })
	service, _ := h15Service(t)

	status, body := h15Get(t, service, "/x/charge", nil)
	if status != 404 {
		t.Errorf("状态码 = %d, want 404", status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload["error"] == nil {
		t.Errorf("没有给出可读的原因: %s", body)
	}
	// $0.00 会被当成"这个号没赚钱"，而真相是"我们不知道"。两者必须长得不一样。
	if payload["pool"] != nil || payload["credentials"] != nil {
		t.Error("文件不存在却还是交出了形状完整的空数据")
	}
}

// 文件坏了也不能当成空数据。
func TestChargeRouteRejectsAMalformedFile(t *testing.T) {
	h15WriteCharge(t, "{ this is not json")
	service, _ := h15Service(t)

	status, body := h15Get(t, service, "/x/charge", nil)
	if status != 502 {
		t.Errorf("状态码 = %d, want 502", status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload["error"] == nil {
		t.Errorf("没有给出可读的原因: %s", body)
	}
}

// 年龄要真的跟着文件时间走，不是写死的 0。
func TestChargeAgeTracksTheFileMTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "charge.json")
	if err := os.WriteFile(path, []byte(`{"credentials":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	original := chargeFilePathOverride
	chargeFilePathOverride = path
	t.Cleanup(func() { chargeFilePathOverride = original })

	service, _ := h15Service(t)
	_, body := h15Get(t, service, "/x/charge", nil)
	var payload struct {
		Age int64 `json:"age_seconds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Age < 5000 || payload.Age > 5700 {
		t.Errorf("age_seconds = %d，一个 90 分钟前的文件应当在 5400 秒上下", payload.Age)
	}
}

// 扣费文件的位置必须跟着插件的数据目录走，不能写死宿主机的路径。
//
// 第一版写的是 /opt/cliproxyapi/charge.json —— 那是宿主机上的路径。插件跑在
// 容器里，容器里只挂了 /CLIProxyAPI/plugins 等几个目录，那个路径根本不存在。
// 这条路由会永远回 404，而唯一的症状是卡片上少了一行，没人会知道为什么。
//
// 这个测试断言的是"路径从 DataDir 推出来"，不是"文件读得到"——后者在构建机上
// 本来就成立，前者才是当初错的地方。
func TestChargeFileLivesInThePluginDataDirectory(t *testing.T) {
	service, _ := h15Service(t)
	cfg := service.config()
	cfg.DataDir = "/CLIProxyAPI/plugins/oai-basispoints-data"
	service.mu.Lock()
	service.cfg = cfg
	service.mu.Unlock()

	got := service.chargeFilePath()
	want := "/CLIProxyAPI/plugins/oai-basispoints-data/charge.json"
	if got != want {
		t.Errorf("扣费文件路径 = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "/opt/") {
		t.Error("又用回了宿主机路径：容器里看不到它")
	}
}
