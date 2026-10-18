package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func openAIEncryptedContentWSCloseStatus(status int) coderws.StatusCode {
	if status == http.StatusConflict || status >= http.StatusInternalServerError {
		return coderws.StatusTryAgainLater
	}
	return coderws.StatusPolicyViolation
}

// A ciphertext owner is a routing requirement, so a failed owner lookup must
// not fall through to generic capacity handling or another account attempt.
func (h *OpenAIGatewayHandler) handleOpenAIEncryptedContentAffinityError(c *gin.Context, err error, streamStarted bool) bool {
	var affinityErr *service.OpenAIEncryptedContentAffinityError
	if !errors.As(err, &affinityErr) {
		return false
	}
	errType := "invalid_request_error"
	if affinityErr.HTTPStatus() >= http.StatusInternalServerError {
		errType = "api_error"
	}
	h.handleStreamingAwareErrorWithCode(c, affinityErr.HTTPStatus(), errType, affinityErr.Code, affinityErr.Message, streamStarted || c.Writer.Written(), false)
	return true
}
