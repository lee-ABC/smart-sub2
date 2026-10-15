package basispoints

import (
	"strings"
	"testing"
)

func liteSource(extra map[string]any) map[string]any {
	source := map[string]any{
		"input": []any{
			map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
				map[string]any{"type": "namespace", "name": "functions", "tools": []any{
					map[string]any{"type": "custom", "name": "exec"},
					map[string]any{"type": "function", "name": "wait"},
				}},
			}},
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
	}
	for k, v := range extra {
		source[k] = v
	}
	return source
}

func TestHotfixLiteToolsAreNotToldToAnswerAsText(t *testing.T) {
	got := clientToolProtocolInstructions(liteSource(nil))
	if strings.Contains(got, "Return the answer as assistant text") {
		t.Fatalf("lite request still told to answer as text: %q", got)
	}
	if !strings.Contains(got, "additional_tools") {
		t.Fatalf("lite request not pointed at its additional_tools: %q", got)
	}
}

func TestHotfixNoToolsStillAnswersAsText(t *testing.T) {
	source := map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}}}
	if got := clientToolProtocolInstructions(source); !strings.Contains(got, "Return the answer as assistant text") {
		t.Fatalf("tool-less request lost the text-only instruction: %q", got)
	}
}

func TestHotfixToolChoiceNoneStillAnswersAsText(t *testing.T) {
	got := clientToolProtocolInstructions(liteSource(map[string]any{"tool_choice": "none"}))
	if !strings.Contains(got, "Return the answer as assistant text") {
		t.Fatalf("tool_choice=none lite request should stay text-only: %q", got)
	}
}

func TestHotfixEmptyAdditionalToolsStillAnswersAsText(t *testing.T) {
	source := map[string]any{"input": []any{map[string]any{"type": "additional_tools", "tools": []any{}}}}
	if got := clientToolProtocolInstructions(source); !strings.Contains(got, "Return the answer as assistant text") {
		t.Fatalf("empty additional_tools should stay text-only: %q", got)
	}
}
