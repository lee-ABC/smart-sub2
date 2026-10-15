package basispoints

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestReplayProductionNamespaceFailures replays the downstream request bodies
// of the turns the upstream refused with "Missing namespace for function_call"
// and checks that every replayed call of a tool_search-loaded tool now reaches
// the upstream with its namespace. The bodies are production traffic, so they
// are never committed: point REPLAY_DIR at them to run it.
func TestReplayProductionNamespaceFailures(t *testing.T) {
	dir := os.Getenv("REPLAY_DIR")
	if dir == "" {
		t.Skip("REPLAY_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) == 0 {
		t.Fatalf("no request bodies in %s", dir)
	}
	sort.Strings(files)
	totalSearched, totalMissing, failingFiles := 0, 0, 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var source map[string]any
		if err := json.Unmarshal(raw, &source); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		searched := map[string]bool{}
		input, _ := source["input"].([]any)
		for _, value := range input {
			if item := objectValue(value); item != nil && stringValue(item["type"]) == "tool_search_output" {
				groups, _ := item["tools"].([]any)
				for _, rawGroup := range groups {
					if group := objectValue(rawGroup); group != nil && stringValue(group["type"]) == "namespace" {
						members, _ := group["tools"].([]any)
						for _, rawTool := range members {
							if tool := objectValue(rawTool); tool != nil {
								searched[stringValue(tool["name"])] = true
							}
						}
					}
				}
			}
		}
		body, err := prepareResponsesBody(source, defaultConfig())
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		upstream, _ := body["input"].([]any)
		missing := 0
		for _, value := range upstream {
			item := objectValue(value)
			if item == nil || stringValue(item["type"]) != "function_call" || !searched[stringValue(item["name"])] {
				continue
			}
			totalSearched++
			if stringValue(item["namespace"]) == "" {
				missing++
			}
		}
		totalMissing += missing
		if missing > 0 {
			failingFiles++
		}
	}
	t.Logf("%d request bodies, %d replayed calls of tool_search-loaded tools, %d still without namespace (in %d bodies)",
		len(files), totalSearched, totalMissing, failingFiles)
	if totalMissing > 0 {
		t.Errorf("%d replayed calls in %d of %d bodies would still be refused upstream", totalMissing, failingFiles, len(files))
	}
}
