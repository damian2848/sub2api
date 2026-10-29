package admin

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"time"
)

type antigravityPlanlensImporter interface {
	AntigravityPlanlensImport(context.Context, service.AntigravityPlanlensImportRequest, bool) (*service.AntigravityPlanlensImportResult, error)
}

func (h *AccountHandler) AntigravityPlanlensImportPrepare(c *gin.Context) {
	h.antigravityPlanlensImport(c, false)
}
func (h *AccountHandler) AntigravityPlanlensImportCommit(c *gin.Context) {
	h.antigravityPlanlensImport(c, true)
}
func (h *AccountHandler) antigravityPlanlensImport(c *gin.Context, commit bool) {
	fail := func(status string) {
		response.Success(c, &service.AntigravityPlanlensImportResult{Capability: service.AntigravityPlanlensImportCapability, Status: status})
	}
	importer, ok := h.adminService.(antigravityPlanlensImporter)
	if !ok {
		fail("capability_missing")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)
	var request service.AntigravityPlanlensImportRequest
	if c.ShouldBindJSON(&request) != nil {
		fail("invalid_request")
		return
	}
	result, err := importer.AntigravityPlanlensImport(c.Request.Context(), request, commit)
	if err != nil {
		fail("remote_error")
		return
	}
	// Keep native new-account privacy setup outside the short DB transaction.
	// Existing accounts keep their administrator-selected extra/settings intact.
	if result.OK && result.Status == "imported" {
		id, _ := strconv.ParseInt(result.RemoteID, 10, 64)
		ctx, cancel := context.WithDeadline(c.Request.Context(), time.UnixMilli(request.DeadlineAt))
		defer cancel()
		if account, err := h.adminService.GetAccount(ctx, id); err == nil && account != nil {
			h.adminService.ForceAntigravityPrivacy(ctx, account)
		}
	}
	response.Success(c, result)
}
