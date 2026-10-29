package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) ListFamilyGroups(c *gin.Context) {
	groups, err := h.adminService.ListAccountFamilyGroups(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, groups)
}
