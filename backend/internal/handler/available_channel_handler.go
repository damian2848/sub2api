package handler

import (
	"context"
	"log/slog"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// groupModelLister 返回分组在指定平台下「账号实际可调用」的模型清单（空 = 无显式映射）。
// 由 *service.GatewayService 实现，抽成接口便于单测。
type groupModelLister interface {
	GetAvailableModels(ctx context.Context, groupID *int64, platform string) []string
}

// AvailableChannelHandler 处理用户侧「可用渠道」查询。
//
// 响应以分组为顶层（同平台分组不合并），每个分组只列出它真实可调用的模型：
//  1. 分组过滤：只保留用户可访问、且挂了 Active 渠道的分组；
//  2. 候选模型：分组关联渠道的支持模型（含定价），普通分组按分组平台隔离，
//     Composite 分组展开渠道已配置的具体平台；
//  3. 可调用裁剪：与分组下账号实际可调用清单（同 /v1/models 口径）求交，
//     再叠加分组模型白名单与用户在该分组被禁用的模型；
//  4. 字段白名单：仅返回用户需要的字段（省略 BillingModelSource / RestrictModels
//     / 内部 ID / Status 等管理字段）。
type AvailableChannelHandler struct {
	channelService *service.ChannelService
	apiKeyService  *service.APIKeyService
	settingService *service.SettingService
	gatewayService groupModelLister
}

// NewAvailableChannelHandler 创建用户侧可用渠道 handler。
func NewAvailableChannelHandler(
	channelService *service.ChannelService,
	apiKeyService *service.APIKeyService,
	settingService *service.SettingService,
	gatewayService *service.GatewayService,
) *AvailableChannelHandler {
	h := &AvailableChannelHandler{
		channelService: channelService,
		apiKeyService:  apiKeyService,
		settingService: settingService,
	}
	if gatewayService != nil {
		h.gatewayService = gatewayService
	}
	return h
}

// featureEnabled 返回 available-channels 开关是否启用。默认关闭（opt-in）。
func (h *AvailableChannelHandler) featureEnabled(c *gin.Context) bool {
	if h.settingService == nil {
		return false
	}
	return h.settingService.GetAvailableChannelsRuntime(c.Request.Context()).Enabled
}

// userAvailableGroup 用户可见的分组概要（白名单字段）。
//
// 前端据此区分专属 vs 公开分组（IsExclusive）、订阅 vs 标准分组（SubscriptionType，
// 订阅视觉加深），并展示默认倍率与高峰倍率规则；用户专属倍率前端走
// /groups/rates，和 API 密钥页面保持一致。
type userAvailableGroup struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"`
	Platform           string  `json:"platform"`
	SubscriptionType   string  `json:"subscription_type"`
	RateMultiplier     float64 `json:"rate_multiplier"`
	PeakRateEnabled    bool    `json:"peak_rate_enabled"`
	PeakStart          string  `json:"peak_start"`
	PeakEnd            string  `json:"peak_end"`
	PeakRateMultiplier float64 `json:"peak_rate_multiplier"`
	IsExclusive        bool    `json:"is_exclusive"`
}

// userSupportedModelPricing 用户可见的定价字段白名单。
type userSupportedModelPricing struct {
	BillingMode                string                   `json:"billing_mode"`
	InputPrice                 *float64                 `json:"input_price"`
	OutputPrice                *float64                 `json:"output_price"`
	CacheWritePrice            *float64                 `json:"cache_write_price"`
	CacheWrite1hPrice          *float64                 `json:"cache_write_1h_price"`
	CacheReadPrice             *float64                 `json:"cache_read_price"`
	ReasoningEffortMultipliers map[string]float64       `json:"reasoning_effort_multipliers,omitempty"`
	ImageInputPrice            *float64                 `json:"image_input_price"`
	ImageOutputPrice           *float64                 `json:"image_output_price"`
	PerRequestPrice            *float64                 `json:"per_request_price"`
	Intervals                  []userPricingIntervalDTO `json:"intervals"`
}

