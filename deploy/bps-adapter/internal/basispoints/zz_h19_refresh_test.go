package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// h19：停用号每秒续期一次的风暴，以及令牌端点的 429/5xx 被当成 401。
//
// 宿主排续期时只看记录：内存里的 LastRefreshedAt，没有就看 Metadata 里的 last_refresh，
// 文件里的 last_refresh 它从来不读。记录在停用号文件被改写后重建时会丢掉 LastRefreshedAt，
// 而我们交回的 Metadata 又没有 last_refresh，宿主便当它"从未续期"立刻再续一次——续完
// 写文件、文件变了再重建、再丢、再续，2026-10-06 现网约每秒一次、每分钟 2,700 次改写。
// 把文件里的 last_refresh / expired 发布进 Metadata，重建出来的记录就能接上原来的节奏。

func h19Token(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func h19Metadata(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	c, err := parseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	metadata, ok := authData(raw, "a.json", c, Provider)["Metadata"].(map[string]any)
	if !ok {
		t.Fatal("记录里没有 Metadata")
	}
	return metadata
}

func h19Raw(t *testing.T, exp int64, extra string) []byte {
	t.Helper()
	claims := map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-h19"}}
	if exp != 0 {
		claims["exp"] = exp
	}
	raw := `{"access_token":"` + h19Token(t, claims) + `","refresh_token":"rt","email":"a@example.com"` + extra + `}`
	return []byte(raw)
}

func TestARecordCarriesWhenItWasLastRenewed(t *testing.T) {
	exp := time.Date(2026, 10, 16, 11, 1, 43, 0, time.UTC)
	metadata := h19Metadata(t, h19Raw(t, exp.Unix(), `,"last_refresh":"2026-10-06T11:01:43Z","expired":"2026-10-16T11:01:43Z"`))
	if got := metadata["last_refresh"]; got != "2026-10-06T11:01:43Z" {
		t.Errorf("last_refresh = %#v，文件里是 2026-10-06T11:01:43Z；记录里没有它，宿主就当这个号从未续期", got)
	}
	if got := metadata["expired"]; got != "2026-10-16T11:01:43Z" {
		t.Errorf("expired = %#v，令牌 exp 是 2026-10-16T11:01:43Z", got)
	}
}

// 从别处导进来的文件可能把 last_refresh 写成 Unix 秒；宿主两种都认，但我们统一成 RFC 3339。
func TestANumericLastRefreshIsPublishedAsRFC3339(t *testing.T) {
	metadata := h19Metadata(t, h19Raw(t, 1792285303, `,"last_refresh":1791284503`))
	if got := metadata["last_refresh"]; got != "2026-10-06T11:01:43Z" {
		t.Errorf("last_refresh = %#v，应当是 2026-10-06T11:01:43Z", got)
	}
}

// 没续过期的号就不要发布 last_refresh：null、看不懂的值都算没有。宿主于是续它一次、
// 写进文件，之后节奏就有了锚点。令牌没有 exp、文件也没写 expired 时同样不编一个出来。
func TestANeverRenewedCredentialPublishesNoTimestampItDoesNotHave(t *testing.T) {
	cases := map[string][]byte{
		"根本没这个字段": h19Raw(t, 1792285303, ``),
		"是 null":  h19Raw(t, 1792285303, `,"last_refresh":null`),
		"看不懂":     h19Raw(t, 1792285303, `,"last_refresh":"yesterday"`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if value, has := h19Metadata(t, raw)["last_refresh"]; has {
				t.Errorf("没续过期的号多出了 last_refresh = %#v", value)
			}
		})
	}
	if value, has := h19Metadata(t, h19Raw(t, 0, ``))["expired"]; has {
		t.Errorf("令牌没有 exp、文件没有 expired，记录里却有 expired = %#v", value)
	}
}

