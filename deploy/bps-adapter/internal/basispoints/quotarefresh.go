package basispoints

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Quota for one credential, read now.
//
// The background sampler only reads a credential that is serving traffic, so a
// credential that stops -- disabled, exhausted or banned -- keeps whatever
// reading it had when it stopped. The dollar figures are that percentage
// multiplied by a measured rate, so they freeze too, and a credential with
// nothing left goes on reporting what it had hours ago. The figure is not
// miscalculated; it is old, and a number that never moves looks more
// trustworthy than one that does, not less.
//
// This route answers "read this one credential now". It differs from the quota
// page in managementpage.go in three ways, each of which was a defect there:
//
//   - One credential, named by file or auth index, instead of all of them. The
//     page takes no filter, so refreshing one credential queried every
//     credential: 48 tokens hitting one endpoint from one address inside a
//     second. That is precisely the pool-wide uniformity the identity work
//     exists to prevent, arriving through the management UI instead of the
//     relay.
//   - The reading is written back to the ledger, so the dollar figures move
//     with it. Reading upstream and discarding the answer is why a card could
//     show two percentages that disagreed.
//   - It can return the upstream body verbatim, so the control panel's own
//     quota card can be served from here rather than reaching chatgpt.com
//     itself. The panel classifies oai-basispoints credentials as Codex
//     (`_bp === 'codex' || _bp === 'oai-basispoints'`) and sends
//     `codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11` on the
//     credential's own token: a Mac terminal on an account whose every other
//     request is an Excel add-in on Windows. Same endpoint, same token, two
//     irreconcilable clients.
type quotaRefreshResult struct {
	File         string  `json:"file,omitempty"`
	Label        string  `json:"label,omitempty"`
	Plan         string  `json:"plan,omitempty"`
	UsedPercent  float64 `json:"used_percent"`
	WindowSecs   int64   `json:"window_secs,omitempty"`
	ResetAt      string  `json:"reset_at,omitempty"`
	LimitReached bool    `json:"limit_reached"`
	FetchedAt    string  `json:"fetched_at,omitempty"`
	// Noted reports that the reading reached the ledger, so the caller can
	// tell a refresh that corrected the dollar figures from one that only
	// displayed a number.
	Noted bool   `json:"noted"`
	Error string `json:"error,omitempty"`
}

