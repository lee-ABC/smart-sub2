package basispoints

import (
	"encoding/json"
	"testing"
)

const h15ProbeCredential = `{"type":"oai-basispoints","access_token":"t",` +
	`"account_id":"acct-1","email":"probe@example.com"}`

// h15ProbeService stubs the host and records every outbound body.
func h15ProbeService(t *testing.T, served string, status int) (*Service, *[]map[string]any) {
	t.Helper()
	var sent []map[string]any
	service := NewService()
	service.ledger = newCostLedger()
	cfg := defaultConfig()
	cfg.ClientIdentity.Mode = "thread"
	cfg.ClientIdentity.Pepper = "test-pepper"
	service.cfg = cfg
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.auth.list":
			return h15Decode(map[string]any{"files": []map[string]any{
				{"auth_index": "1", "name": "p.json", "label": "probe@example.com",
					"provider": Provider},
			}}, out)
		case "host.auth.get":
			return h15Decode(map[string]any{"json": json.RawMessage(h15ProbeCredential)}, out)
		case "host.http.do":
			object, _ := payload.(map[string]any)
			var body map[string]any
			if raw, ok := object["body"].([]byte); ok {
				_ = json.Unmarshal(raw, &body)
			}
			sent = append(sent, body)
			answer := map[string]any{"status": "completed", "model": served,
				"usage": map[string]any{"input_tokens": 12, "output_tokens": 3}}
			return h15Decode(upstreamResponse{StatusCode: status, Body: jsonBytes(answer)}, out)
		}
		return nil
	})
	return service, &sent
}

func h15RunProbe(t *testing.T, service *Service, query map[string][]string) (int, probeResult) {
	t.Helper()
	raw, _ := json.Marshal(managementRequest{Method: "GET", Path: "/x/probe", Query: query})
	response, err := service.managementHandle(raw)
	if err != nil {
		t.Fatalf("探测返回错误: %v", err)
	}
	status, _ := response["StatusCode"].(int)
	body, _ := response["Body"].([]byte)
	var result probeResult
	if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
		t.Fatalf("返回的不是 probeResult: %s", body)
	}
	return status, result
}

// 被换模型要认出来，而且要说清楚换成了什么。
func TestProbeDetectsASubstitutedModel(t *testing.T) {
	service, sent := h15ProbeService(t, "gpt-5.6-luna", 200)
	status, result := h15RunProbe(t, service, map[string][]string{"file": {"p.json"}})
	if status != 200 {
		t.Fatalf("状态码 %d", status)
	}
	if !result.Swapped {
		t.Error("请求 astra、回的是 luna，却没判成被换")
	}
	if result.Served != "gpt-5.6-luna" || result.Requested != "gpt-6-astra" {
		t.Errorf("requested=%q served=%q", result.Requested, result.Served)
	}
	if len(*sent) != 1 {
		t.Errorf("发了 %d 次上游请求，应当恰好 1 次", len(*sent))
	}
}

// 没被换的时候不能误报——误报会让人把好号停掉。
func TestProbeSaysNothingIsWrongWhenNothingIs(t *testing.T) {
	service, _ := h15ProbeService(t, "gpt-6-astra", 200)
	_, result := h15RunProbe(t, service, map[string][]string{"file": {"p.json"}})
	if result.Swapped {
		t.Error("原样作答却报成了被换")
	}
	if result.Error != "" {
		t.Errorf("正常情况下带了错误: %q", result.Error)
	}
}

