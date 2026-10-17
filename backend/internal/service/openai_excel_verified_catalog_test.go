//go:build unit

package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestExcelVerifiedCanonicalModelCatalog(t *testing.T) {
	// Live BPS probes on 2026-10-18 returned completed with the same model ID.
	verified := []string{"gpt-6-sol", "gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
	require.ElementsMatch(t, verified, ExcelModelIDs())
	seen := map[string]bool{}
	for _, model := range ExcelModelIDs() {
		require.False(t, seen[model], "duplicate catalog entry: %s", model)
		seen[model] = true
		require.True(t, IsExcelSupportedModel(model), "catalog model must route through BPS: %s", model)
	}
	// Upstream rejected the exact gpt-5.5 name with basispoints_model_access_changed.
	// Never make it appear supported by falling back to a different upstream model.
	require.False(t, IsExcelSupportedModel("gpt-5.5"))
	require.False(t, IsExcelSupportedModel("not-a-model"))
}
