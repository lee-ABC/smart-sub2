package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestExcelCatalogPreservesNative(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("SUB2_EXCEL_CONFIG_FILE", p)
	require.NoError(t, os.WriteFile(p, []byte(`{"group_modes":{"9":"excel"}}`), 0600))
	g := &service.Group{ID: 9, Platform: service.PlatformOpenAI}
	native := `{"models":[{"slug":"gpt-5.5","context_window":1048576,"custom":true}],"extra":"keep"}`
	orig := &service.OpenAIModelsResponse{Body: []byte(native)}
	got, err := mergeExcelModelsResponse(g, orig, "")
	require.NoError(t, err)
	for _, text := range []string{"gpt-5.5", "gpt-6-sol", "1048576", "custom", "keep"} {
		require.Contains(t, string(got.Body), text)
	}
	require.Equal(t, native, string(orig.Body))
	again, err := mergeExcelModelsResponse(g, orig, got.ETag)
	require.NoError(t, err)
	require.True(t, again.NotModified)
	g.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-5.5"}}
	limited, err := mergeExcelModelsResponse(g, orig, "")
	require.NoError(t, err)
	require.NotContains(t, string(limited.Body), "gpt-6-sol")
	require.Contains(t, appendExcelGroupModels(g, []string{"gpt-5.5"}), "gpt-5.5")
}
