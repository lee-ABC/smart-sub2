package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func testCurrentTurnStateAccount() *Account {
	return &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
}

func TestExtractOpenAICurrentTurnState(t *testing.T) {
	require.Equal(t, "state-top", extractOpenAICurrentTurnState([]byte(`{"current_turn_state":"state-top"}`)))
	require.Equal(t, "state-response", extractOpenAICurrentTurnState([]byte(`{"response":{"current_turn_state":"state-response"}}`)))
	require.Equal(t, "state-sse", extractOpenAICurrentTurnState([]byte("data: {\"current_turn_state\":\"state-sse\"}\n\n")))
	require.Empty(t, extractOpenAICurrentTurnState([]byte(`{"current_turn_state":""}`)))
	require.Empty(t, extractOpenAICurrentTurnState([]byte(`{"current_turn_state":"`+strings.Repeat("a", openAICurrentTurnStateMax+1)+`"}`)))
}

func TestPrepareOpenAICurrentTurnStateBodyIsAccountScoped(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := testCurrentTurnStateAccount()
	require.True(t, svc.recordOpenAICurrentTurnState(context.Background(), account, "gpt-5.6-sol", "state-A"))

	body := svc.prepareOpenAICurrentTurnStateBody(context.Background(), account, []byte(`{"model":"gpt-5.6-sol","current_turn_state":"foreign"}`))
	require.Equal(t, "state-A", gjson.GetBytes(body, "current_turn_state").String())

	other := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body = svc.prepareOpenAICurrentTurnStateBody(context.Background(), other, []byte(`{"model":"gpt-5.6-sol","current_turn_state":"foreign"}`))
	require.Empty(t, gjson.GetBytes(body, "current_turn_state").String())
}

func TestOpenAICurrentTurnStateSignals(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := testCurrentTurnStateAccount()

	require.True(t, svc.handleOpenAICurrentTurnStateSignal(context.Background(), account, "gpt-5.6-sol", openAICurrentTurnState292, http.Header{}, []byte(`{"current_turn_state":"state-A"}`)))
	require.Equal(t, "state-A", svc.getOpenAICurrentTurnState(context.Background(), account, "gpt-5.6-sol"))

	require.True(t, svc.handleOpenAICurrentTurnStateSignal(context.Background(), account, "gpt-5.6-sol", openAICurrentTurnState312, nil, nil))
	require.Empty(t, svc.getOpenAICurrentTurnState(context.Background(), account, "gpt-5.6-sol"))
}