// userPricingIntervalDTO 定价区间白名单（去掉内部 ID、SortOrder 等前端不渲染的字段）。
type userPricingIntervalDTO struct {
	MinTokens            int      `json:"min_tokens"`
	MaxTokens            *int     `json:"max_tokens"`
	TierLabel            string   `json:"tier_label,omitempty"`
	InputPrice           *float64 `json:"input_price"`
	OutputPrice          *float64 `json:"output_price"`
	CacheWritePrice      *float64 `json:"cache_write_price"`
	CacheWrite1hPrice    *float64 `json:"cache_write_1h_price"`
	CacheReadPrice       *float64 `json:"cache_read_price"`
	InputMultiplier      *float64 `json:"input_multiplier"`
	OutputMultiplier     *float64 `json:"output_multiplier"`
	CacheWriteMultiplier *float64 `json:"cache_write_multiplier"`
	CacheReadMultiplier  *float64 `json:"cache_read_multiplier"`
	PerRequestPrice      *float64 `json:"per_request_price"`
}

// userSupportedModel 用户可见的支持模型条目。
type userSupportedModel struct {
	Name     string                     `json:"name"`
	Platform string                     `json:"platform"`
	Pricing  *userSupportedModelPricing `json:"pricing"`
}

// userGroupChannelRef 分组所挂渠道的简要信息。
type userGroupChannelRef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// userAvailableGroupView 用户可见的分组条目：分组概要 + 所挂渠道 + 该分组可调用的模型。
type userAvailableGroupView struct {
	Group    userAvailableGroup    `json:"group"`
	Channels []userGroupChannelRef `json:"channels"`
	Models   []userSupportedModel  `json:"models"`
}

