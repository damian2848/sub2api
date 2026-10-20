package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const (
	forbiddenTypeValidation = "validation"
	forbiddenTypeViolation  = "violation"
	forbiddenTypeForbidden  = "forbidden"

	// 机器可读的错误码
	errorCodeForbidden       = "forbidden"
	errorCodeUnauthenticated = "unauthenticated"
	errorCodeRateLimited     = "rate_limited"
	errorCodeNetworkError    = "network_error"
)

// AntigravityQuotaFetcher 从 Antigravity API 获取额度
type AntigravityQuotaFetcher struct {
	proxyRepo ProxyRepository
	cfg       *config.Config
}

// NewAntigravityQuotaFetcher 创建 AntigravityQuotaFetcher
func NewAntigravityQuotaFetcher(proxyRepo ProxyRepository, cfg *config.Config) *AntigravityQuotaFetcher {
	return &AntigravityQuotaFetcher{proxyRepo: proxyRepo, cfg: cfg}
}

// CanFetch 检查是否可以获取此账户的额度
func (f *AntigravityQuotaFetcher) CanFetch(account *Account) bool {
	if account.Platform != PlatformAntigravity {
		return false
	}
	accessToken := account.GetCredential("access_token")
	return accessToken != ""
}

// FetchQuota 获取 Antigravity 账户额度信息
func (f *AntigravityQuotaFetcher) FetchQuota(ctx context.Context, account *Account, proxyURL string) (*QuotaResult, error) {
	return f.fetchQuotaFromURL(ctx, account, proxyURL, resolveAntigravityForwardBaseURL(account))
}

// Pin both quota sources to the account's forwarding endpoint. Production and
// daily can expose different pools; a successful response from the other pool
// is not a valid fallback for the account's actual traffic.
func (f *AntigravityQuotaFetcher) fetchQuotaFromURL(ctx context.Context, account *Account, proxyURL, baseURL string) (*QuotaResult, error) {
	accessToken := account.GetCredential("access_token")
	projectID := account.GetCredential("project_id")

	client, err := antigravity.NewClient(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create antigravity client failed: %w", err)
	}

	// The official client uses retrieveUserQuotaSummary for rolling windows.
	// Fetch model metadata and subscription info independently: a catalog failure
	// must not hide a valid summary, and a slow optional call must not serialize
	// the three upstream requests.
	var summary *antigravity.RetrieveUserQuotaSummaryResponse
	var modelsResp *antigravity.FetchAvailableModelsResponse
	var summaryRaw, modelsRaw map[string]any
	var summaryErr, modelsErr error
	var tierRaw, tierNormalized string
	var loadResp *antigravity.LoadCodeAssistResponse
	var queries sync.WaitGroup
	queries.Add(3)
	go func() {
		defer queries.Done()
		summary, summaryRaw, summaryErr = client.RetrieveUserQuotaSummaryFromURL(ctx, baseURL, accessToken, projectID, resolveModelsListReadLimit(f.cfg))
	}()
	go func() {
		defer queries.Done()
		modelsResp, modelsRaw, modelsErr = client.FetchAvailableModelsFromURL(ctx, baseURL, accessToken, projectID, resolveModelsListReadLimit(f.cfg))
	}()
	go func() {
		defer queries.Done()
		tierRaw, tierNormalized, loadResp = f.fetchSubscriptionTier(ctx, client, accessToken)
	}()
	queries.Wait()

	groups := buildAntigravityQuotaGroups(summary)
	if len(groups) == 0 && modelsErr != nil {
		// Preserve existing forbidden/reauth handling when neither quota source
		// succeeded. Optional catalog errors do not override an official summary.
		var forbiddenErr *antigravity.ForbiddenError
		if errors.As(modelsErr, &forbiddenErr) {
			now := time.Now()
			fbType := classifyForbiddenType(forbiddenErr.Body)
			return &QuotaResult{
				UsageInfo: &UsageInfo{
					Source: "active", UpdatedAt: &now,
					IsForbidden: true, ForbiddenReason: forbiddenErr.Body,
					ForbiddenType: fbType, ValidationURL: extractValidationURL(forbiddenErr.Body),
					NeedsVerify: fbType == forbiddenTypeValidation, IsBanned: fbType == forbiddenTypeViolation,
					ErrorCode: errorCodeForbidden,
				},
			}, nil
		}
		return nil, modelsErr
	}

	// 转换为 UsageInfo
	usageInfo := f.buildUsageInfo(modelsResp, tierRaw, tierNormalized, loadResp)

	if len(groups) > 0 {
		usageInfo.AntigravityQuotaGroups = groups
		usageInfo.AntigravityQuotaSource = "quota_summary"
	} else {
		// A legacy model fraction is not a substitute for a 5h or weekly window.
		usageInfo.AntigravityQuotaSource = "model_quota"
		usageInfo.AntigravityQuotaError = "quota_summary_unavailable"
		if summaryErr != nil {
			slog.Warn("antigravity quota summary unavailable", "error", summaryErr)
		}
	}
	if modelsRaw == nil {
		modelsRaw = make(map[string]any)
	}
	if summaryRaw != nil {
		modelsRaw["quotaSummary"] = summaryRaw
	}

	return &QuotaResult{
		UsageInfo: usageInfo,
		Raw:       modelsRaw,
	}, nil
}

