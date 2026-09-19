package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// The article describes the state as roughly one hour lived. We intentionally
	// keep this conservative and let the upstream revoke it earlier via 312.
	openAICurrentTurnStateTTL = time.Hour
	openAICurrentTurnStateMax = 64 << 10
	openAICurrentTurnState292 = 292
	openAICurrentTurnState312 = 312
)

// OpenAICurrentTurnStateCache is optional so existing GatewayCache test doubles
// and deployments remain source-compatible. The Redis-backed gateway cache
// implements it; the gateway also keeps a process-local fallback.
type OpenAICurrentTurnStateCache interface {
	GetOpenAICurrentTurnState(ctx context.Context, accountID int64, model string) (string, error)
	SetOpenAICurrentTurnState(ctx context.Context, accountID int64, model, state string, ttl time.Duration) error
	DeleteOpenAICurrentTurnState(ctx context.Context, accountID int64, model string) error
}

type openAICurrentTurnStateLocalEntry struct {
	state     string
	expiresAt time.Time
}

func openAICurrentTurnStateModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func openAICurrentTurnStateKey(accountID int64, model string) string {
	model = openAICurrentTurnStateModel(model)
	if accountID <= 0 || model == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", accountID, model)))
	return hex.EncodeToString(digest[:])
}

func normalizeOpenAICurrentTurnState(state string) string {
	state = strings.TrimSpace(state)
	if state == "" || len(state) > openAICurrentTurnStateMax {
		return ""
	}
	return state
}

func isOpenAICurrentTurnStateAccount(account *Account) bool {
	return account != nil && account.ID > 0 && account.UsesOpenAICodexProtocol()
}

func openAICurrentTurnStateSignalStatus(statusCode int) bool {
	return statusCode == openAICurrentTurnState292 || statusCode == openAICurrentTurnState312
}

func extractOpenAICurrentTurnStateFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, key := range []string{
		"current_turn_state",
		"current-turn-state",
		"x-current-turn-state",
		"x-codex-current-turn-state",
	} {
		if state := normalizeOpenAICurrentTurnState(headers.Get(key)); state != "" {
			return state
		}
	}
	return ""
}

// extractOpenAICurrentTurnState supports the observed JSON shapes without
// assuming the upstream's envelope. It also accepts an SSE body because the
// state can be emitted inside a data frame by a compatibility upstream.
func extractOpenAICurrentTurnState(body []byte) string {
	if len(body) == 0 || !bytes.Contains(body, []byte("current_turn_state")) {
		return ""
	}
	paths := []string{
		"current_turn_state",
		"data.current_turn_state",
		"response.current_turn_state",
		"metadata.current_turn_state",
		"client_metadata.current_turn_state",
	}
	for _, path := range paths {
		if state := normalizeOpenAICurrentTurnState(gjson.GetBytes(body, path).String()); state != "" {
			return state
		}
	}
	if bytesContainsSSEFrame(body) {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if state := extractOpenAICurrentTurnState([]byte(payload)); state != "" {
				return state
			}
		}
	}
	return ""
}

func bytesContainsSSEFrame(body []byte) bool {
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:") {
			return true
		}
	}
	return false
}

// prepareOpenAICurrentTurnStateBody removes client-supplied state and replaces
// it with the state minted for the selected OAuth account. This prevents a
// failover from replaying account A's opaque credential to account B.
func (s *OpenAIGatewayService) prepareOpenAICurrentTurnStateBody(ctx context.Context, account *Account, body []byte) []byte {
	if !isOpenAICurrentTurnStateAccount(account) || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	state := s.getOpenAICurrentTurnState(ctx, account, model)
	updated := body
	if state == "" {
		if deleted, err := sjson.DeleteBytes(updated, "current_turn_state"); err == nil {
			updated = deleted
		}
		return updated
	}
	if patched, err := sjson.SetBytes(updated, "current_turn_state", state); err == nil {
		updated = patched
	}
	return updated
}

