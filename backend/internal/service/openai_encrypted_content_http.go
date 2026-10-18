package service

import (
	"context"

	"github.com/gin-gonic/gin"
)

// Observe the final client-facing representation, after tool namespace and
// argument restoration. Some non-streaming upstreams still return raw SSE.
func (s *OpenAIGatewayService) observeOpenAIEncryptedHTTPBody(ctx context.Context, c *gin.Context, account *Account, body []byte) error {
	observer := s.NewOpenAIEncryptedContentObserverForRequest(ctx, c, account)
	if observer == nil {
		return nil
	}
	if !bodyHasSSEFraming(body) {
		return observer.Observe(body)
	}
	var observeErr error
	forEachOpenAISSEFrame(string(body), func(eventType string, payload []byte) {
		if observeErr == nil {
			observeErr = observer.ObserveEvent(eventType, payload)
		}
	})
	return observeErr
}
