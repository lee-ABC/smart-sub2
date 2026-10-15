package basispoints

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Quota reporting.
//
// Basis Points itself reports nothing: its responses carry no rate-limit
// payload (verified against a live account -- nine event types, none with
// rate_limits), and it exposes no usage endpoint. The quota that governs these
// requests is the account's ChatGPT quota, and that is readable from the same
// OAuth token through the ChatGPT usage endpoint, which is what quota.fetch
// queries on demand.
//
// Shape returned by the endpoint:
//
//	{ "plan_type": "team",
//	  "rate_limit": {
//	    "allowed": true, "limit_reached": false,
//	    "primary_window":   { "used_percent", "limit_window_seconds", "reset_after_seconds", "reset_at" },
//	    "secondary_window": { ... } } }
const (
	whamUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	// The management UI may refresh several cards at once; a short cache keeps
	// that from turning into a burst of upstream calls.
	quotaCacheTTL   = 30 * time.Second
	quotaCacheLimit = 64
)

type quotaWindow struct {
	UsedPercent float64
	WindowSecs  int64
	ResetAt     time.Time
}

type quotaSnapshot struct {
	Plan         string
	Primary      *quotaWindow
	Secondary    *quotaWindow
	LimitReached bool
	FetchedAt    time.Time
	// Raw is the upstream body as it arrived. It is kept so the control
	// panel's own quota card can be answered from this plugin, which sends
	// the credential's established client identity, rather than from the
	// panel, which sends a Codex terminal's. See quotarefresh.go.
	Raw []byte
}

var (
	quotaMu    sync.RWMutex
	quotaCache = map[string]quotaSnapshot{}
)

func cachedQuota(key string) (quotaSnapshot, bool) {
	quotaMu.RLock()
	defer quotaMu.RUnlock()
	snapshot, ok := quotaCache[key]
	if !ok || time.Since(snapshot.FetchedAt) > quotaCacheTTL {
		return quotaSnapshot{}, false
	}
	return snapshot, true
}

func storeQuota(key string, snapshot quotaSnapshot) {
	if key == "" {
		return
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if len(quotaCache) >= quotaCacheLimit {
		for existing := range quotaCache {
			if existing != key {
				delete(quotaCache, existing)
				break
			}
		}
	}
	quotaCache[key] = snapshot
}

func quotaDescribe() map[string]any {
	return map[string]any{
		"supported_providers": []string{Provider},
		"display_name":        "OpenAI Basis Points",
		"supports_reset":      false,
	}
}

type quotaFetchRequest struct {
	AuthID      string            `json:"auth_id"`
	AuthIndex   string            `json:"auth_index"`
	Provider    string            `json:"provider"`
	StorageJSON []byte            `json:"storage_json"`
	Metadata    map[string]any    `json:"metadata"`
	Attributes  map[string]string `json:"attributes"`
}

// quotaFetch reads the credential's live ChatGPT usage and renders it in CPA's
// normalized quota shape.
func (s *Service) quotaFetch(raw []byte) (map[string]any, error) {
	var request quotaFetchRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fail(400, "invalid_request", "quota request is invalid")
		}
	}
	// Quota is informational. Every failure below is reported inside a
	// well-formed response rather than as an error, because the host records a
	// plugin error against the credential itself: an error carrying 401 sets
	// LastError, which hasUnauthorizedAuthFailure treats as terminal and takes
	// the credential out of service. A failed usage lookup must never disable
	// a credential that can still serve traffic.
	if len(request.StorageJSON) == 0 {
		return quotaUnavailable("额度不可用：请求未携带凭据"), nil
	}
	c, err := parseCredential(request.StorageJSON)
	if err != nil {
		return quotaUnavailable("额度不可用：凭据无法解析"), nil
	}

	key := strings.TrimSpace(request.AuthID)
	if key == "" {
		key = c.AccountID
	}
	snapshot, ok := cachedQuota(key)
	if !ok {
		snapshot, err = s.fetchUsage(c)
		if err != nil {
			return quotaUnavailable("额度读取失败：" + safeError(err)), nil
		}
		storeQuota(key, snapshot)
	}
	return renderQuota(snapshot), nil
}

// quotaUnavailable renders a reason in place of quota, keeping the credential
// card intact and never signalling an auth failure to the host.
func quotaUnavailable(reason string) map[string]any {
	return map[string]any{
		"summary": []map[string]any{{
			"key":    "status",
			"label":  reason,
			"value":  0,
			"format": "text",
		}},
		"groups": []map[string]any{},
	}
}

