package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIEncryptedContentAffinityErrorPreservesActionAndTerminalEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &service.OpenAIGatewayService{}
	ctx := context.Background()
	groupID := int64(3)
	observer := svc.NewOpenAIEncryptedContentObserver(ctx, &groupID, 65, 1)
	require.NoError(t, observer.Observe([]byte(`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"send_message","arguments":"{\"message\":\"fixture-cipher\"}","encrypted_function_args":["message"]}}`)))
	err := svc.ValidateOpenAIEncryptedContentAccount(ctx, &groupID, 65, 2, []byte(`{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"fixture-cipher"}]}]}`))
	require.Error(t, err)

	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if started {
				_, writeErr := c.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
				require.NoError(t, writeErr)
			}
			h := &OpenAIGatewayHandler{}
			require.True(t, h.handleOpenAIEncryptedContentAffinityError(c, fmt.Errorf("forward: %w", err), false))
			if started {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), "response.failed", "an already-started response needs a terminal SSE error")
			} else {
				require.Equal(t, http.StatusConflict, rec.Code)
				require.Equal(t, "encrypted_content_account_mismatch", gjson.Get(rec.Body.String(), "error.code").String())
			}
			require.Contains(t, rec.Body.String(), "Reconnect")
			require.NotContains(t, rec.Body.String(), "fixture-cipher")
		})
	}
}

func TestOpenAIEncryptedContentAffinityErrorIgnoresUnrelatedError(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	require.False(t, (&OpenAIGatewayHandler{}).handleOpenAIEncryptedContentAffinityError(c, errors.New("other failure"), false))
	require.Empty(t, rec.Body.String())
}
