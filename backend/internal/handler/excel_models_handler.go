package handler

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func excelGroupEnabled(group *service.Group) bool {
	if group == nil || group.Platform != service.PlatformOpenAI {
		return false
	}
	cfg, err := service.ReadExcelRoutingConfig()
	return err == nil && cfg.Mode(group.ID) == "excel"
}

func appendExcelGroupModels(group *service.Group, models []string) []string {
	if !excelGroupEnabled(group) {
		return models
	}
	result := append([]string(nil), models...)
	seen := map[string]bool{}
	for _, id := range result {
		seen[id] = true
	}
	for _, id := range service.ExcelModelIDs() {
		if !seen[id] && (!group.ModelAllowlistEnabled() || group.ModelAllowlist.Allows(id)) {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result
}

// Preserve native metadata verbatim; only append missing Excel entries.
func mergeExcelModelsResponse(group *service.Group, original *service.OpenAIModelsResponse, etag string) (*service.OpenAIModelsResponse, error) {
	if !excelGroupEnabled(group) {
		return original, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(original.Body, &envelope); err != nil {
		return nil, err
	}
	field, idField := "data", "id"
	if _, ok := envelope["models"]; ok {
		field, idField = "models", "slug"
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(envelope[field], &entries); err != nil {
		return nil, err
	}
	ids := appendExcelGroupModels(group, nil)
	generated, err := service.BuildCodexModelsManifest(ids)
	if err != nil {
		return nil, err
	}
	var extra struct {
		Models []json.RawMessage `json:"models"`
	}
	if err = json.Unmarshal(generated, &extra); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, raw := range entries {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil {
			var id string
			_ = json.Unmarshal(item[idField], &id)
			seen[id] = true
		}
	}
	for i, id := range ids {
		if seen[id] {
			continue
		}
		raw := extra.Models[i]
		if field == "data" {
			raw, err = json.Marshal(map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "openai-excel"})
			if err != nil {
				return nil, err
			}
		}
		entries = append(entries, raw)
	}
	envelope[field], err = json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	result := *original
	result.Body = body
	result.ETag = service.CodexModelsManifestETag(body)
	result.NotModified = service.CodexModelsManifestETagMatches(etag, result.ETag)
	return &result, nil
}
