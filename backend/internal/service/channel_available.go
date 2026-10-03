package service

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
)

// AvailableGroupRef 渠道视图中关联分组的简要信息。
//
// 用户侧「可用渠道」页面据此展示：专属分组 vs 公开分组（IsExclusive）、
// 订阅 vs 标准（SubscriptionType）、默认倍率（RateMultiplier）与高峰倍率规则。
// 用户专属倍率不在这里暴露，前端自己通过 /groups/rates 拉取，和 API 密钥页面保持一致。
type AvailableGroupRef struct {
	ID                 int64
	Name               string
	Platform           string
	SubscriptionType   string
	RateMultiplier     float64
	PeakRateEnabled    bool
	PeakStart          string
	PeakEnd            string
	PeakRateMultiplier float64
	IsExclusive        bool
}

// AvailableChannel 可用渠道视图：用于「可用渠道」页面展示渠道基础信息 +
// 关联的分组 + 推导出的支持模型列表（无通配符）。
type AvailableChannel struct {
	ID                 int64
	Name               string
	Description        string
	Status             string
	BillingModelSource string
	RestrictModels     bool
	Groups             []AvailableGroupRef
	SupportedModels    []SupportedModel
}

// ListAvailable 返回所有渠道的可用视图：每个渠道附带关联分组信息与支持模型列表。
//
// 支持模型通过 (*Channel).SupportedModels() 计算（mapping ∪ pricing 并联）。
// 对于渠道未配置定价的模型，进一步用 PricingService 的全局 LiteLLM 数据合成
// 一份展示用定价，让用户看到默认价格而非"未配置"。
//
// 关联分组信息通过 groupRepo.ListActive 查询后按 ID 映射；渠道 GroupIDs 中未在活跃列表中
// 的分组（已停用或删除）会被忽略。
//
// 前置条件：s.groupRepo 必须非 nil（由 wire DI 保证）。直接 nil-deref 用于 fail-fast，
// 避免静默掩盖注入缺失。
func (s *ChannelService) ListAvailable(ctx context.Context) ([]AvailableChannel, error) {
	channels, err := s.repo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}

	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}
	groupByID := make(map[int64]AvailableGroupRef, len(groups))
	for i := range groups {
		g := groups[i]
		groupByID[g.ID] = AvailableGroupRef{
			ID:                 g.ID,
			Name:               g.Name,
			Platform:           g.Platform,
			SubscriptionType:   g.SubscriptionType,
			RateMultiplier:     g.RateMultiplier,
			PeakRateEnabled:    g.PeakRateEnabled,
			PeakStart:          g.PeakStart,
			PeakEnd:            g.PeakEnd,
			PeakRateMultiplier: g.PeakRateMultiplier,
			IsExclusive:        g.IsExclusive,
		}
	}

	out := make([]AvailableChannel, 0, len(channels))
	for i := range channels {
		ch := &channels[i]
		groups := make([]AvailableGroupRef, 0, len(ch.GroupIDs))
		for _, gid := range ch.GroupIDs {
			if ref, ok := groupByID[gid]; ok {
				groups = append(groups, ref)
			}
		}
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })

		ch.normalizeBillingModelSource()

		supported := ch.SupportedModels()
		fillGlobalPricingFallback(s.pricingService, supported)

		out = append(out, AvailableChannel{
			ID:                 ch.ID,
			Name:               ch.Name,
			Description:        ch.Description,
			Status:             ch.Status,
			BillingModelSource: ch.BillingModelSource,
			RestrictModels:     ch.RestrictModels,
			Groups:             groups,
			SupportedModels:    supported,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// AvailableGroupChannelRef 分组视图中该分组所挂渠道的简要信息。
type AvailableGroupChannelRef struct {
	Name        string
	Description string
}

// AvailableGroupView 可用渠道的分组视角：每个分组一条，附带所挂渠道与候选模型。
//
// Models 只是「渠道配置层」的候选集合（已按分组平台隔离、跨渠道去重），
// 是否真实可调用由调用方再结合账号映射 / 分组白名单 / 用户禁用裁剪；
// 为此一并带出分组的模型白名单配置。
type AvailableGroupView struct {
	Group          AvailableGroupRef
	ModelAllowlist GroupModelAllowlist
	Channels       []AvailableGroupChannelRef
	Models         []SupportedModel
}

// ListAvailableByGroup 以分组为顶层返回「可用渠道」视图，只含 allowedGroupIDs 内的分组。
//
// 同平台的分组彼此独立、不合并。候选模型口径与 ModelPlazaService.ListGroups 一致：
//   - 仅 Active 渠道；SupportedModels ∪ 全局定价回落；
//   - 普通分组按分组平台隔离，Composite 分组展开为渠道已配置的具体平台；
//   - 同分组跨渠道同名模型「先见者胜」，仅当已存条目无定价而新条目有定价时升级。
//
// 至少挂了一个 Active 渠道的分组才会返回；候选模型为空的分组也保留（前端显示“未配置模型”）。
// 输出按 平台 → 倍率 → 名称 排序，组内模型按名称 → 平台排序。
func (s *ChannelService) ListAvailableByGroup(ctx context.Context, allowedGroupIDs map[int64]struct{}) ([]AvailableGroupView, error) {
	channels, err := s.repo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}

	sort.SliceStable(channels, func(i, j int) bool {
		return strings.ToLower(channels[i].Name) < strings.ToLower(channels[j].Name)
	})

	views := make(map[int64]*AvailableGroupView, len(groups))
	order := make([]int64, 0, len(groups))
	for i := range groups {
		g := groups[i]
		if _, ok := allowedGroupIDs[g.ID]; !ok {
			continue
		}
		views[g.ID] = &AvailableGroupView{
			Group: AvailableGroupRef{
				ID:                 g.ID,
				Name:               g.Name,
				Platform:           g.Platform,
				SubscriptionType:   g.SubscriptionType,
				RateMultiplier:     g.RateMultiplier,
				PeakRateEnabled:    g.PeakRateEnabled,
				PeakStart:          g.PeakStart,
				PeakEnd:            g.PeakEnd,
				PeakRateMultiplier: g.PeakRateMultiplier,
				IsExclusive:        g.IsExclusive,
			},
			ModelAllowlist: g.ModelAllowlist,
		}
		order = append(order, g.ID)
	}

	type modelKey struct{ platform, name string }
	modelIdx := make(map[int64]map[modelKey]int, len(views))
	for i := range channels {
		ch := &channels[i]
		if ch.Status != StatusActive {
			continue
		}
		ch.normalizeBillingModelSource()
		supported := ch.SupportedModels()
		fillGlobalPricingFallback(s.pricingService, supported)

		for _, gid := range ch.GroupIDs {
			v, ok := views[gid]
			if !ok {
				continue
			}
			v.Channels = append(v.Channels, AvailableGroupChannelRef{Name: ch.Name, Description: ch.Description})
			idx := modelIdx[gid]
			if idx == nil {
				idx = make(map[modelKey]int, len(supported))
				modelIdx[gid] = idx
			}
			for j := range supported {
				m := supported[j]
				if v.Group.Platform == PlatformComposite {
					if !isConcreteRequestPlatform(m.Platform) {
						continue
					}
				} else if m.Platform != v.Group.Platform {
					continue
				}
				key := modelKey{platform: m.Platform, name: m.Name}
				if at, seen := idx[key]; seen {
					if v.Models[at].Pricing == nil && m.Pricing != nil {
						v.Models[at].Pricing = m.Pricing
					}
					continue
				}
				idx[key] = len(v.Models)
				v.Models = append(v.Models, m)
			}
		}
	}

	out := make([]AvailableGroupView, 0, len(order))
	for _, gid := range order {
		v := views[gid]
		if len(v.Channels) == 0 {
			continue
		}
		sort.SliceStable(v.Models, func(i, j int) bool {
			if v.Models[i].Name != v.Models[j].Name {
				return v.Models[i].Name < v.Models[j].Name
			}
			return v.Models[i].Platform < v.Models[j].Platform
		})
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Group, out[j].Group
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.RateMultiplier != b.RateMultiplier {
			return a.RateMultiplier < b.RateMultiplier
		}
		return a.Name < b.Name
	})
	return out, nil
}

