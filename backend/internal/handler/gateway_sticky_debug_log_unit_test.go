//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const stickyLogMetadataUserID = `{"device_id":"device-secret-0123456789","account_uuid":"acc-uuid","session_id":"sess-abcdef1234567890"}`

func serveMessagesWithObservedLogs(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2401, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := []*service.Account{queueFullAccount(queueFullPrimaryAccount, 1, group.ID, service.AccountTypeAPIKey, nil)}
	h, cleanup := newQueueFullReselectHandler(t, group, accounts,
		&scriptedSlotCache{script: map[int64][]bool{}, attempts: map[int64]int{}},
		&queueFullHelperCache{increments: map[int64]int{}},
		newRecordingSessionLimitCache(), &recordingUpstream{}, false)
	defer cleanup()

	core, logs := observer.New(level)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"model":"claude-sonnet-4-5","max_tokens":64,"metadata":{"user_id":` + strconv.Quote(stickyLogMetadataUserID) + `},"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), ctxkey.Group, group)
	ctx = logger.IntoContext(ctx, zap.New(core))
	c.Request = req.WithContext(ctx)
	groupID := group.ID
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 3401, UserID: 4401, GroupID: &groupID, Status: service.StatusActive, Group: group, User: &service.User{ID: 4401, Concurrency: 10, Balance: 100}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 4401, Concurrency: 10})
	h.Messages(c)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	return logs
}

func TestGatewayHandlerMessages_StickyLogsStayOffInfo(t *testing.T) {
	logs := serveMessagesWithObservedLogs(t, zapcore.InfoLevel)
	for _, entry := range logs.All() {
		require.False(t, strings.HasPrefix(entry.Message, "sticky."), "unexpected Info log %s", entry.Message)
	}
}

func TestGatewayHandlerMessages_StickyDebugLogsDropRawMetadata(t *testing.T) {
	logs := serveMessagesWithObservedLogs(t, zapcore.DebugLevel)
	generated := logs.FilterMessage("sticky.session_hash_generated").All()
	require.Len(t, generated, 1)
	fields := generated[0].ContextMap()
	require.NotContains(t, fields, "metadata_user_id_raw")
	require.Equal(t, int64(len(stickyLogMetadataUserID)), fields["metadata_user_id_len"])
	require.Equal(t, "sess-abc", fields["session_hash"])
	require.NotEmpty(t, logs.FilterMessage("sticky.account_selected").All())
	for _, entry := range logs.All() {
		for _, value := range entry.ContextMap() {
			if s, ok := value.(string); ok {
				require.NotContains(t, s, "device-secret-0123456789")
				require.NotContains(t, s, "sess-abcdef1234567890")
			}
		}
	}
}