// List 列出当前用户可见的「可用渠道」（分组视角）。
// GET /api/v1/channels/available
func (h *AvailableChannelHandler) List(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	// Feature 未启用时返回空数组（不暴露渠道信息）。检查放在认证之后，
	// 保持与未开关前的 401 行为一致：未登录先 401，登录后再按开关决定。
	if !h.featureEnabled(c) {
		response.Success(c, []userAvailableGroupView{})
		return
	}

	ctx := c.Request.Context()
	userGroups, err := h.apiKeyService.GetAvailableGroups(ctx, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	allowedGroupIDs := make(map[int64]struct{}, len(userGroups))
	for i := range userGroups {
		allowedGroupIDs[userGroups[i].ID] = struct{}{}
	}

	views, err := h.channelService.ListAvailableByGroup(ctx, allowedGroupIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 用户在分组内被禁用的模型仅是展示裁剪，真正的准入在网关；取数失败时照常展示。
	denied, err := h.apiKeyService.GetUserGroupDeniedModels(ctx, subject.UserID)
	if err != nil {
		slog.Warn("available_channels_user_denied_models_failed", "error", err, "user_id", subject.UserID)
		denied = nil
	}

	out := make([]userAvailableGroupView, 0, len(views))
	for i := range views {
		out = append(out, buildUserGroupView(ctx, views[i], h.gatewayService, denied[views[i].Group.ID]))
	}
	response.Success(c, out)
}

// buildUserGroupView 把 service 层分组视图转换成用户 DTO，并把候选模型裁剪成真实可调用集合。
func buildUserGroupView(
	ctx context.Context,
	v service.AvailableGroupView,
	lister groupModelLister,
	denied []string,
) userAvailableGroupView {
	channels := make([]userGroupChannelRef, 0, len(v.Channels))
	for _, ch := range v.Channels {
		channels = append(channels, userGroupChannelRef{Name: ch.Name, Description: ch.Description})
	}
	return userAvailableGroupView{
		Group: userAvailableGroup{
			ID:                 v.Group.ID,
			Name:               v.Group.Name,
			Platform:           v.Group.Platform,
			SubscriptionType:   v.Group.SubscriptionType,
			RateMultiplier:     v.Group.RateMultiplier,
			PeakRateEnabled:    v.Group.PeakRateEnabled,
			PeakStart:          v.Group.PeakStart,
			PeakEnd:            v.Group.PeakEnd,
			PeakRateMultiplier: v.Group.PeakRateMultiplier,
			IsExclusive:        v.Group.IsExclusive,
		},
		Channels: channels,
		Models:   toUserSupportedModels(resolveGroupModels(ctx, v, lister, denied), nil),
	}
}

// resolveGroupModels 计算分组真实可调用的模型：
//  1. 按平台与「账号实际可调用」清单求交（Composite 分组逐个具体平台求交；lister 为 nil
//     或某平台无显式映射时不裁剪该平台）；
//  2. 去掉不在分组模型白名单内的；
//  3. 去掉用户在该分组被禁用的。
func resolveGroupModels(
	ctx context.Context,
	v service.AvailableGroupView,
	lister groupModelLister,
	denied []string,
) []service.SupportedModel {
	byPlatform := make(map[string][]service.SupportedModel, 2)
	for _, m := range v.Models {
		byPlatform[m.Platform] = append(byPlatform[m.Platform], m)
	}
	platforms := make([]string, 0, len(byPlatform))
	for p := range byPlatform {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)

	groupID := v.Group.ID
	out := make([]service.SupportedModel, 0, len(v.Models))
	for _, platform := range platforms {
		models := byPlatform[platform]
		if lister != nil {
			models = service.NarrowModelsByListing(models, lister.GetAvailableModels(ctx, &groupID, platform))
		}
		for _, m := range models {
			if !v.ModelAllowlist.Allows(m.Name) || service.UserGroupDeniesModel(denied, m.Name) {
				continue
			}
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Platform < out[j].Platform
	})
	return out
}

// toUserSupportedModels 将 service 层支持模型转换为用户 DTO（字段白名单）。
// 仅保留平台在 allowedPlatforms 中的条目，防止跨平台模型信息泄漏。
// allowedPlatforms 为 nil 时不做平台过滤（保留全部，供测试或明确无过滤场景使用）。
func toUserSupportedModels(
	src []service.SupportedModel,
	allowedPlatforms map[string]struct{},
) []userSupportedModel {
	out := make([]userSupportedModel, 0, len(src))
	for i := range src {
		m := src[i]
		if allowedPlatforms != nil {
			if _, ok := allowedPlatforms[m.Platform]; !ok {
				continue
			}
		}
		out = append(out, userSupportedModel{
			Name:     m.Name,
			Platform: m.Platform,
			Pricing:  toUserPricing(m.Pricing),
		})
	}
	return out
}

// toUserPricingIntervals 将定价区间转换为用户 DTO 白名单形态；nil 入参返回 nil（JSON omitempty 可省略）。
func toUserPricingIntervals(src []service.PricingInterval) []userPricingIntervalDTO {
	if src == nil {
		return nil
	}
	intervals := make([]userPricingIntervalDTO, 0, len(src))
	for _, iv := range src {
		intervals = append(intervals, userPricingIntervalDTO{
			MinTokens:            iv.MinTokens,
			MaxTokens:            iv.MaxTokens,
			TierLabel:            iv.TierLabel,
			InputPrice:           iv.InputPrice,
			OutputPrice:          iv.OutputPrice,
			CacheWritePrice:      iv.CacheWritePrice,
			CacheWrite1hPrice:    iv.CacheWrite1hPrice,
			CacheReadPrice:       iv.CacheReadPrice,
			InputMultiplier:      iv.InputMultiplier,
			OutputMultiplier:     iv.OutputMultiplier,
			CacheWriteMultiplier: iv.CacheWriteMultiplier,
			CacheReadMultiplier:  iv.CacheReadMultiplier,
			PerRequestPrice:      iv.PerRequestPrice,
		})
	}
	return intervals
}

// toUserPricing 将 service 层定价转换为用户 DTO；入参为 nil 时返回 nil。
func toUserPricing(p *service.ChannelModelPricing) *userSupportedModelPricing {
	if p == nil {
		return nil
	}
	intervals := toUserPricingIntervals(p.Intervals)
	if intervals == nil {
		// 用户侧定价的 intervals 固定输出数组（空配置为 []），保持既有契约。
		intervals = []userPricingIntervalDTO{}
	}
	billingMode := string(p.BillingMode)
	if billingMode == "" {
		billingMode = string(service.BillingModeToken)
	}
	return &userSupportedModelPricing{
		BillingMode:                billingMode,
		InputPrice:                 p.InputPrice,
		OutputPrice:                p.OutputPrice,
		CacheWritePrice:            p.CacheWritePrice,
		CacheWrite1hPrice:          p.CacheWrite1hPrice,
		CacheReadPrice:             p.CacheReadPrice,
		ReasoningEffortMultipliers: p.ReasoningEffortMultipliers,
		ImageInputPrice:            p.ImageInputPrice,
		ImageOutputPrice:           p.ImageOutputPrice,
		PerRequestPrice:            p.PerRequestPrice,
		Intervals:                  intervals,
	}
}