// NarrowModelsByListing 用「账号实际可调用」清单（GatewayService.GetAvailableModels 的返回值）
// 裁剪渠道候选模型：保留精确命中或命中清单中末尾 * 通配条目的模型。
// listed 为空表示该分组没有显式映射（默认全开放），候选原样返回。
func NarrowModelsByListing(models []SupportedModel, listed []string) []SupportedModel {
	if len(listed) == 0 {
		return models
	}
	out := make([]SupportedModel, 0, len(models))
	for _, m := range models {
		for _, entry := range listed {
			if strings.EqualFold(entry, m.Name) || matchWildcard(entry, m.Name) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// fillGlobalPricingFallback 对未命中渠道定价的支持模型，从全局 LiteLLM 数据合成一份
// 展示用定价。仅用于「可用渠道」展示，不影响真实计费链路。
//
// 触发条件：
//  1. Pricing == nil（渠道完全没声明该模型的定价条目）
//  2. Pricing 非 nil 但所有价格字段为空（admin UI 建了条目但没填价格）
//
// 当 pricingService 为 nil（测试场景），跳过价格回落。
// 可用渠道与模型广场共用。
func fillGlobalPricingFallback(pricingService *PricingService, models []SupportedModel) {
	for i := range models {
		if pricingService != nil && pricingNeedsFallback(models[i].Pricing) {
			if lp := pricingService.GetModelPricing(models[i].Name); lp != nil {
				models[i].Pricing = synthesizePricingFromLiteLLM(lp, models[i].Pricing)
			}
		}
	}
}

// pricingNeedsFallback 判定一个 ChannelModelPricing 是否需要走全局回落。
// 价格全部缺失（无 flat 字段且无任何带价 interval）即视为未配置。
func pricingNeedsFallback(p *ChannelModelPricing) bool {
	if p == nil {
		return true
	}
	if p.InputPrice != nil || p.OutputPrice != nil ||
		p.CacheWritePrice != nil || p.CacheWrite1hPrice != nil || p.CacheReadPrice != nil ||
		p.ImageOutputPrice != nil || p.PerRequestPrice != nil {
		return false
	}
	for _, iv := range p.Intervals {
		if iv.InputPrice != nil || iv.OutputPrice != nil ||
			iv.CacheWritePrice != nil || iv.CacheWrite1hPrice != nil || iv.CacheReadPrice != nil ||
			iv.PerRequestPrice != nil {
			return false
		}
	}
	return true
}

// synthesizePricingFromLiteLLM 把 LiteLLM 的定价数据转成 ChannelModelPricing 形态，
// 仅用于展示。
//
// 计费模式优先级：
//  1. 渠道已选 BillingMode（admin 在 UI 里选了 image / per_request 但没填价的场景，
//     按选定模式合成对应字段）
//  2. LiteLLM mode="image_generation" → image
//  3. 默认 token
//
// LiteLLM 中字段 0 视为未配置，不带入展示。
func synthesizePricingFromLiteLLM(lp *LiteLLMModelPricing, existing *ChannelModelPricing) *ChannelModelPricing {
	if lp == nil {
		return existing
	}

	mode := BillingModeToken
	switch {
	case existing != nil && existing.BillingMode != "":
		mode = existing.BillingMode
	case lp.Mode == "image_generation":
		mode = BillingModeImage
	}

	if mode == BillingModeImage || mode == BillingModePerRequest {
		return &ChannelModelPricing{
			BillingMode:                mode,
			PerRequestPrice:            nonZeroPtr(lp.OutputCostPerImage),
			ImageOutputPrice:           nonZeroPtr(lp.OutputCostPerImageToken),
			InputPrice:                 nonZeroPtr(lp.InputCostPerToken),
			OutputPrice:                nonZeroPtr(lp.OutputCostPerToken),
			ReasoningEffortMultipliers: reasoningEffortMultipliersFromPricing(existing),
		}
	}
	return &ChannelModelPricing{
		BillingMode:                mode,
		InputPrice:                 nonZeroPtr(lp.InputCostPerToken),
		OutputPrice:                nonZeroPtr(lp.OutputCostPerToken),
		CacheWritePrice:            nonZeroPtr(lp.CacheCreationInputTokenCost),
		CacheWrite1hPrice:          nonZeroPtr(lp.CacheCreationInputTokenCostAbove1hr),
		CacheReadPrice:             nonZeroPtr(lp.CacheReadInputTokenCost),
		ImageOutputPrice:           nonZeroPtr(lp.OutputCostPerImageToken),
		ReasoningEffortMultipliers: reasoningEffortMultipliersFromPricing(existing),
	}
}

func reasoningEffortMultipliersFromPricing(pricing *ChannelModelPricing) map[string]float64 {
	if pricing == nil {
		return nil
	}
	return maps.Clone(pricing.ReasoningEffortMultipliers)
}

func nonZeroPtr(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}