func (s *Service) fetchUsage(c credential) (quotaSnapshot, error) {
	// This host is in the Chrome TLS allow-list, so the handshake below is a
	// full browser one; sending four headers behind it described a client that
	// does not exist. The real add-in does reach chatgpt.com -- the capture
	// catches it posting to /ces/v1/rgstr with auth and cookies -- so the
	// traffic itself is in character, and what was out of character was the
	// bare header set.
	cfg := s.config()
	profile := profileFor(cfg)
	headers := map[string][]string{
		"Authorization":      {"Bearer " + c.AccessToken},
		"ChatGPT-Account-ID": {c.AccountID},
		"Accept":             {"application/json"},
		"User-Agent":         {profile.userAgent},
		"Sec-Ch-Ua":          {profile.secCHUA},
		"Sec-Ch-Ua-Mobile":   {"?0"},
		"Sec-Ch-Ua-Platform": {`"` + profile.platform + `"`},
		"Sec-Fetch-Dest":     {"empty"},
		"Sec-Fetch-Mode":     {"cors"},
		"Sec-Fetch-Site":     {"same-origin"},
		"Origin":             {"https://chatgpt.com"},
		"Priority":           {"u=1, i"},
	}
	if language := strings.TrimSpace(cfg.ClientIdentity.AcceptLanguage); language != "" {
		headers["Accept-Language"] = []string{language}
	}
	var response upstreamResponse
	callErr := s.call("host.http.do", map[string]any{
		"method":  http.MethodGet,
		"url":     whamUsageURL,
		"headers": headers,
	}, &response)
	if callErr != nil {
		return quotaSnapshot{}, fail(502, "quota_transport", "ChatGPT usage request failed: "+safeError(callErr))
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		// Deliberately not a 401: quotaFetch swallows this into a status line,
		// and letting an auth-shaped code escape would disable the credential.
		return quotaSnapshot{}, fail(502, "quota_rejected", "ChatGPT rejected the credential while reading usage")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return quotaSnapshot{}, fail(502, "quota_rejected", "ChatGPT usage endpoint returned an unexpected status")
	}

	var root map[string]any
	if err := json.Unmarshal(response.Body, &root); err != nil || root == nil {
		return quotaSnapshot{}, fail(502, "quota_invalid", "ChatGPT usage response is not a JSON object")
	}
	limits := objectValue(firstValue(root, "rate_limit", "rate_limits", "rateLimit"))
	snapshot := quotaSnapshot{
		Plan:      firstString(root, "plan_type", "planType"),
		FetchedAt: time.Now(),
		Raw:       response.Body,
	}
	if limits != nil {
		snapshot.LimitReached = boolValue(firstValue(limits, "limit_reached", "limitReached"))
		snapshot.Primary = quotaWindowFrom(limits, "primary_window", "primary")
		snapshot.Secondary = quotaWindowFrom(limits, "secondary_window", "secondary")
	}
	if snapshot.Plan == "" {
		snapshot.Plan = credentialPlan(c)
	}
	return snapshot, nil
}

// quotaWindowFrom accepts both spellings of a window: the usage endpoint uses
// primary_window/limit_window_seconds, while the Codex event stream uses
// primary/window_minutes.
func quotaWindowFrom(limits map[string]any, names ...string) *quotaWindow {
	var window map[string]any
	for _, name := range names {
		if window = objectValue(limits[name]); window != nil {
			break
		}
	}
	if window == nil {
		return nil
	}
	used, hasUsed := floatValue(firstValue(window, "used_percent", "usedPercent"))
	if !hasUsed || used < 0 || used > 100 {
		return nil
	}
	result := &quotaWindow{UsedPercent: used}
	if seconds, ok := floatValue(firstValue(window, "limit_window_seconds", "limitWindowSeconds")); ok && seconds > 0 {
		result.WindowSecs = int64(seconds)
	} else if minutes, ok := floatValue(firstValue(window, "window_minutes", "windowMinutes")); ok && minutes > 0 {
		result.WindowSecs = int64(minutes) * 60
	}
	if at, ok := floatValue(firstValue(window, "reset_at", "resetAt")); ok && at > 0 {
		result.ResetAt = time.Unix(int64(at), 0)
	} else if after, ok := floatValue(firstValue(window, "reset_after_seconds", "resetAfterSeconds")); ok && after >= 0 {
		result.ResetAt = time.Now().Add(time.Duration(after) * time.Second)
	}
	return result
}

func renderQuota(snapshot quotaSnapshot) map[string]any {
	response := map[string]any{}
	if plan := strings.TrimSpace(snapshot.Plan); plan != "" {
		response["subscription"] = map[string]any{"plan": plan}
	}

	summary := []map[string]any{}
	buckets := []map[string]any{}
	for _, entry := range []struct {
		window *quotaWindow
		key    string
		label  string
	}{
		{snapshot.Primary, "primary", "主窗口"},
		{snapshot.Secondary, "secondary", "次窗口"},
	} {
		if entry.window == nil {
			continue
		}
		name := entry.label
		if span := humanWindow(entry.window.WindowSecs); span != "" {
			name += "（" + span + "）"
		}
		summary = append(summary, map[string]any{
			"key":    entry.key + "_used_percent",
			"label":  name + " 已用",
			"value":  entry.window.UsedPercent,
			"unit":   "%",
			"format": "percent",
		})
		bucket := map[string]any{
			"window":            humanWindow(entry.window.WindowSecs),
			"remainingFraction": (100 - entry.window.UsedPercent) / 100,
			"description":       name,
		}
		if !entry.window.ResetAt.IsZero() {
			bucket["resetTime"] = entry.window.ResetAt.UTC().Format(time.RFC3339)
		}
		buckets = append(buckets, bucket)
	}

	if len(summary) == 0 {
		summary = append(summary, map[string]any{
			"key":    "status",
			"label":  "ChatGPT 未返回额度窗口",
			"value":  0,
			"format": "text",
		})
	}
	if snapshot.LimitReached {
		summary = append(summary, map[string]any{
			"key":    "limit_reached",
			"label":  "已触发限流",
			"value":  1,
			"format": "text",
		})
	}
	response["summary"] = summary
	response["groups"] = []map[string]any{{
		"displayName": "ChatGPT 账号额度",
		"buckets":     buckets,
	}}
	return response
}

func humanWindow(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds%86400 == 0:
		return itoa(seconds/86400) + " 天"
	case seconds%3600 == 0:
		return itoa(seconds/3600) + " 小时"
	case seconds%60 == 0:
		return itoa(seconds/60) + " 分钟"
	default:
		return itoa(seconds) + " 秒"
	}
}

func floatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func boolValue(value any) bool {
	flag, _ := value.(bool)
	return flag
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
