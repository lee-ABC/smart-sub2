//go:build unit

package handler

import (
	"context"
	"errors"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCredentialFailoverResponseKeepsProviderIdentity(t *testing.T) {
	tests := []struct {
		name    string
		reason  service.GatewayFailureReason
		message string
	}{
		{"OpenAI native", service.NativeOAuthCredentialsUnavailableReason, service.NativeOAuthCredentialsUnavailableMessage},
		{"OpenAI BPS", service.BPSOAuthCredentialsUnavailableReason, service.BPSOAuthCredentialsUnavailableMessage},
		{"Grok", service.GrokCredentialReasonRevoked, service.GrokCredentialUnavailableClientMessage},
		{"unknown provider", "other_provider_credentials", "No healthy upstream account is currently available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &service.UpstreamFailoverError{Stage: service.GatewayFailureStageAccountAuth, Reason: tt.reason, ClientMessage: "unsafe_token=must-not-leak", ClientStatusCode: 418}
			status, message := credentialFailoverClientResponse(err)
			require.Equal(t, http.StatusServiceUnavailable, status)
			require.Equal(t, tt.message, message)
			require.NotContains(t, message, "must-not-leak")
			if tt.reason != service.GrokCredentialReasonRevoked {
				require.NotContains(t, message, "Grok")
			}
		})
	}
}

func TestCredentialFailoverHTTPProtocolsKeepOpenAIMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reason := range []service.GatewayFailureReason{service.NativeOAuthCredentialsUnavailableReason, service.BPSOAuthCredentialsUnavailableReason} {
		_, expected := credentialFailoverClientResponse(&service.UpstreamFailoverError{Reason: reason})
		for _, protocol := range []string{"responses", "messages", "chat"} {
			t.Run(string(reason)+"/"+protocol, func(t *testing.T) {
				r := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(r)
				failure := &service.UpstreamFailoverError{Stage: service.GatewayFailureStageAccountAuth, Scope: service.GatewayFailureScopeRequest, Reason: reason, RequestScopedTransient: true}
				switch protocol {
				case "responses":
					(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failure, false)
				case "messages":
					(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, failure, false)
				case "chat":
					(&GatewayHandler{}).handleCCFailoverExhausted(c, failure, false)
				}
				require.Equal(t, http.StatusServiceUnavailable, r.Code)
				require.Equal(t, expected, gjson.Get(r.Body.String(), "error.message").String())
				require.NotContains(t, r.Body.String(), "Grok")
			})
		}
	}
}

func TestCredentialFailoverNilIsProviderNeutral(t *testing.T) {
	status, message := credentialFailoverClientResponse(nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.NotContains(t, message, "Grok")
}

func TestCredentialFailoverWebSocketKeepsOpenAIMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reason := range []service.GatewayFailureReason{service.NativeOAuthCredentialsUnavailableReason, service.BPSOAuthCredentialsUnavailableReason} {
		t.Run(string(reason), func(t *testing.T) {
			failure := &service.UpstreamFailoverError{Stage: service.GatewayFailureStageAccountAuth, Reason: reason}
			_, expected := credentialFailoverClientResponse(failure)
			router := gin.New()
			router.GET("/ws", func(c *gin.Context) {
				conn, err := websocket.Accept(c.Writer, c.Request, nil)
				if err != nil {
					return
				}
				closeOpenAIWSFailoverExhausted(c, conn, failure)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			_, _, err = conn.Read(ctx)
			var closed websocket.CloseError
			require.True(t, errors.As(err, &closed))
			require.Equal(t, websocket.StatusTryAgainLater, closed.Code)
			require.Equal(t, expected, closed.Reason)
			require.NotContains(t, closed.Reason, "Grok")
		})
	}
}
