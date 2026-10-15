package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"strings"
	"time"
)

const excelResponsesWebSocketURL = "wss://bps.openai.com/basispoints/api/responses"

func excelWSRoute(ctx context.Context) *excelRequestRoute {
	route, _ := ctx.Value(excelRequestRouteKey{}).(*excelRequestRoute)
	if route == nil || !route.websocket {
		return nil
	}
	return route
}
func isExcelWebSocket(ctx context.Context) bool { return excelWSRoute(ctx) != nil }

// Conservative first rollout: preserve the existing HTTP adapter for tools,
// files, images, compaction and server-side history rather than lose features.
func excelWebSocketTextRequest(raw []byte) bool {
	if len(raw) >= int(openAIWSHTTPBridgeThresholdBytesDefault) {
		return false
	}
	var b map[string]json.RawMessage
	if json.Unmarshal(raw, &b) != nil || b == nil {
		return false
	}
	for _, k := range []string{"tools", "previous_response_id", "tool_choice"} {
		if v, ok := b[k]; ok && string(v) != "null" && string(v) != "[]" && string(v) != `""` {
			return false
		}
	}
	var text string
	if len(b["input"]) > 0 && b["input"][0] == 34 && json.Unmarshal(b["input"], &text) == nil {
		return true
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(b["input"], &items) != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.Type != "" && item.Type != "message" {
			return false
		}
		switch item.Role {
		case "user", "assistant", "developer", "system":
		default:
			return false
		}
		if len(item.Content) > 0 && item.Content[0] == 34 && json.Unmarshal(item.Content, &text) == nil {
			continue
		}
		var parts []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if json.Unmarshal(item.Content, &parts) != nil || len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if (part.Type != "input_text" && part.Type != "output_text" && part.Type != "text") || part.Text == nil {
				return false
			}
		}
	}
	return true
}
func enableExcelWebSocket(ctx context.Context, raw []byte) {
	route, _ := ctx.Value(excelRequestRouteKey{}).(*excelRequestRoute)
	if route != nil && !route.config.UsesCPAAdapter() && route.config.WebSocketEnabled && excelWebSocketTextRequest(raw) {
		route.websocket = true
	}
}
func (s *OpenAIGatewayService) buildOpenAIResponsesWSURLForContext(ctx context.Context, account *Account) (string, error) {
	if isExcelWebSocket(ctx) {
		return excelResponsesWebSocketURL, nil
	}
	return s.buildOpenAIResponsesWSURL(account)
}
func excelWebSocketHeaders(ctx context.Context, token string) (http.Header, error) {
	route := excelWSRoute(ctx)
	if route == nil || len(route.wsHeaders) == 0 {
		return nil, errors.New("Excel WS request was not prepared")
	}
	h := route.wsHeaders.Clone()
	h.Set("Authorization", "Bearer "+token)
	return h, nil
}
func (s *OpenAIGatewayService) prepareExcelWebSocketPayload(ctx context.Context, c *gin.Context, account *Account, token string, raw []byte) ([]byte, error) {
	route := excelWSRoute(ctx)
	if route == nil {
		return raw, nil
	}
	var input struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &input) != nil || !IsExcelSupportedModel(input.Model) || !excelWebSocketTextRequest(raw) {
		return nil, errors.New("Excel WS text session requires full text history and an Excel model; start a new request for tools, images or another transport")
	}
	key, err := route.config.TransportKey()
	if err != nil {
		return nil, err
	}
	identity, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, err
	}
	workspace := identity.GetChatGPTAccountID()
	if workspace == "" {
		return nil, errors.New("selected account has no ChatGPT workspace identity")
	}
	scope := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%s", route.keyID, route.groupID, account.ID, workspace))))
	prepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(prepCtx, http.MethodPost, strings.TrimRight(route.config.GatewayURL, "/")+"/ws/prepare", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sub2-Excel-Access-Token", token)
	req.Header.Set("X-Sub2-Excel-Account-Id", workspace)
	req.Header.Set("X-Sub2-Excel-Scope", scope)
	if user := identity.GetChatGPTUserID(); user != "" {
		req.Header.Set("X-Sub2-Excel-Account-User-Id", user)
	}
	resp, err := s.httpUpstream.Do(req, "", account.ID, account.Mode1EffectiveConcurrency())
	if err != nil {
		return nil, &excelTransportError{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &excelTransportError{}
	}
	const limit = 32 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return nil, &excelTransportError{}
	}
	var prepared struct {
		Payload json.RawMessage   `json:"payload"`
		Headers map[string]string `json:"headers"`
	}
	if json.Unmarshal(data, &prepared) != nil || !json.Valid(prepared.Payload) {
		return nil, &excelTransportError{}
	}
	var wire struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(prepared.Payload, &wire) != nil || wire.Type != "response.create" || !IsExcelSupportedModel(wire.Model) {
		return nil, &excelTransportError{}
	}
	if route.wsHeaders == nil {
		h := http.Header{}
		for k, v := range prepared.Headers {
			k = strings.ToLower(k)
			if k == "origin" || k == "user-agent" || k == "content-type" || k == "accept-encoding" || k == "x-basispoints-auth-mode" || strings.HasPrefix(k, "x-openai-internal-basispoints-") || strings.HasPrefix(k, "x-stainless-") {
				if strings.ContainsRune(v, 13) || strings.ContainsRune(v, 10) {
					return nil, &excelTransportError{}
				}
				h.Set(k, v)
			}
		}
		h.Set("Origin", "https://bps.openai.com")
		h.Set("Accept", "*/*")
		h.Set("X-Basispoints-Auth-Mode", "chatgpt")
		h.Set("Chatgpt-Account-Id", workspace)
		h.Set("X-Openai-Account-Id", workspace)
		h.Set("Session_Id", scope)
		if user := identity.GetChatGPTUserID(); user != "" {
			h.Set("X-Openai-Account-User-Id", user)
		}
		route.wsHeaders = h
	}
	logOpenAIWSModeInfo("excel_bps_ws_prepared account_id=%d group_id=%d model=%s", account.ID, route.groupID, wire.Model)
	return prepared.Payload, nil
}