// 探测必须和真实请求用同一个座位。
//
// 这是整件事里最容易悄悄做错的一条：探测如果自己造一套身份，就等于在同一个令牌
// 上多出一个客户端——正是这一轮刚从配额管理那条路上去掉的毛病。所以这里拿真实
// 的 executor 请求和探测请求各发一次，比对 task_id。
func TestProbeUsesTheSameSeatAsRealTraffic(t *testing.T) {
	service, sent := h15ProbeService(t, "gpt-6-astra", 200)

	// 一发真实请求：和线上一样，只带 file_name，不带 AuthID。
	realRaw, _ := json.Marshal(map[string]any{
		"AuthMetadata":    map[string]any{"file_name": "p.json"},
		"OriginalRequest": jsonBytes(map[string]any{"model": "gpt-6-astra", "input": "hi"}),
		"Payload":         jsonBytes(map[string]any{"model": "gpt-6-astra", "input": "hi"}),
		"StorageJSON":     []byte(h15ProbeCredential),
		"Model":           "gpt-6-astra",
	})
	if _, err := service.Handle("executor.execute", realRaw); err != nil {
		t.Fatalf("真实请求失败: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("真实请求没有发出去（发了 %d 次）", len(*sent))
	}
	realBody := (*sent)[0]

	h15RunProbe(t, service, map[string][]string{"file": {"p.json"}})
	if len(*sent) != 2 {
		t.Fatalf("探测没有发出去（一共 %d 次）", len(*sent))
	}
	probeBody := (*sent)[1]

	realMeta, _ := realBody["metadata"].(map[string]any)
	probeMeta, _ := probeBody["metadata"].(map[string]any)
	if realMeta == nil || probeMeta == nil {
		t.Fatalf("两边的 metadata: 真实=%v 探测=%v", realBody["metadata"], probeBody["metadata"])
	}
	if realMeta["task_id"] != probeMeta["task_id"] {
		t.Errorf("探测用了另一个座位：真实 task_id=%v，探测 task_id=%v —— "+
			"同一个令牌上出现了两个客户端", realMeta["task_id"], probeMeta["task_id"])
	}
	if realMeta["turn_id"] == probeMeta["turn_id"] {
		t.Error("探测和真实请求共用了 turn_id，两次不同的调用不该是同一轮")
	}

	// 字段集合也要一致，少一个多一个都是另一种客户端。
	for key := range realBody {
		if _, ok := probeBody[key]; !ok {
			t.Errorf("探测的请求体少了字段 %q", key)
		}
	}
	for key := range probeBody {
		if _, ok := realBody[key]; !ok {
			t.Errorf("探测的请求体多了字段 %q", key)
		}
	}
}

// 不在名单里的模型，一次上游请求都不该发。
func TestProbeRejectsAModelOutsideTheAllowList(t *testing.T) {
	service, sent := h15ProbeService(t, "gpt-6-astra", 200)
	status, result := h15RunProbe(t, service,
		map[string][]string{"file": {"p.json"}, "model": {"gpt-9-whatever"}})
	if status != 400 || result.Error == "" {
		t.Errorf("状态码 %d，错误 %q", status, result.Error)
	}
	if len(*sent) != 0 {
		t.Errorf("模型没过校验却还是打了 %d 次上游", len(*sent))
	}
}

// 上游报错要如实说，不能静悄悄显示成"没问题"。
func TestProbeReportsAnUpstreamFailureInsteadOfPassing(t *testing.T) {
	service, _ := h15ProbeService(t, "gpt-6-astra", 403)
	_, result := h15RunProbe(t, service, map[string][]string{"file": {"p.json"}})
	if result.Error == "" {
		t.Error("上游 403 却没有报错")
	}
	if result.Swapped {
		t.Error("请求失败时不该声称模型被换了")
	}
	if result.Served != "" {
		t.Errorf("请求失败却报了作答模型 %q", result.Served)
	}
}

// 冷却已按要求去掉：连着测两次，就该有两发上游请求。
//
// 留着这条是因为"限制被移除"和"限制还在但没触发"从结果上看是一样的，而前者是
// 操作者要的、后者是回归。
func TestProbeCanBeRepeatedImmediately(t *testing.T) {
	service, sent := h15ProbeService(t, "gpt-6-astra", 200)
	for i := 0; i < 2; i++ {
		status, result := h15RunProbe(t, service, map[string][]string{"file": {"p.json"}})
		if status != 200 {
			t.Fatalf("第 %d 次状态码 = %d，正文错误 %q", i+1, status, result.Error)
		}
	}
	if len(*sent) != 2 {
		t.Errorf("连测两次只发出了 %d 次上游请求，冷却好像还在", len(*sent))
	}
}

// 上游对 stream:false 照样回事件流，探测必须认得。
//
// 这条是补的：第一版探测把回包当 JSON 解，线上一发就废——而中继自己早就知道这件
// 事，service.go 里 parseFinalStreamResponse 上面那段注释写的就是它。之前那几条
// 测试全用 JSON 回包做桩，于是它们一起漏掉了唯一一种真实会发生的形状。
//
// 教训不在"忘了处理 SSE"，在于桩数据是按我以为的样子造的，不是按上游真实的样子
// 造的。所以这里的正文是从线上实抓的回包裁下来的。
func TestProbeReadsTheModelFromAnEventStream(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_07779","object":"response",` +
		`"status":"in_progress","model":"gpt-5.6-luna"}}` + "\n\n" +
		"event: response.in_progress\n" +
		`data: {"type":"response.in_progress","response":{"id":"resp_07779","model":"gpt-5.6-luna"}}` +
		"\n\n"

	var service *Service
	service = NewService()
	service.ledger = newCostLedger()
	cfg := defaultConfig()
	cfg.ClientIdentity.Mode = "thread"
	cfg.ClientIdentity.Pepper = "test-pepper"
	service.cfg = cfg
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.auth.list":
			return h15Decode(map[string]any{"files": []map[string]any{
				{"auth_index": "1", "name": "p.json", "label": "probe@example.com",
					"provider": Provider},
			}}, out)
		case "host.auth.get":
			return h15Decode(map[string]any{"json": json.RawMessage(h15ProbeCredential)}, out)
		case "host.http.do":
			return h15Decode(upstreamResponse{StatusCode: 200, Body: []byte(stream)}, out)
		}
		return nil
	})

	status, result := h15RunProbe(t, service, map[string][]string{"file": {"p.json"}})
	if status != 200 {
		t.Fatalf("状态码 %d，错误 %q", status, result.Error)
	}
	if result.Error != "" {
		t.Fatalf("解析事件流时报错了: %q", result.Error)
	}
	if result.Served != "gpt-5.6-luna" {
		t.Errorf("served = %q, want gpt-5.6-luna", result.Served)
	}
	if !result.Swapped {
		t.Error("请求 astra、事件流里是 luna，却没判成被换")
	}
}