// 加这两个键不能动到别的：同一个号带不带 last_refresh，记录只差这一个键。
func TestTheTimestampsDoNotDisturbTheRestOfTheRecord(t *testing.T) {
	plain := h19Metadata(t, h19Raw(t, 1792285303, ``))
	stamped := h19Metadata(t, h19Raw(t, 1792285303, `,"last_refresh":"2026-10-06T11:01:43Z"`))
	if len(stamped) != len(plain)+1 {
		t.Errorf("带 last_refresh 的记录有 %d 个键，不带的有 %d 个，应当恰好多一个", len(stamped), len(plain))
	}
	for key, value := range plain {
		if string(jsonBytes(value)) != string(jsonBytes(stamped[key])) {
			t.Errorf("加上 last_refresh 之后 %q 变了：%s -> %s", key, jsonBytes(value), jsonBytes(stamped[key]))
		}
	}
	for _, key := range []string{"type", "auth_kind", "account_id", "file_name", "access_token", "request_scoped_errors", "refresh_interval_seconds", "expired"} {
		if _, has := plain[key]; !has {
			t.Errorf("记录里少了原有的 %q", key)
		}
	}
}

// 续期本身交回去的记录要带上这一次的时间：和要写回文件的那一份一致，并且就是刚才。
func TestARenewalPublishesItsOwnTimestamp(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		if response, ok := out.(*upstreamResponse); ok && method == "host.http.do" {
			response.StatusCode = 200
			response.Body = []byte(`{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoyLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.2.fake","expires_in":864000}`)
		}
		return nil
	})
	raw, _ := json.Marshal(authRefreshRequest{
		AuthID:      "bp-a",
		StorageJSON: []byte(`{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoxLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.1.fake","account_id":"acct","last_refresh":"2026-10-05T00:00:00Z"}`),
		Metadata:    map[string]any{"file_name": "a.json"},
	})
	before := time.Now().Add(-time.Second)
	response, err := service.authRefresh(raw)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Second)
	auth := objectValue(response["Auth"])
	published, _ := objectValue(auth["Metadata"])["last_refresh"].(string)
	stamp, errParse := time.Parse(time.RFC3339, published)
	if errParse != nil {
		t.Fatalf("续期交回的记录里 last_refresh = %q，不是 RFC 3339", published)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Errorf("续期交回的 last_refresh = %s，不是刚才（%s ~ %s）", stamp, before, after)
	}
	storage, _ := auth["StorageJSON"].([]byte)
	var stored map[string]any
	if err := json.Unmarshal(storage, &stored); err != nil {
		t.Fatalf("续期交回的 StorageJSON 不是 JSON：%v", err)
	}
	if stored["last_refresh"] != published {
		t.Errorf("记录里 last_refresh = %q，要写回文件的那一份里是 %v，两边必须一致", published, stored["last_refresh"])
	}
	if _, has := objectValue(auth["Metadata"])["expired"]; !has {
		t.Error("续期给了 expires_in，记录里却没有 expired")
	}
}

