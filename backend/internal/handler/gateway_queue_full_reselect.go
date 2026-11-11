package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// reselectAfterAccountQueueFull 在首选账号等待队列已满时，排除它重新选号一次。
// 只接受当场抢到真实槽位（Acquired）的另一个账号；选号失败、或只拿到等待计划时返回 nil，
// 由调用方维持原来的 429 gateway_queue_full。拿到的等待计划不会被使用：把排队转到下一个
// 账号的队列在线上回放里会让大量请求挂满等待超时后才失败。被丢弃的等待计划在选号时
// 注册过会话（max_sessions），这里立即释放；但会话本就粘在该账号上（boundAccountID）时，
// 注册早于本请求，不能删。
func (h *GatewayHandler) reselectAfterAccountQueueFull(ctx context.Context, groupID *int64, sessionKey, model string, failed map[int64]struct{}, busyAccountID, boundAccountID int64, metadataUserID string, userID int64) *service.AccountSelectionResult {
	if h == nil || h.gatewayService == nil || !h.gatewayService.QueueFullReselectEnabled() {
		return nil
	}
	excluded := mergeAccountIDSets(failed, map[int64]struct{}{busyAccountID: {}})
	alternative, err := h.gatewayService.SelectAccountWithLoadAwareness(ctx, groupID, sessionKey, model, excluded, metadataUserID, userID)
	if err != nil || alternative == nil || alternative.Account == nil {
		return nil
	}
	if alternative.Account.ID == busyAccountID {
		if alternative.Acquired && alternative.ReleaseFunc != nil {
			alternative.ReleaseFunc()
		}
		return nil
	}
	if !alternative.Acquired {
		if alternative.Account.ID != boundAccountID {
			h.gatewayService.ReleaseAccountSession(context.Background(), alternative.Account, sessionKey)
		}
		return nil
	}
	return alternative
}

// interceptWarmupAfterReselect 队列满换号后，对新账号重做预热/建议模式拦截检查：
// 选号后的检查只针对首选账号，新账号开了拦截时同样要返回 mock 响应，不能把预热请求发到上游。
// 命中时释放新账号的槽位并写出 mock 响应，返回 true 表示请求已处理完毕。
func interceptWarmupAfterReselect(c *gin.Context, selection *service.AccountSelectionResult, body []byte, model string, maxTokens int, isClaudeCodeClient, stream bool) bool {
	if selection == nil || selection.Account == nil || !selection.Account.IsInterceptWarmupEnabled() {
		return false
	}
	interceptType := detectInterceptType(body, model, maxTokens, isClaudeCodeClient)
	if interceptType == InterceptTypeNone {
		return false
	}
	if selection.Acquired && selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	if stream {
		sendMockInterceptStream(c, model, interceptType)
	} else {
		sendMockInterceptResponse(c, model, interceptType)
	}
	return true
}
