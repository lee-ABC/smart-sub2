package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// h22's original test, restored: with the default relay_tool_contracts
// (summary) the catalogue is h22's natural-language directory again.
func TestPrepareResponsesBodyStripsToolsAndUsesNaturalLanguageCatalog(t *testing.T) {
	cfg := defaultConfig()
	source := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "What is the weather?"}},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "get_weather",
			"description": "Get current weather.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
		"reasoning": map[string]any{"effort": "max"},
	}
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["tools"]; exists {
		t.Fatal("upstream body still contains tools")
	}
	if got := body["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	items, ok := body["input"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("input = %#v, want developer prologue and user history", body["input"])
	}
	first := objectValue(items[0])
	if first["role"] != "developer" {
		t.Fatalf("first input role = %v, want developer", first["role"])
	}
	content, _ := first["content"].([]any)
	catalog := itemText(objectValue(content[0])["text"])
	if !strings.Contains(catalog, "get_weather") || !strings.Contains(catalog, "city (required)") {
		t.Fatalf("catalog omitted natural language tool directory: %s", catalog)
	}
	if strings.Contains(catalog, `"properties"`) {
		t.Fatalf("catalog unexpectedly included raw schema: %s", catalog)
	}
	metadata := objectValue(body["metadata"])
	if stringValue(metadata["turn_id"]) == "" || stringValue(metadata["task_id"]) == "" || metadata["agent_iteration"] != "1" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

// rc2's version of the test above. Complete contracts are now opt-in
// (relay_tool_contracts: full), so this test switches them on.
func TestPrepareResponsesBodyStripsTopLevelToolsAndIncludesFullContract(t *testing.T) {
	cfg := defaultConfig()
	cfg.RelayToolContracts = relayToolContractsFull
	source := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "What is the weather?"}},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "get_weather",
			"description": "Get current weather.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
		"reasoning": map[string]any{"effort": "max"},
	}
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["tools"]; exists {
		t.Fatal("upstream body still contains tools")
	}
	// "max" is above the Basis Points ceiling, so it clamps to the deepest
	// setting the upstream offers instead of falling back to the default.
	if got := body["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	items, ok := body["input"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("input = %#v, want developer prologue and user history", body["input"])
	}
	first := objectValue(items[0])
	if first["role"] != "developer" {
		t.Fatalf("first input role = %v, want developer", first["role"])
	}
	content, _ := first["content"].([]any)
	catalog := itemText(objectValue(content[0])["text"])
	if !strings.Contains(catalog, "get_weather") || !strings.Contains(catalog, `"required":["city"]`) {
		t.Fatalf("catalog omitted complete client tool contract: %s", catalog)
	}
	if !strings.Contains(catalog, `"properties"`) {
		t.Fatalf("catalog omitted raw schema: %s", catalog)
	}
	metadata := objectValue(body["metadata"])
	if stringValue(metadata["turn_id"]) == "" || stringValue(metadata["task_id"]) == "" || metadata["agent_iteration"] != "1" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func TestTransportCodeUsesToolAndArgsAndPreservesNativeItem(t *testing.T) {
	source := map[string]any{
		// The input is here so the source has a conversation to be scoped by.
		// Without one conversationScope returns no scope at all, every cache
		// below it is skipped, and the replayed call comes back rebuilt rather
		// than preserved -- which this test used to pass through only because
		// an unscopeable request fell into a literal shared "anonymous"
		// namespace. prepareRequest refuses a request with no answerable input,
		// so a source without one never reaches this code in production.
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "weather in Tokyo?"}},
		}},
		"tools": []any{map[string]any{
			"type": "function",
			"name": "get_weather",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
	}
	native := map[string]any{
		"type":       "function_call",
		"id":         "fc_native_weather",
		"call_id":    "call_native_weather",
		"name":       "run_officejs",
		"status":     "completed",
		"summary":    "Get current weather for Tokyo",
		"references": []any{"Tokyo weather"},
		"arguments": string(jsonBytes(map[string]any{
			"summary": "Get current weather for Tokyo",
			"code":    string(jsonBytes(map[string]any{"tool": "get_weather", "args": map[string]any{"city": "Tokyo"}})),
		})),
	}
	response := map[string]any{"output": []any{native}}
	call, ok := extractNativeClientToolCall(response, source)
	if !ok {
		t.Fatal("transport call was not decoded")
	}
	if call["name"] != "get_weather" || call["call_id"] != "call_native_weather" {
		t.Fatalf("decoded call = %#v", call)
	}
	if got := stringValue(call["arguments"]); got != `{"city":"Tokyo"}` {
		t.Fatalf("arguments = %s", got)
	}
	translated, _, _, err := transformResponseBody(jsonBytes(response), source)
	if err != nil {
		t.Fatal(err)
	}
	var clientResponse map[string]any
	_ = json.Unmarshal(translated, &clientResponse)
	clientCall := objectValue(clientResponse["output"].([]any)[0])
	if clientCall["name"] != "get_weather" {
		t.Fatalf("client call = %#v", clientCall)
	}
	items := translateInputItems([]any{
		clientCall,
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_native_weather",
			"output":  "18°C",
		},
	}, clientToolSpecs(source), conversationScope(source))
	if !reflect.DeepEqual(items[0], native) {
		t.Fatalf("replayed native item = %#v, want %#v", items[0], native)
	}
	output := objectValue(items[1])
	if output["type"] != "function_call_output" || output["id"] != "fc_call_native_weather" || output["output"] != "18°C" {
		t.Fatalf("normalized output = %#v", output)
	}
}

