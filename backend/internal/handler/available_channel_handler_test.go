//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUserAvailableChannel_Unauthenticated401(t *testing.T) {
	// 没有 AuthSubject 注入时，handler 应返回 401 且不触达 service 依赖。
	gin.SetMode(gin.TestMode)
	h := &AvailableChannelHandler{} // nil services — 401 路径不会调用它们
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/channels/available", nil)

	h.List(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestToUserSupportedModels_FiltersByAllowedPlatforms(t *testing.T) {
	// 用户可访问分组只覆盖 anthropic；anthropic 平台的模型保留，openai 模型被剔除。
	src := []service.SupportedModel{
		{Name: "claude-sonnet-4-6", Platform: "anthropic", Pricing: nil},
		{Name: "gpt-4o", Platform: "openai", Pricing: nil},
	}
	allowed := map[string]struct{}{"anthropic": {}}
	out := toUserSupportedModels(src, allowed)
	require.Len(t, out, 1)
	require.Equal(t, "claude-sonnet-4-6", out[0].Name)
}

func TestToUserSupportedModels_NilAllowedPlatformsKeepsAll(t *testing.T) {
	// 显式传 nil allowedPlatforms 表示不做过滤。
	src := []service.SupportedModel{
		{Name: "a", Platform: "anthropic"},
		{Name: "b", Platform: "openai"},
	}
	require.Len(t, toUserSupportedModels(src, nil), 2)
}

func TestUserAvailableGroupView_FieldWhitelist(t *testing.T) {
	// 响应以分组为顶层：只有 group / channels / models；不含管理端字段。
	row := userAvailableGroupView{
		Group:    userAvailableGroup{ID: 1, Name: "g1", Platform: "anthropic"},
		Channels: []userGroupChannelRef{{Name: "ch", Description: "d"}},
		Models:   []userSupportedModel{},
	}
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	for _, key := range []string{"id", "status", "billing_model_source", "restrict_models", "model_allowlist"} {
		_, exists := decoded[key]
		require.Falsef(t, exists, "user DTO must not expose %q", key)
	}
	for _, key := range []string{"group", "channels", "models"} {
		_, exists := decoded[key]
		require.Truef(t, exists, "user DTO must expose %q", key)
	}

	// Group DTO 暴露区分专属/公开、订阅类型、默认倍率和高峰倍率规则所需的字段，
	// 前端据此渲染 GroupBadge 并与 API 密钥页保持一致的视觉。
	rawGroup, err := json.Marshal(row.Group)
	require.NoError(t, err)
	var groupDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawGroup, &groupDecoded))
	for _, key := range []string{"id", "name", "platform", "subscription_type", "rate_multiplier", "peak_rate_enabled", "peak_start", "peak_end", "peak_rate_multiplier", "is_exclusive"} {
		_, exists := groupDecoded[key]
		require.Truef(t, exists, "group DTO must expose %q", key)
	}

	// pricing interval 白名单：不应暴露 id / sort_order。
	inputMultiplier := 2.0
	outputMultiplier := 1.5
	cacheWriteMultiplier := 2.0
	cacheReadMultiplier := 2.0
	pricing := toUserPricing(&service.ChannelModelPricing{
		BillingMode:                service.BillingModeToken,
		ReasoningEffortMultipliers: map[string]float64{"high": 1.5, "max": 3},
		Intervals: []service.PricingInterval{
			{
				ID: 7, MinTokens: 0, MaxTokens: nil, SortOrder: 3,
				InputMultiplier: &inputMultiplier, OutputMultiplier: &outputMultiplier,
				CacheWriteMultiplier: &cacheWriteMultiplier, CacheReadMultiplier: &cacheReadMultiplier,
			},
		},
	})
	require.NotNil(t, pricing)
	require.Equal(t, map[string]float64{"high": 1.5, "max": 3}, pricing.ReasoningEffortMultipliers)
	require.Len(t, pricing.Intervals, 1)
	rawIv, err := json.Marshal(pricing.Intervals[0])
	require.NoError(t, err)
	var ivDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawIv, &ivDecoded))
	for _, key := range []string{"id", "pricing_id", "sort_order"} {
		_, exists := ivDecoded[key]
		require.Falsef(t, exists, "user pricing interval must not expose %q", key)
	}
	for key, want := range map[string]float64{
		"input_multiplier": inputMultiplier, "output_multiplier": outputMultiplier,
		"cache_write_multiplier": cacheWriteMultiplier, "cache_read_multiplier": cacheReadMultiplier,
	} {
		got, exists := ivDecoded[key]
		require.Truef(t, exists, "user pricing interval must expose %q", key)
		require.InDelta(t, want, got.(float64), 1e-12)
	}
}

// stubGroupModelLister 按「分组 ID + 平台」返回预设的账号可调用清单。
type stubGroupModelLister map[string][]string

