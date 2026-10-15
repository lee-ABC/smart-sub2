package basispoints

import (
	"encoding/json"
	"testing"
)

func compactSource(extra map[string]any) map[string]any {
	source := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}
	for k, v := range extra {
		source[k] = v
	}
	return source
}

func injectedThreshold(t *testing.T, body map[string]any) any {
	t.Helper()
	items, ok := body["context_management"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("context_management = %#v", body["context_management"])
	}
	return items[0].(map[string]any)["compact_threshold"]
}

func TestCompactThresholdDefaultsAboveCodexOwnCompaction(t *testing.T) {
	body, err := prepareResponsesBody(compactSource(nil), defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := injectedThreshold(t, body); got != DefaultCompactThreshold || DefaultCompactThreshold != 700000 {
		t.Fatalf("default threshold = %v, want 700000 (above Codex's ~245k)", got)
	}
}

func TestClientContextManagementIsRelayedUnchanged(t *testing.T) {
	own := []any{map[string]any{"type": "compaction", "compact_threshold": 123456}}
	body, err := prepareResponsesBody(compactSource(map[string]any{"context_management": own}), defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := injectedThreshold(t, body); got != 123456 {
		t.Fatalf("client threshold replaced: %v", got)
	}
}

func TestCompactThresholdFollowsConfigYAML(t *testing.T) {
	t.Chdir(t.TempDir())
	service := NewService()
	reconfigure := func(yaml string) error {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
		_, err := service.Handle("plugin.reconfigure", raw)
		return err
	}
	if err := reconfigure("compact_threshold: 300000\n"); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if got := service.config().CompactThreshold; got != 300000 {
		t.Fatalf("configured threshold = %d", got)
	}
	body, err := prepareResponsesBody(compactSource(nil), service.config())
	if err != nil {
		t.Fatal(err)
	}
	if got := injectedThreshold(t, body); got != 300000 {
		t.Fatalf("injected threshold = %v", got)
	}
	// Removing the key from config.yaml must restore the default, not the
	// value cached in settings.json.
	if err := reconfigure("timeout_seconds: 300\n"); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if got := service.config().CompactThreshold; got != DefaultCompactThreshold {
		t.Fatalf("removed threshold kept %d", got)
	}
	if err := reconfigure("compact_threshold: 5000\n"); err == nil {
		t.Fatal("out-of-range threshold accepted")
	}
}
