package admin

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

type geminiWebCannedRepliesRequest struct {
	Custom []string `json:"custom"`
}

var errGeminiWebCannedSettingsUnavailable = errors.New("setting service is not configured")

// GetGeminiWebCannedReplies returns the built-in and custom canned failure
// phrases that fail a Gemini Web reply.
func (h *AccountHandler) GetGeminiWebCannedReplies(c *gin.Context) {
	if h.codexTicketSettings == nil {
		response.ErrorFrom(c, errGeminiWebCannedSettingsUnavailable)
		return
	}
	settings, err := h.codexTicketSettings.GetGeminiWebCannedReplySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateGeminiWebCannedReplies replaces the custom canned failure phrases.
func (h *AccountHandler) UpdateGeminiWebCannedReplies(c *gin.Context) {
	if h.codexTicketSettings == nil {
		response.ErrorFrom(c, errGeminiWebCannedSettingsUnavailable)
		return
	}
	var req geminiWebCannedRepliesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if _, err := h.codexTicketSettings.SetGeminiWebCannedReplySettings(c.Request.Context(), req.Custom); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	settings, err := h.codexTicketSettings.GetGeminiWebCannedReplySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