// 令牌端点的拒绝要按本来的意思交给宿主：只有 400/401 是号本身的问题（401），429 是限流（429
// 带 Retry-After），其余是端点自己的故障（502，503 保留）。以前一律 401——v8 里一次 429
// 就能把一个好号永久下线。
func TestTokenEndpointRefusalsKeepTheirMeaning(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    http.Header
		body       string
		wantStatus int
		wantKind   string
		contains   []string
		excludes   []string
	}{
		{"429 带 Retry-After", 429, http.Header{"Retry-After": {"60"}}, `{"error":"rate_limited"}`, 429, "refresh_rate_limited", []string{"status 429", "Retry-After 60"}, []string{"status 401"}},
		{"429 不带 Retry-After", 429, nil, ``, 429, "refresh_rate_limited", []string{"status 429"}, []string{"Retry-After", "status 401"}},
		{"429 的 Retry-After 是日期", 429, http.Header{"Retry-After": {"Wed, 21 Oct 2026 07:28:00 GMT"}}, ``, 429, "refresh_rate_limited", []string{"Retry-After Wed, 21 Oct 2026 07:28:00 GMT"}, nil},
		{"500", 500, nil, `{"error":"server_error"}`, 502, "refresh_upstream_error", []string{"status 500"}, []string{"status 401"}},
		{"502", 502, nil, ``, 502, "refresh_upstream_error", []string{"status 502"}, []string{"status 401"}},
		{"503", 503, nil, ``, 503, "refresh_upstream_error", []string{"status 503"}, []string{"status 401"}},
		{"403（WAF 一类）", 403, nil, `<html>blocked</html>`, 502, "refresh_upstream_error", []string{"status 403"}, []string{"status 401", "html"}},
		{"404", 404, nil, ``, 502, "refresh_upstream_error", []string{"status 404"}, []string{"status 401"}},
		{"400 invalid_grant", 400, nil, `{"error":"invalid_grant","error_description":"The refresh token is invalid"}`, 401, "refresh_rejected", []string{"status 400", "(invalid_grant)"}, []string{"The refresh token"}},
		{"401 对象形式的 error", 401, nil, `{"error":{"code":"invalid_client","message":"nope"}}`, 401, "refresh_rejected", []string{"status 401", "(invalid_client)"}, []string{"nope"}},
		{"400 看不懂的 body", 400, nil, `not json`, 401, "refresh_rejected", []string{"status 400"}, []string{"("}},
		{"400 带脏字符的 error", 400, nil, `{"error":"<script>x</script>"}`, 401, "refresh_rejected", []string{"status 400"}, []string{"<", "script"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService()
			service.SetHost(func(method string, payload any, out any) error {
				if response, ok := out.(*upstreamResponse); ok && method == "host.http.do" {
					response.StatusCode = tc.status
					response.Headers = tc.headers
					response.Body = []byte(tc.body)
				}
				return nil
			})
			raw, _ := json.Marshal(authRefreshRequest{
				AuthID:      "bp-a",
				StorageJSON: []byte(`{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoxLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.1.fake","account_id":"acct"}`),
				Metadata:    map[string]any{"file_name": "a.json"},
			})
			_, err := service.authRefresh(raw)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v，不是 *APIError", err)
			}
			if apiErr.Status != tc.wantStatus || apiErr.Kind != tc.wantKind {
				t.Errorf("status/kind = %d/%s，应当是 %d/%s（%s）", apiErr.Status, apiErr.Kind, tc.wantStatus, tc.wantKind, apiErr.Message)
			}
			for _, want := range tc.contains {
				if !strings.Contains(apiErr.Message, want) {
					t.Errorf("消息 %q 里应当有 %q", apiErr.Message, want)
				}
			}
			for _, bad := range tc.excludes {
				if strings.Contains(apiErr.Message, bad) {
					t.Errorf("消息 %q 里不该有 %q", apiErr.Message, bad)
				}
			}
		})
	}
}

// 续期成功的路径不受影响：200 照样换令牌。
func TestASuccessfulRenewalStillRenews(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		if response, ok := out.(*upstreamResponse); ok && method == "host.http.do" {
			response.StatusCode = 200
			response.Body = []byte(`{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoyLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.2.fake","expires_in":864000}`)
		}
		return nil
	})
	raw, _ := json.Marshal(authRefreshRequest{
		AuthID:      "bp-a",
		StorageJSON: []byte(`{"access_token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoxLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature","refresh_token":"rt.1.fake","account_id":"acct"}`),
		Metadata:    map[string]any{"file_name": "a.json"},
	})
	response, err := service.authRefresh(raw)
	if err != nil {
		t.Fatal(err)
	}
	storage, _ := objectValue(response["Auth"])["StorageJSON"].([]byte)
	var stored map[string]any
	_ = json.Unmarshal(storage, &stored)
	if stored["access_token"] != "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmaXh0dXJlLXVzZXIiLCJjbGllbnRfaWQiOiJhcHBfRU1vYW1FRVo3M2YwQ2tYYVhwN2hyYW5uIiwiZXhwIjo0MTAyNDQ0ODAwLCJ2ZXJzaW9uIjoyLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdCIsInVzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3VzZXJfaWQiOiJmaXh0dXJlLXVzZXIiLCJjaGF0Z3B0X3BsYW5fdHlwZSI6InRlYW0ifX0.fixture-signature" || stored["refresh_token"] != "rt.2.fake" {
		t.Errorf("200 没有换令牌（access=%v refresh=%v）", stored["access_token"], stored["refresh_token"])
	}
}