func TestTurnIDStaysStableWhileAgentIterationAdvances(t *testing.T) {
	base := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "Inspect the workbook"}},
	}
	cfg := defaultConfig()
	first, err := prepareResponsesBody(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nextSource := cloneObject(base)
	nextSource["input"] = append(nextSource["input"].([]any),
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "run_officejs", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
	)
	second, err := prepareResponsesBody(nextSource, cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata := objectValue(first["metadata"])
	secondMetadata := objectValue(second["metadata"])
	if firstMetadata["turn_id"] != secondMetadata["turn_id"] {
		t.Fatalf("turn_id changed: %v -> %v", firstMetadata["turn_id"], secondMetadata["turn_id"])
	}
	if secondMetadata["agent_iteration"] != "2" {
		t.Fatalf("agent_iteration = %v, want 2", secondMetadata["agent_iteration"])
	}
}

func TestParseCredentialPrefersJWTAuthAccountID(t *testing.T) {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-from-jwt"},
	}) + "."
	credential, err := parseCredential([]byte(`{"access_token":"` + token + `","account_id":"acct-from-file"}`))
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountID != "acct-from-jwt" {
		t.Fatalf("account ID = %s, want acct-from-jwt", credential.AccountID)
	}
}

func basisPointsCredentialJSON(accountID string) []byte {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	}) + "."
	return jsonBytes(map[string]any{
		"type":          Provider,
		"access_token":  token,
		"refresh_token": "rt-test",
		"account_id":    "file-account-id",
	})
}

// A native Codex credential must be left entirely to CPA. Claiming it made the
// host skip its own parsing, and the record this plugin returned in its place
// carried no access_token/refresh_token, so every existing Codex model started
// failing with 401 as soon as the plugin loaded.
func TestAuthParseLeavesNativeCodexCredentialsToTheHost(t *testing.T) {
	for _, provider := range []string{"codex", "openai", ""} {
		request, _ := json.Marshal(authParseRequest{
			Provider: provider,
			FileName: "codex-account.json",
			RawJSON:  basisPointsCredentialJSON("acct-auth-parse"),
		})
		response, err := NewService().authParse(request)
		if err != nil {
			t.Fatalf("provider %q: %v", provider, err)
		}
		if response["Handled"] != false {
			t.Fatalf("provider %q: response = %#v, want unhandled", provider, response)
		}
	}
}

// Exactly one auth must come back: the host stamps plugin_virtual on every
// record only when a file expands into more than one, and that flag disables
// persistence and token renewal.
func TestAuthParseReturnsSingleAuthForOwnCredential(t *testing.T) {
	request, _ := json.Marshal(authParseRequest{
		Provider: Provider,
		FileName: "basispoints-account.json",
		RawJSON:  basisPointsCredentialJSON("acct-auth-parse"),
	})
	response, err := NewService().authParse(request)
	if err != nil {
		t.Fatal(err)
	}
	if response["Handled"] != true {
		t.Fatalf("response = %#v, want handled", response)
	}
	if _, expanded := response["Auths"]; expanded {
		t.Fatalf("response = %#v, want a single Auth and no Auths expansion", response)
	}
	auth := objectValue(response["Auth"])
	if auth["Provider"] != Provider {
		t.Fatalf("auth = %#v, want provider %q", auth, Provider)
	}
	metadata := objectValue(auth["Metadata"])
	if metadata["account_id"] != "acct-auth-parse" {
		t.Fatalf("metadata = %#v, want JWT account ID", metadata)
	}
	// Without a refresh interval CPA never schedules this credential, because
	// its provider is not in the host's built-in refresh lead registry.
	if interval, _ := metadata["refresh_interval_seconds"].(int); interval <= 0 {
		t.Fatalf("metadata = %#v, want a positive refresh_interval_seconds", metadata)
	}
}

