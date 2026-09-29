package handler

import (
	"errors"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *GatewayHandler) pinnedOpenAIModels(c *gin.Context, group *service.Group) {
	if c.Request.Context().Err() != nil {
		return
	}
	if h.openAIGatewayService == nil {
		writeOpenAIModelsError(c, http.StatusInternalServerError, "api_error", "OpenAI model discovery is not configured")
		return
	}
	ifNoneMatch := c.GetHeader("If-None-Match")
	if excelGroupEnabled(group) {
		ifNoneMatch = ""
	}
	response, account, err := h.openAIGatewayService.FetchPinnedOpenAIModelsList(
		c.Request.Context(), group, h.maxAccountSwitches, ifNoneMatch,
	)
	if c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		if errors.Is(err, service.ErrNoPinnedCodexModelsAccounts) {
			writeOpenAIModelsError(c, http.StatusServiceUnavailable, "upstream_error", "No available OpenAI model discovery accounts")
			return
		}
		writeOpenAIModelsError(c, infraerrors.Code(err), "upstream_error", infraerrors.Message(err))
		return
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	writeOpenAIModelsResponse(c, response)
}

func writeOpenAIModelsError(c *gin.Context, status int, errorType, message string) {
	c.JSON(status, gin.H{"error": gin.H{"type": errorType, "message": message}})
}

func writeOpenAIModelsResponse(c *gin.Context, manifest *service.OpenAIModelsResponse) {
	if key, ok := middleware2.GetAPIKeyFromContext(c); ok && key != nil {
		merged, err := mergeExcelModelsResponse(key.Group, manifest, c.GetHeader("If-None-Match"))
		if err != nil {
			writeOpenAIModelsError(c, 500, "api_error", "Cannot merge model catalog")
			return
		}
		manifest = merged
	}
	if manifest.ETag != "" {
		c.Header("ETag", manifest.ETag)
	}
	if manifest.NotModified {
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	c.Data(http.StatusOK, "application/json", manifest.Body)
}
