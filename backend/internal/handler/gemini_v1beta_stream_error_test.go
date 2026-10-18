package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Once a Gemini SSE stream is committed (for example by Gemini Web
// heartbeats), a Google error must be written as an SSE event, not as a JSON
// body appended to the event stream.
func TestGoogleErrorOnCommittedStreamWritesSSEEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:streamGenerateContent", nil)
	c.Header("Content-Type", "text/event-stream")
	c.Writer.WriteHeaderNow()
	_, _ = c.Writer.Write([]byte("\n\n"))

	googleError(c, http.StatusBadGateway, "Gemini Web failed to generate a reply")

	body := strings.TrimLeft(recorder.Body.String(), "\n")
	require.True(t, strings.HasPrefix(body, "data: "), "body = %q", body)
	require.True(t, strings.HasSuffix(body, "\n\n"))
	var event struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(body, "data: "))), &event))
	require.Equal(t, http.StatusBadGateway, event.Error.Code)
	require.Equal(t, "Gemini Web failed to generate a reply", event.Error.Message)
}

func TestGoogleErrorBeforeStreamWritesJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	googleError(c, http.StatusBadGateway, "boom")
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.True(t, strings.HasPrefix(recorder.Body.String(), "{"), recorder.Body.String())
}