// Identifier() must equal the Provider on the records this plugin emits, or the
// host cannot resolve the plugin for auth.refresh and the credential silently
// stops renewing.
func TestAuthIdentifierMatchesEmittedProvider(t *testing.T) {
	if AuthProviderID != Provider {
		t.Fatalf("AuthProviderID = %q, want %q", AuthProviderID, Provider)
	}
}

func TestServiceUsesCodexAuthParserAndBasisPointsExecutorIdentifiers(t *testing.T) {
	service := NewService()
	authIdentifier, err := service.Handle("auth.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	if objectValue(authIdentifier)["identifier"] != AuthProviderID {
		t.Fatalf("auth identifier = %#v, want %q", authIdentifier, AuthProviderID)
	}
	executorIdentifier, err := service.Handle("executor.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	if objectValue(executorIdentifier)["identifier"] != Provider {
		t.Fatalf("executor identifier = %#v, want %q", executorIdentifier, Provider)
	}
}

func TestExecuteBuildsAuthenticatedBasisPointsBody(t *testing.T) {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-exec"},
	}) + "."
	requestBody := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{"type": "function", "name": "demo", "parameters": map[string]any{"type": "object"}}},
	}
	rawRequest := jsonBytes(requestBody)
	storage := jsonBytes(map[string]any{"type": "codex", "access_token": token, "account_id": "wrong-file-id"})
	service := NewService()
	var seenHeaders map[string][]string
	var seenBody map[string]any
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return nil
		}
		request := objectValue(payload.(map[string]any))
		if headers, ok := request["headers"].(http.Header); ok {
			seenHeaders = map[string][]string(headers)
		}
		var body map[string]any
		if err := json.Unmarshal(request["body"].([]byte), &body); err != nil {
			return err
		}
		seenBody = body
		response := out.(*upstreamResponse)
		response.StatusCode = 200
		response.Headers = http.Header{"Content-Type": {"application/json"}}
		response.Body = jsonBytes(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
		})
		return nil
	})

	raw, _ := json.Marshal(map[string]any{"OriginalRequest": rawRequest, "Payload": rawRequest, "StorageJSON": storage, "Model": "gpt-6-astra"})
	result, err := service.Handle("executor.execute", raw)
	if err != nil {
		t.Fatal(err)
	}
	if seenHeaders["Authorization"] == nil || !strings.HasPrefix(seenHeaders["Authorization"][0], "Bearer ") {
		t.Fatalf("authorization header missing: %#v", seenHeaders)
	}
	if seenHeaders["ChatGPT-Account-ID"][0] != "acct-exec" || seenHeaders["X-OpenAI-Account-ID"][0] != "acct-exec" {
		t.Fatalf("account headers = %#v", seenHeaders)
	}
	// X-Basispoints-Auth-Mode is deliberately no longer sent: it is not part of
	// the X-OpenAI-Internal-Basispoints-* family the real client uses, and a
	// probe against the live endpoint completed without it.
	if _, sent := seenHeaders["X-Basispoints-Auth-Mode"]; sent {
		t.Fatalf("auth mode header is back: %#v", seenHeaders)
	}
	if _, exists := seenBody["tools"]; exists {
		t.Fatalf("upstream body still has tools: %#v", seenBody)
	}
	if result == nil {
		t.Fatal("executor result is nil")
	}
}