func (s *OpenAIGatewayService) getOpenAICurrentTurnState(ctx context.Context, account *Account, model string) string {
	if !isOpenAICurrentTurnStateAccount(account) {
		return ""
	}
	key := openAICurrentTurnStateKey(account.ID, model)
	if key == "" {
		return ""
	}
	now := time.Now()
	if raw, ok := s.openaiCurrentTurnStates.Load(key); ok {
		if entry, valid := raw.(openAICurrentTurnStateLocalEntry); valid && now.Before(entry.expiresAt) {
			return entry.state
		}
		s.openaiCurrentTurnStates.Delete(key)
	}
	if s.openaiCurrentTurnStateCache != nil {
		state, err := s.openaiCurrentTurnStateCache.GetOpenAICurrentTurnState(ctx, account.ID, model)
		if err == nil {
			if state = normalizeOpenAICurrentTurnState(state); state != "" {
				s.openaiCurrentTurnStates.Store(key, openAICurrentTurnStateLocalEntry{state: state, expiresAt: now.Add(openAICurrentTurnStateTTL)})
				return state
			}
		}
	}
	return ""
}

func (s *OpenAIGatewayService) recordOpenAICurrentTurnState(ctx context.Context, account *Account, model, state string) bool {
	if !isOpenAICurrentTurnStateAccount(account) {
		return false
	}
	state = normalizeOpenAICurrentTurnState(state)
	key := openAICurrentTurnStateKey(account.ID, model)
	if state == "" || key == "" {
		return false
	}
	now := time.Now()
	s.openaiCurrentTurnStates.Store(key, openAICurrentTurnStateLocalEntry{state: state, expiresAt: now.Add(openAICurrentTurnStateTTL)})
	if s.openaiCurrentTurnStateCache != nil {
		// Persistence is best-effort: a Redis outage must not turn a successful
		// upstream response into a gateway error.
		_ = s.openaiCurrentTurnStateCache.SetOpenAICurrentTurnState(ctx, account.ID, model, state, openAICurrentTurnStateTTL)
	}
	return true
}

func (s *OpenAIGatewayService) captureOpenAICurrentTurnState(ctx context.Context, account *Account, model string, headers http.Header, body []byte) bool {
	if !isOpenAICurrentTurnStateAccount(account) {
		return false
	}
	state := extractOpenAICurrentTurnStateFromHeaders(headers)
	if state == "" {
		state = extractOpenAICurrentTurnState(body)
	}
	return s.recordOpenAICurrentTurnState(ctx, account, model, state)
}

func (s *OpenAIGatewayService) invalidateOpenAICurrentTurnState(ctx context.Context, account *Account, model string) {
	if !isOpenAICurrentTurnStateAccount(account) {
		return
	}
	key := openAICurrentTurnStateKey(account.ID, model)
	if key != "" {
		s.openaiCurrentTurnStates.Delete(key)
	}
	if s.openaiCurrentTurnStateCache != nil {
		_ = s.openaiCurrentTurnStateCache.DeleteOpenAICurrentTurnState(ctx, account.ID, model)
	}
}

func (s *OpenAIGatewayService) handleOpenAICurrentTurnStateSignal(ctx context.Context, account *Account, model string, statusCode int, headers http.Header, body []byte) (retry bool) {
	if !isOpenAICurrentTurnStateAccount(account) || !openAICurrentTurnStateSignalStatus(statusCode) {
		return false
	}
	if statusCode == openAICurrentTurnState312 {
		s.invalidateOpenAICurrentTurnState(ctx, account, model)
		logger.LegacyPrintf("service.openai_current_turn_state", "OpenAI current_turn_state revoked (account=%d model=%s status=312)", account.ID, model)
		return true
	}
	// 292 is the state minting response. Capture it and let the caller replay
	// the original request once with the newly minted state.
	captured := s.captureOpenAICurrentTurnState(ctx, account, model, headers, body)
	logger.LegacyPrintf("service.openai_current_turn_state", "OpenAI current_turn_state signal (account=%d model=%s status=292 captured=%v)", account.ID, model, captured)
	return captured
}
