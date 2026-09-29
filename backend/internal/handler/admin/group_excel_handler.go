package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"strconv"
)

func (h *GroupHandler) ExcelMode(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid group ID")
		return
	}
	group, err := h.adminService.GetGroup(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if group.Platform != service.PlatformOpenAI {
		response.BadRequest(c, "Excel transport requires an OpenAI group")
		return
	}
	if c.Request.Method == "PUT" {
		var input struct {
			Mode string `json:"mode"`
		}
		if c.ShouldBindJSON(&input) != nil || (input.Mode != "native" && input.Mode != "excel") {
			response.BadRequest(c, "Mode must be native or excel")
			return
		}
		if err := service.SetExcelGroupMode(id, input.Mode); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
	}
	cfg, err := service.ReadExcelRoutingConfig()
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	_, credentialErr := cfg.TransportKey()
	response.Success(c, gin.H{"mode": cfg.Mode(id), "available": credentialErr == nil})
}