// fetchSubscriptionTier 获取账号订阅等级，失败返回空字符串。
// 同时返回 LoadCodeAssistResponse，以便提取 AI Credits 余额。
func (f *AntigravityQuotaFetcher) fetchSubscriptionTier(ctx context.Context, client *antigravity.Client, accessToken string) (raw, normalized string, loadResp *antigravity.LoadCodeAssistResponse) {
	loadResp, _, err := client.LoadCodeAssist(ctx, accessToken)
	if err != nil {
		slog.Warn("failed to fetch subscription tier", "error", err)
		return "", "", nil
	}
	if loadResp == nil {
		return "", "", nil
	}

	raw = loadResp.GetTier() // 已有方法：paidTier > currentTier
	normalized = normalizeTier(raw)
	return raw, normalized, loadResp
}

// normalizeTier 将原始 tier 字符串归一化为 FREE/PRO/ULTRA/UNKNOWN
func normalizeTier(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "ultra"):
		return "ULTRA"
	case strings.Contains(lower, "pro"):
		return "PRO"
	case strings.Contains(lower, "free"):
		return "FREE"
	default:
		return "UNKNOWN"
	}
}

// buildUsageInfo 将 API 响应转换为 UsageInfo。
func (f *AntigravityQuotaFetcher) buildUsageInfo(modelsResp *antigravity.FetchAvailableModelsResponse, tierRaw, tierNormalized string, loadResp *antigravity.LoadCodeAssistResponse) *UsageInfo {
	now := time.Now()
	info := &UsageInfo{
		Source:                  "active",
		UpdatedAt:               &now,
		AntigravityQuota:        make(map[string]*AntigravityModelQuota),
		AntigravityQuotaDetails: make(map[string]*AntigravityModelDetail),
		SubscriptionTier:        tierNormalized,
		SubscriptionTierRaw:     tierRaw,
	}

	if modelsResp == nil {
		modelsResp = &antigravity.FetchAvailableModelsResponse{}
	}

	// 遍历所有模型，填充 AntigravityQuota 和 AntigravityQuotaDetails
	for modelName, modelInfo := range modelsResp.Models {
		if modelInfo.QuotaInfo == nil {
			continue
		}

		// remainingFraction 是剩余比例 (0.0-1.0)，转换为使用率百分比
		utilization := int((1.0 - modelInfo.QuotaInfo.RemainingFraction) * 100)

		info.AntigravityQuota[modelName] = &AntigravityModelQuota{
			Utilization: utilization,
			ResetTime:   modelInfo.QuotaInfo.ResetTime,
		}

		// 填充模型详细能力信息
		detail := &AntigravityModelDetail{
			DisplayName:        modelInfo.DisplayName,
			SupportsImages:     modelInfo.SupportsImages,
			SupportsThinking:   modelInfo.SupportsThinking,
			ThinkingBudget:     modelInfo.ThinkingBudget,
			Recommended:        modelInfo.Recommended,
			MaxTokens:          modelInfo.MaxTokens,
			MaxOutputTokens:    modelInfo.MaxOutputTokens,
			SupportedMimeTypes: modelInfo.SupportedMimeTypes,
		}
		info.AntigravityQuotaDetails[modelName] = detail
	}

	// 废弃模型转发规则
	if len(modelsResp.DeprecatedModelIDs) > 0 {
		info.ModelForwardingRules = make(map[string]string, len(modelsResp.DeprecatedModelIDs))
		for oldID, deprecated := range modelsResp.DeprecatedModelIDs {
			info.ModelForwardingRules[oldID] = deprecated.NewModelID
		}
	}

	// fetchAvailableModels does not identify the quota period. In particular,
	// never synthesize FiveHour from a model's remainingFraction/resetTime.

	if loadResp != nil {
		for _, credit := range loadResp.GetAvailableCredits() {
			info.AICredits = append(info.AICredits, AICredit{
				CreditType:     credit.CreditType,
				Amount:         credit.GetAmount(),
				MinimumBalance: credit.GetMinimumAmount(),
			})
		}
	}

	return info
}

// GetProxyURL 获取账户的代理 URL
func (f *AntigravityQuotaFetcher) GetProxyURL(ctx context.Context, account *Account) string {
	if account.ProxyID == nil || f.proxyRepo == nil {
		return ""
	}
	proxy, err := f.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// classifyForbiddenType 根据 403 响应体判断禁止类型
func classifyForbiddenType(body string) string {
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "validation_required") ||
		strings.Contains(lower, "verify your account") ||
		strings.Contains(lower, "validation_url"):
		return forbiddenTypeValidation
	case strings.Contains(lower, "terms of service") ||
		strings.Contains(lower, "violation"):
		return forbiddenTypeViolation
	default:
		return forbiddenTypeForbidden
	}
}

// urlPattern 用于从 403 响应体中提取 URL（降级方案）
var urlPattern = regexp.MustCompile(`https://[^\s"'\\]+`)

// extractValidationURL 从 403 响应 JSON 中提取验证/申诉链接
func extractValidationURL(body string) string {
	// 1. 尝试结构化 JSON 提取: /error/details[*]/metadata/validation_url 或 appeal_url
	var parsed struct {
		Error struct {
			Details []struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil {
		for _, detail := range parsed.Error.Details {
			if u := detail.Metadata["validation_url"]; u != "" {
				return u
			}
			if u := detail.Metadata["appeal_url"]; u != "" {
				return u
			}
		}
	}

	// 2. 降级：正则匹配 URL
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "validation") &&
		!strings.Contains(lower, "verify") &&
		!strings.Contains(lower, "appeal") {
		return ""
	}
	// 先解码常见转义再匹配
	normalized := strings.ReplaceAll(body, `\u0026`, "&")
	if m := urlPattern.FindString(normalized); m != "" {
		return m
	}
	return ""
}
