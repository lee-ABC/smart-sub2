package basispoints

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestH23DiagnosticsPreserveErrorAndExcludePayload(t *testing.T) {
	err := withToolDiagnostic(toolReject("schema_mismatch"), "response.output_item.done", 4, true)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 422 || api.Kind != "invalid_tool_call" || !strings.Contains(api.Message, "reason=schema_mismatch") {
		t.Fatal("diagnostic error lost its API identity or reason")
	}
	const secret = "SENSITIVE_PAYLOAD_MUST_NEVER_APPEAR"
	source := map[string]any{
		"model": secret, "instructions": secret,
		"tools": []any{map[string]any{"type": "function", "name": secret, "description": secret, "parameters": map[string]any{"secret": secret}}},
		"input": []any{
			messageItem("user", secret),
			map[string]any{"type": "function_call_output", "output": secret, "encrypted_content": secret, "call_id": secret},
			map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "custom", "name": secret}}},
			map[string]any{"type": "tool_search_output", "tools": []any{map[string]any{"type": "namespace", "name": secret, "tools": []any{map[string]any{"type": "function", "name": secret}}}}},
		},
	}
	fields, ok := toolDiagnosticFields(err, source)
	if !ok || strings.Contains(string(jsonBytes(fields)), secret) || strings.Contains(err.Error(), secret) {
		t.Fatal("payload leaked into diagnostics")
	}
	if fields["top_function_count"] != 1 || fields["additional_custom_count"] != 1 || fields["search_function_count"] != 1 || fields["tool_already_delivered"] != true {
		t.Fatal("safe structure counts were not retained")
	}
	unsafe := withToolDiagnostic(toolReject(secret), secret, 1000000000, false)
	unknown, _ := toolDiagnosticFields(unsafe, nil)
	if strings.Contains(string(jsonBytes(unknown)), secret) || strings.Contains(unsafe.Error(), secret) || unknown["reason"] != "unspecified" || unknown["stage"] != "unknown" || unknown["output_index"] != -1 {
		t.Fatal("untrusted labels or indices were copied to diagnostics")
	}
}

func TestH23DiagnosticFailureDoesNotReplaceToolError(t *testing.T) {
	for _, panics := range []bool{false, true} {
		service := NewService()
		err := toolReject("tool_not_in_catalog")
		before := err.Error()
		service.SetHost(func(method string, payload any, out any) error {
			if method != "host.log" {
				t.Fatal("unexpected logging callback")
			}
			if panics {
				panic("host logging unavailable")
			}
			return errors.New("logging unavailable")
		})
		service.logToolRejection("callback-context", nil, err)
		if err.Error() != before {
			t.Fatal("logging changed the original request failure")
		}
	}
}

func TestH23CatalogueDiagnosticBudget(t *testing.T) {
	tools := make([]any, 10000)
	for i := range tools {
		tools[i] = map[string]any{"type": "function", "name": "ignored"}
	}
	fields, _ := toolDiagnosticFields(toolReject("schema_mismatch"), map[string]any{"tools": tools})
	if fields["catalogue_count_truncated"] != true || fields["top_function_count"].(int) > 4096 {
		t.Fatal("diagnostic catalogue scan was not bounded")
	}
}

func TestH23MissingTerminalIsNotInvalidToolCall(t *testing.T) {
	err := withToolDiagnostic(streamIncomplete(), "stream_end", -1, false)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 502 || api.Kind != "invalid_upstream_response" || strings.Contains(err.Error(), "invalid_tool_call") {
		t.Fatal("an upstream interruption was misclassified as a bad tool")
	}
	fields, ok := toolDiagnosticFields(err, nil)
	if !ok || fields["event"] != "relay_stream_incomplete" || fields["reason"] != "missing_terminal_event" {
		t.Fatal("upstream interruption lost its safe diagnostic reason")
	}
}

func TestH23ServiceLogsOnceBeforeClosingStream(t *testing.T) {
	for _, bad := range []bool{false, true} {
		service := NewService()
		args := string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{"tool": "get_weather", "args": map[string]any{"city": "Shanghai"}}))}))
		if bad {
			args = `{"code":"PRIVATE_BAD_BODY"}`
		}
		chunks := transportChunks(args)
		var mu sync.Mutex
		var order []string
		var logged map[string]any
		var closeError string
		next := 0
		done := make(chan struct{})
		service.SetHost(func(method string, payload any, out any) error {
			request, _ := payload.(map[string]any)
			switch method {
			case "host.http.do_stream":
				response := out.(*upstreamStream)
				response.StatusCode, response.StreamID = 200, "upstream-diagnostic"
			case "host.http.stream_read":
				response := out.(*streamChunk)
				if next < len(chunks) {
					response.Payload = []byte(chunks[next])
					next++
				} else {
					response.Done = true
				}
			case "host.log":
				mu.Lock()
				order = append(order, "log")
				logged = request
				mu.Unlock()
			case "host.stream.close":
				mu.Lock()
				order = append(order, "close")
				closeError = stringValue(request["error"])
				mu.Unlock()
				close(done)
			}
			return nil
		})
		body := weatherRequest()
		raw, _ := json.Marshal(map[string]any{
			"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body), "StorageJSON": streamTestCredential(),
			"Model": "gpt-6-astra", "Stream": true, "stream_id": "client-diagnostic", "host_callback_id": "request-context-123",
		})
		if _, err := service.Handle("executor.execute_stream", raw); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not close")
		}
		mu.Lock()
		if bad {
			if strings.Join(order, ",") != "log,close" || logged["host_callback_id"] != "request-context-123" || !strings.Contains(closeError, "invalid_tool_call") {
				t.Fatalf("diagnostic context/order lost: %v", order)
			}
			fields := objectValue(logged["fields"])
			if fields["reason"] == "unspecified" || fields["stage"] != "response.output_item.done" || strings.Contains(string(jsonBytes(logged)), "PRIVATE_BAD_BODY") {
				t.Fatal("failure was not safely attributed to its actual stream stage")
			}
			message := stringValue(logged["message"])
			if len(message) > 2048 || !strings.Contains(message, stringValue(fields["diagnostic_ref"])) || !strings.Contains(message, "response.output_item.done") || !strings.Contains(message, stringValue(fields["reason"])) {
				t.Fatal("safe diagnostic would be lost by a formatter that drops fields")
			}
		} else if strings.Join(order, ",") != "close" || logged != nil || closeError != "" {
			t.Fatal("success generated a rejection diagnostic")
		}
		mu.Unlock()
	}
}