func (l stubGroupModelLister) GetAvailableModels(_ context.Context, groupID *int64, platform string) []string {
	return l[fmt.Sprintf("%d/%s", *groupID, platform)]
}

func groupView(id int64, platform string, allow service.GroupModelAllowlist, models ...service.SupportedModel) service.AvailableGroupView {
	return service.AvailableGroupView{
		Group:          service.AvailableGroupRef{ID: id, Name: fmt.Sprintf("g%d", id), Platform: platform},
		ModelAllowlist: allow,
		Channels:       []service.AvailableGroupChannelRef{{Name: "ch"}},
		Models:         models,
	}
}

func modelNames(models []service.SupportedModel) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return names
}

func TestResolveGroupModels_NilListerKeepsCandidates(t *testing.T) {
	v := groupView(1, service.PlatformOpenAI, service.GroupModelAllowlist{},
		service.SupportedModel{Name: "gpt-5", Platform: service.PlatformOpenAI},
		service.SupportedModel{Name: "gpt-4o", Platform: service.PlatformOpenAI},
	)
	got := resolveGroupModels(context.Background(), v, nil, nil)
	require.Equal(t, []string{"gpt-4o", "gpt-5"}, modelNames(got))
}

func TestResolveGroupModels_NoExplicitMappingDoesNotNarrow(t *testing.T) {
	// 账号无显式映射（清单为空）= 默认全开放，渠道候选原样保留。
	v := groupView(1, service.PlatformOpenAI, service.GroupModelAllowlist{},
		service.SupportedModel{Name: "gpt-5", Platform: service.PlatformOpenAI},
	)
	got := resolveGroupModels(context.Background(), v, stubGroupModelLister{}, nil)
	require.Equal(t, []string{"gpt-5"}, modelNames(got))
}

func TestResolveGroupModels_IntersectsWithAccountListing(t *testing.T) {
	// 渠道配了三个 claude 模型，账号只放行 opus 精确名 + sonnet-* 通配。
	v := groupView(1, service.PlatformAnthropic, service.GroupModelAllowlist{},
		service.SupportedModel{Name: "claude-opus-4-7", Platform: service.PlatformAnthropic},
		service.SupportedModel{Name: "claude-sonnet-4-6", Platform: service.PlatformAnthropic},
		service.SupportedModel{Name: "claude-haiku-4-5", Platform: service.PlatformAnthropic},
	)
	lister := stubGroupModelLister{"1/anthropic": {"claude-opus-4-7", "claude-sonnet-*"}}
	got := resolveGroupModels(context.Background(), v, lister, nil)
	require.Equal(t, []string{"claude-opus-4-7", "claude-sonnet-4-6"}, modelNames(got))
}

func TestResolveGroupModels_AllowlistAndDeniedFilter(t *testing.T) {
	v := groupView(1, service.PlatformOpenAI,
		service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-5*", "o3"}},
		service.SupportedModel{Name: "gpt-5", Platform: service.PlatformOpenAI},
		service.SupportedModel{Name: "gpt-5-mini", Platform: service.PlatformOpenAI},
		service.SupportedModel{Name: "o3", Platform: service.PlatformOpenAI},
		service.SupportedModel{Name: "gpt-4o", Platform: service.PlatformOpenAI},
	)
	// 白名单挡掉 gpt-4o；用户在该分组被禁用 gpt-5-mini。
	got := resolveGroupModels(context.Background(), v, nil, []string{"gpt-5-mini"})
	require.Equal(t, []string{"gpt-5", "o3"}, modelNames(got))
}

func TestResolveGroupModels_CompositeNarrowsPerConcretePlatform(t *testing.T) {
	// Composite 分组对每个具体平台分别求交：anthropic 有显式映射被裁剪，openai 无映射保持全开。
	v := groupView(9, service.PlatformComposite, service.GroupModelAllowlist{},
		service.SupportedModel{Name: "claude-opus-4-7", Platform: service.PlatformAnthropic},
		service.SupportedModel{Name: "claude-haiku-4-5", Platform: service.PlatformAnthropic},
		service.SupportedModel{Name: "gpt-5", Platform: service.PlatformOpenAI},
	)
	lister := stubGroupModelLister{"9/anthropic": {"claude-opus-4-7"}}
	got := resolveGroupModels(context.Background(), v, lister, nil)
	require.Equal(t, []string{"claude-opus-4-7", "gpt-5"}, modelNames(got))
}

func TestBuildUserGroupView_EmptyModelsStillReturnsGroup(t *testing.T) {
	v := groupView(1, service.PlatformOpenAI, service.GroupModelAllowlist{})
	view := buildUserGroupView(context.Background(), v, nil, nil)
	require.Equal(t, int64(1), view.Group.ID)
	require.Len(t, view.Channels, 1)
	require.NotNil(t, view.Models)
	require.Empty(t, view.Models)
}
