package basispoints

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestH24ShadowObservationIsOptInAndCannotBeCallerInjected(t *testing.T) {
	s := NewService()
	called := false
	s.SetHost(func(string, any, any) error { called = true; return nil })
	source := weatherRequest()
	source[schemaObserverKey] = map[string]any{"observe": "injected"}
	s.attachSchemaObservation(source, "callback")
	for _, spec := range clientToolSpecs(source) {
		observeSelectedToolSchema(source, spec, map[string]any{"city": "Paris"})
	}
	if called || source[schemaObserverKey] != nil || s.config().SchemaShadow {
		t.Fatal("shadow must stay off by default")
	}
}

func TestH24ShadowObservationNeverChangesCompatibilityOrLogsPayload(t *testing.T) {
	s := NewService()
	s.cfg.SchemaShadow = true
	var logs []string
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.log" {
			logs = append(logs, string(jsonBytes(payload)))
		}
		return errors.New("logger unavailable")
	})
	source := weatherRequest()
	s.attachSchemaObservation(source, "safe_callback_id")
	spec := toolSpec{Key: "SECRET_TOOL_NAME", Type: "function", Spec: map[string]any{
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"city": map[string]any{"type": "string", "maxLength": 1},
		}},
	}}
	value := map[string]any{"city": "SECRET_ARGUMENT_VALUE"}
	before := checkSchemaCompatibility(value, spec.Spec["parameters"])
	for n := 0; n < 12; n++ {
		observeSelectedToolSchema(source, spec, value)
	}
	if after := checkSchemaCompatibility(value, spec.Spec["parameters"]); before != after || after == schemaMismatch {
		t.Fatal("shadow modified the compatibility decision")
	}
	if len(logs) != 8 {
		t.Fatalf("observation cap: got %d", len(logs))
	}
	for _, log := range logs {
		if strings.Contains(log, "SECRET_") || strings.Contains(log, "maxLength") || strings.Contains(log, "city") {
			t.Fatalf("payload leaked: %s", log)
		}
		if !strings.Contains(log, `"enforced":false`) || !strings.Contains(log, `"shadow_result":"mismatch"`) {
			t.Fatalf("wrong observation: %s", log)
		}
	}
	if len(jsonBytes(source)) == 0 {
		t.Fatal("source snapshot must remain serializable")
	}
}

func TestH24ShadowObserverPanicAndConcurrentBudget(t *testing.T) {
	source := map[string]any{schemaObserverKey: &selectedSchemaObserver{remaining: 1,
		observe: func(toolSpec, any) { panic("host failure") }}}
	var workers sync.WaitGroup
	for n := 0; n < 16; n++ {
		workers.Add(1)
		go func() { defer workers.Done(); observeSelectedToolSchema(source, toolSpec{}, nil) }()
	}
	workers.Wait()
	if source[schemaObserverKey].(*selectedSchemaObserver).remaining != 0 {
		t.Fatal("budget not enforced")
	}
}

func TestH24RelayConfigurationModes(t *testing.T) {
	for _, mode := range []string{"", "auto", "legacy", "references", " REFERENCES "} {
		cfg := defaultConfig()
		cfg.RelayProtocol = mode
		if err := cfg.normalize(); err != nil {
			t.Fatal(err)
		}
	}
	// H25-SPEC I4 reverses rc2 here: an invalid value must not fail
	// configuration (a failed register drops the provider from CPA's
	// snapshot). It falls back to the default, legacy, with a warning.
	cfg := defaultConfig()
	cfg.RelayProtocol = "typo"
	if warnings := cfg.normalizeRelaySwitches(); len(warnings) != 1 || cfg.RelayProtocol != relayProtocolLegacy {
		t.Fatalf("invalid protocol mode: warnings=%v value=%q", warnings, cfg.RelayProtocol)
	}
	cfg.RelayProtocol = "typo"
	if err := cfg.normalize(); err != nil || cfg.RelayProtocol != relayProtocolLegacy {
		t.Fatalf("invalid protocol mode failed configuration: %v %q", err, cfg.RelayProtocol)
	}
}

func TestH24PreparedRequestPinsDecoderProtocolAcrossReconfiguration(t *testing.T) {
	s := NewService()
	// references for new conversations is opt-in since h25 (default legacy).
	s.cfg.RelayProtocol = relayProtocolAuto
	source := weatherRequest()
	source["input"] = "h24 protocol pinning synthetic new conversation"
	request := ExecutorRequest{OriginalRequest: jsonBytes(source), StorageJSON: fakeCredential(t)}
	prepared, _, selected, err := s.prepareRequestWithRelayProtocol(request)
	if err != nil || selected != relayProtocolReferences {
		t.Fatalf("new request selection: %q %v", selected, err)
	}
	request.selectedRelayProtocol = selected
	s.cfg.RelayProtocol = relayProtocolLegacy
	decodeSource := withRelayProtocolSelection(source, request.selectedRelayProtocol)
	if relayProtocolForDecode(decodeSource) != relayProtocolReferences {
		t.Fatal("response decoder changed protocol after request preparation")
	}
	if strings.Contains(string(jsonBytes(prepared)), relayProtocolSelectionKey) || strings.Contains(string(jsonBytes(prepared)), schemaObserverKey) {
		t.Fatal("private selection/observation state leaked upstream")
	}
	raw, err := json.Marshal(request)
	if err != nil || strings.Contains(string(raw), "selectedRelayProtocol") {
		t.Fatal("internal selection leaked to host ABI")
	}
	var decoded ExecutorRequest
	if json.Unmarshal(raw, &decoded) != nil || decoded.selectedRelayProtocol != "" {
		t.Fatal("unexported selection was serialized")
	}
}