func firstQuery(query map[string][]string, name string) string {
	if values := query[name]; len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

// serveQuotaRefresh reads one credential's quota and files it.
//
// Errors come back as JSON with a status code rather than as the HTML frame,
// because every caller of this route parses it.
func (s *Service) serveQuotaRefresh(query map[string][]string) map[string]any {
	file := firstQuery(query, "file")
	index := firstQuery(query, "auth_index")
	raw := strings.EqualFold(firstQuery(query, "format"), "raw")
	if file == "" && index == "" {
		return quotaRefreshJSON(http.StatusBadRequest, nil,
			quotaRefreshResult{Error: "要指定 file 或 auth_index"})
	}

	authIndex, name, label, err := s.findCredentialFile(file, index)
	if err != nil {
		return quotaRefreshJSON(http.StatusNotFound, nil, quotaRefreshResult{
			File: file, Error: safeError(err)})
	}

	var record struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := s.call("host.auth.get", map[string]any{"auth_index": authIndex}, &record); err != nil {
		return quotaRefreshJSON(http.StatusBadGateway, nil, quotaRefreshResult{
			File: name, Label: label, Error: "读取凭据失败：" + safeError(err)})
	}
	c, parseErr := parseCredential(record.JSON)
	if parseErr != nil {
		return quotaRefreshJSON(http.StatusBadGateway, nil, quotaRefreshResult{
			File: name, Label: label, Error: "凭据无法解析"})
	}

	// The cache is kept even on an explicit refresh. A person clicking twice
	// wants a current number, not two upstream calls, and thirty seconds is
	// shorter than the interval over which a quota percentage visibly moves.
	snapshot, ok := cachedQuota(authIndex)
	if !ok {
		snapshot, err = s.fetchUsage(c)
		if err != nil {
			return quotaRefreshJSON(http.StatusBadGateway, nil, quotaRefreshResult{
				File: name, Label: label, Error: "额度读取失败：" + safeError(err)})
		}
		storeQuota(authIndex, snapshot)
	}

	noted := s.noteQuotaForFile(name, snapshot)
	if raw {
		if len(snapshot.Raw) == 0 {
			return quotaRefreshJSON(http.StatusBadGateway, nil, quotaRefreshResult{
				File: name, Label: label, Error: "上游没有返回可转发的原文"})
		}
		return map[string]any{
			"StatusCode": http.StatusOK,
			"Headers": map[string][]string{
				"Content-Type":  {"application/json; charset=utf-8"},
				"Cache-Control": {"no-store"},
			},
			"Body": snapshot.Raw,
		}
	}

	result := quotaRefreshResult{
		File:         name,
		Label:        label,
		Plan:         snapshot.Plan,
		LimitReached: snapshot.LimitReached,
		FetchedAt:    snapshot.FetchedAt.UTC().Format(time.RFC3339),
		Noted:        noted,
	}
	if window := governingWindow(snapshot); window != nil {
		result.UsedPercent = window.UsedPercent
		result.WindowSecs = window.WindowSecs
		if !window.ResetAt.IsZero() {
			result.ResetAt = window.ResetAt.UTC().Format(time.RFC3339)
		}
	} else {
		result.Error = "ChatGPT 未返回额度窗口"
	}
	return quotaRefreshJSON(http.StatusOK, nil, result)
}

func quotaRefreshJSON(status int, _ any, result quotaRefreshResult) map[string]any {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"error":"结果无法序列化"}`)
		status = http.StatusInternalServerError
	}
	return map[string]any{
		"StatusCode": status,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": body,
	}
}

// findCredentialFile resolves a file name or an auth index to both, so the
// caller can look the credential up and still file the reading under the name
// the ledger uses.
func (s *Service) findCredentialFile(file, index string) (authIndex, name, label string, err error) {
	var list struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Label     string `json:"label"`
			Provider  string `json:"provider"`
			Type      string `json:"type"`
		} `json:"files"`
	}
	if callErr := s.call("host.auth.list", map[string]any{}, &list); callErr != nil {
		return "", "", "", fail(502, "auth_list", "无法读取凭据列表："+safeError(callErr))
	}
	for _, entry := range list.Files {
		if !strings.EqualFold(entry.Provider, Provider) && !strings.EqualFold(entry.Type, Provider) {
			continue
		}
		if (file != "" && strings.EqualFold(entry.Name, file)) ||
			(index != "" && entry.AuthIndex == index) {
			return entry.AuthIndex, entry.Name, entry.Label, nil
		}
	}
	return "", "", "", fail(404, "auth_missing", "没有找到这个凭据")
}

// noteQuotaForFile files an on-demand reading under the key the ledger already
// uses for this credential.
//
// addSample creates an account when the key is unknown, so writing back under
// a key derived here rather than the one the executor files under would not
// fail loudly: it would add a second entry for the same credential, with no
// spend and a full quota, which then joins the pooled median and drags every
// other credential's estimate with it. The key is therefore never constructed,
// only looked up, and a reading with nowhere to go is dropped. A credential
// that has never served a request has no spend to correct, so dropping it
// costs nothing.
func (s *Service) noteQuotaForFile(file string, snapshot quotaSnapshot) bool {
	if s == nil || s.ledger == nil || strings.TrimSpace(file) == "" {
		return false
	}
	key, label, ok := s.ledger.keyForFile(file)
	if !ok {
		return false
	}
	s.ledger.addSample(usageTarget{Key: key, File: file, Label: label}, snapshot)
	return true
}