// A quota lookup must never fail in a way the host can read as an auth problem.
// CPA marks a credential terminally unavailable when a plugin error carries 401
// (hasUnauthorizedAuthFailure), which would take a perfectly usable credential
// out of service just because its usage endpoint was unreachable.
func TestQuotaFetchNeverReportsAuthFailure(t *testing.T) {
	service := NewService()
	// No host callback is wired, so the usage lookup cannot succeed.
	for name, payload := range map[string]string{
		"missing credential": `{"auth_id":"bp-x"}`,
		"broken credential":  `{"auth_id":"bp-x","storage_json":"eyJub3BlIjp0cnVlfQ=="}`,
	} {
		result, err := service.Handle("quota.fetch", json.RawMessage(payload))
		if err != nil {
			t.Fatalf("%s: quota.fetch returned an error (%v); it must degrade to a status payload", name, err)
		}
		object := objectValue(result)
		if object == nil {
			t.Fatalf("%s: quota.fetch returned %#v, want an object", name, result)
		}
		if _, hasSummary := object["summary"]; !hasSummary {
			t.Fatalf("%s: quota.fetch returned %#v, want a summary", name, object)
		}
	}
}

// Each exposed model must reach its own upstream model. A single global
// upstream_model made every alias collapse onto one model, so asking for
// gpt-5.6-terra silently produced gpt-6-astra.
func TestEachExposedModelKeepsItsOwnUpstream(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if !isResponseModel(model, cfg) {
			t.Fatalf("%s is not exposed by default; got %v", model, cfg.Models)
		}
		body, err := prepareResponsesBody(map[string]any{"model": model, "input": "hi"}, cfg)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if got := stringValue(body["model"]); got != model {
			t.Fatalf("%s went upstream as %q, want it unchanged", model, got)
		}
	}
	// An explicit alias still resolves through upstream_model.
	body, err := prepareResponsesBody(map[string]any{"model": "gpt-6-astra-basispoints", "input": "hi"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := stringValue(body["model"]); got != cfg.UpstreamModel {
		t.Fatalf("alias resolved to %q, want %q", got, cfg.UpstreamModel)
	}
}

// Asking for more reasoning than Basis Points offers must land on its ceiling,
// not on the default. 0.1.1 turned max/ultra into medium, so a caller asking
// for the deepest reasoning silently got the middle setting.
func TestEffortAboveCeilingClampsToXHigh(t *testing.T) {
	for _, requested := range []string{"max", "ultra", "maximum", "highest"} {
		if got := normalizeEffort(requested); got != "xhigh" {
			t.Fatalf("effort %q normalized to %q, want xhigh", requested, got)
		}
	}
	for _, requested := range []string{"low", "medium", "high", "xhigh"} {
		if got := normalizeEffort(requested); got != requested {
			t.Fatalf("effort %q normalized to %q, want it unchanged", requested, got)
		}
	}
	if got := normalizeEffort("x-high"); got != "xhigh" {
		t.Fatalf("alias x-high normalized to %q, want xhigh", got)
	}
	// No ordering to clamp against, so the default still applies.
	for _, requested := range []string{"", "banana"} {
		if got := normalizeEffort(requested); got != "medium" {
			t.Fatalf("effort %q normalized to %q, want medium", requested, got)
		}
	}
}

// The floor is the mirror of the ceiling above. Basis Points rejects anything
// below "low", and a caller that asks for less thinking must not be quietly
// given more: Codex exposes a "minimal" setting, and mapping it to the medium
// default made those requests slower and more expensive than requested.
func TestEffortBelowFloorClampsToLow(t *testing.T) {
	for _, requested := range []string{"minimal", "min", "minimum", "lowest", "none", "off"} {
		if got := normalizeEffort(requested); got != "low" {
			t.Fatalf("effort %q normalized to %q, want low", requested, got)
		}
	}
	for _, requested := range []string{"MINIMAL", "  Minimal  "} {
		if got := normalizeEffort(requested); got != "low" {
			t.Fatalf("effort %q normalized to %q, want low", requested, got)
		}
	}
	// Still never below what the upstream accepts.
	for _, requested := range []string{"minimal", "none", "max"} {
		if _, ok := supportedReasoningEfforts[normalizeEffort(requested)]; !ok {
			t.Fatalf("effort %q normalized outside the upstream's accepted set", requested)
		}
	}
}

// CPA decides whether a failure belongs to the request or the credential by
// parsing the plugin's error message as JSON and reading error.type. Flattening
// the upstream envelope to its message string hid that signal, so a request the
// caller malformed was charged to the credential and disabled it.
func TestUpstreamErrorKeepsJSONEnvelopeForClassification(t *testing.T) {
	body := []byte(`{"error":{"message":"Invalid URL (POST /responses)","type":"invalid_request_error"}}`)
	err := statusError(400, body)
	var object map[string]any
	if jsonErr := json.Unmarshal([]byte(err.Error()), &object); jsonErr != nil {
		t.Fatalf("error message is not JSON (%v): %s", jsonErr, err.Error())
	}
	nested, _ := object["error"].(map[string]any)
	if nested == nil || nested["type"] != "invalid_request_error" {
		t.Fatalf("error.type was lost: %s", err.Error())
	}
	// A non-JSON upstream body still degrades to a readable message.
	if got := statusError(500, []byte("boom")).Error(); got != "boom" {
		t.Fatalf("plain body = %q, want %q", got, "boom")
	}
}

// Inline images must become URLs the upstream can fetch, and the relay must not
// outlive its purpose: Basis Points rejects data URLs outright, and the route
// serving these bytes is unauthenticated.
func TestInlineImageRelay(t *testing.T) {
	// 1x1 PNG.
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	input := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "what is this"},
			map[string]any{"type": "input_image", "image_url": png},
		},
	}}
	result := rewriteInlineImages(input, "https://example.test/bp")
	rewritten, unsupported := result.Rewritten, result.Unusable
	if rewritten != 1 || unsupported != 0 {
		t.Fatalf("rewritten=%d unsupported=%d, want 1/0", rewritten, unsupported)
	}
	part := objectValue(objectValue(input[0])["content"].([]any)[1])
	url := stringValue(part["image_url"])
	if !strings.HasPrefix(url, "https://example.test/bp/"+imageRelayPath+"?") {
		t.Fatalf("image_url = %q, want a relay URL", url)
	}
	token := url[strings.Index(url, "=")+1:]
	if len(token) != 64 {
		t.Fatalf("token %q is %d chars, want 64 hex chars of entropy", token, len(token))
	}
	if _, mediaType, ok := takeImage(token); !ok || mediaType != "image/png" {
		t.Fatalf("relayed image not retrievable as image/png (ok=%v type=%q)", ok, mediaType)
	}
	// Bounded reuse: the entry retires rather than serving forever.
	for i := 1; i < imageRelayMaxFetches; i++ {
		takeImage(token)
	}
	if _, _, ok := takeImage(token); ok {
		t.Fatal("relay entry still served after its fetch budget was spent")
	}
	// Without somewhere to publish, an inline image is reported, never dropped.
	input2 := []any{map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_image", "image_url": png}}}}
	if unsupported := rewriteInlineImages(input2, "").Unusable; unsupported != 1 {
		t.Fatalf("unsupported=%d with no public base URL, want 1", unsupported)
	}
	// A plain URL is already fetchable and must be left alone.
	input3 := []any{map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_image", "image_url": "https://cdn.test/a.png"}}}}
	if r := rewriteInlineImages(input3, "https://example.test/bp"); r.Rewritten != 0 || r.Unusable != 0 {
		rewritten, unsupported := r.Rewritten, r.Unusable
		t.Fatalf("plain URL rewritten=%d unsupported=%d, want 0/0", rewritten, unsupported)
	}
}

// Codex returns screenshots inside a tool result's output array, not in
// item.content. A rewriter that only walked content passed those through
// untouched and the upstream rejected the whole request with 422.
func TestInlineImageRelayFindsImagesInToolOutput(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	input := []any{
		map[string]any{
			"type":    "custom_tool_call_output",
			"call_id": "call_1",
			"output": []any{
				map[string]any{"type": "output_text", "text": "screenshot taken"},
				map[string]any{"type": "input_image", "image_url": png},
			},
		},
		map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_image", "image_url": png}},
		},
	}
	result := rewriteInlineImages(input, "https://example.test/bp")
	rewritten, unsupported := result.Rewritten, result.Unusable
	if rewritten != 2 || unsupported != 0 {
		t.Fatalf("rewritten=%d unsupported=%d, want 2/0 (tool output and message content)", rewritten, unsupported)
	}
	toolPart := objectValue(objectValue(input[0])["output"].([]any)[1])
	if url := stringValue(toolPart["image_url"]); !strings.HasPrefix(url, "https://example.test/bp/") {
		t.Fatalf("tool-output image was not relayed: %q", url)
	}
}
