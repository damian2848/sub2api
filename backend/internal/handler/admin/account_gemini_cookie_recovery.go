package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type geminiWebCookieRecoveryService interface {
	GeminiWebCookieRecovery(context.Context, int64, string, string, string, int64) (*service.GeminiWebCookieRecoveryResult, error)
}

func (h *AccountHandler) GeminiWebCookieRecovery(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	responder := func(status string) {
		response.Success(c, &service.GeminiWebCookieRecoveryResult{Capability: service.GeminiWebCookieRecoveryCapability, Status: status})
	}
	recovery, ok := h.adminService.(geminiWebCookieRecoveryService)
	if !ok || h.rateLimitService == nil {
		responder("capability_missing")
		return
	}
	capturedCache, err := h.rateLimitService.PrepareGeminiWebCookieRecoveryCache(c.Request.Context(), id)
	if err != nil {
		responder("capability_missing")
		return
	}
	var request struct {
		Email      string `json:"email"`
		Revision   string `json:"revision"`
		CookieJSON string `json:"cookie_json"`
		DeadlineAt int64  `json:"deadline_at"`
	}
	if c.Request.Method == http.MethodGet {
		request.Email = c.Query("email")
	} else {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 600*1024)
		if c.ShouldBindJSON(&request) != nil || request.CookieJSON == "" {
			responder("invalid_request")
			return
		}
	}
	result, err := recovery.GeminiWebCookieRecovery(c.Request.Context(), id, request.Email, request.Revision, request.CookieJSON, request.DeadlineAt)
	if err != nil {
		responder("remote_error")
		return
	}
	if result.Applied {
		if err := h.rateLimitService.FinalizeGeminiWebCookieRecovery(c.Request.Context(), id, capturedCache); err != nil {
			responder("verification_failed")
			return
		}
	}
	response.Success(c, result)
}
