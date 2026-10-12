package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// FilterSupportedChannelModels intersects the display catalog with the group's
// allowlist and configured account capabilities. Pricing alone is not evidence
// of support. One account lookup covers the whole platform; transient rate
// limits do not make supported models disappear from this catalog.
func (s *GatewayService) FilterSupportedChannelModels(ctx context.Context, group *Group, platform string, models []SupportedModel) ([]SupportedModel, error) {
	out := make([]SupportedModel, 0, len(models))
	if group == nil || len(models) == 0 {
		return out, nil
	}
	if s == nil || s.accountRepo == nil {
		return nil, fmt.Errorf("model availability repository is unavailable")
	}
	ctx = WithResolvedTargetPlatform(ctx, platform)
	platforms := []string{platform}
	if platform == PlatformAnthropic || platform == PlatformGemini {
		platforms = append(platforms, PlatformAntigravity)
	}
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, &group.ID, platforms, false)
	if err != nil {
		return nil, fmt.Errorf("list group model capabilities: %w", err)
	}
	// OAuth routing intentionally accepts unknown OpenAI names. For display,
	// use its synced catalog (or the default listing) instead of that permissive
	// routing fallback. Decode metadata only once per account.
	oauthCatalogs := make([]map[string]struct{}, len(accounts))
	for i := range accounts {
		if !accounts[i].IsOpenAIOAuth() {
			continue
		}
		catalog := make(map[string]struct{})
		if snapshot := accounts[i].GetUpstreamModelMetadataSnapshot(); snapshot != nil {
			for name := range snapshot.Models {
				catalog[name] = struct{}{}
			}
		} else {
			for _, name := range openai.DefaultModelIDs() {
				catalog[name] = struct{}{}
			}
		}
		oauthCatalogs[i] = catalog
	}
	for _, model := range models {
		if model.Platform != platform || strings.TrimSpace(model.Name) == "" || strings.Contains(model.Name, "*") || !group.ModelAllowlist.Allows(model.Name) {
			continue
		}
		requestedModel := model.Name
		if s.channelService != nil {
			mapping, restricted := s.channelService.ResolveChannelMappingAndRestrict(ctx, &group.ID, requestedModel)
			if restricted {
				continue
			}
			if mapping.Mapped {
				requestedModel = mapping.MappedModel
			}
		}
		for i := range accounts {
			account := &accounts[i]
			if account.Platform != platform && !(account.Platform == PlatformAntigravity && account.IsMixedSchedulingEnabled() && (platform == PlatformAnthropic || platform == PlatformGemini)) {
				continue
			}
			if catalog := oauthCatalogs[i]; catalog != nil {
				if _, explicitlyMapped := account.ResolveMappedModel(requestedModel); !explicitlyMapped {
					if _, listed := catalog[requestedModel]; !listed {
						continue
					}
				}
			}
			supported := account.IsModelSupportedInGroup(&group.ID, requestedModel)
			if account.Platform == PlatformAnthropic || account.Platform == PlatformAntigravity {
				supported = s.isModelSupportedByAccountInGroup(ctx, account, &group.ID, requestedModel)
			}
			if supported && !account.modelCapabilityRejectedForRequest(ctx, requestedModel) {
				out = append(out, model)
				break
			}
		}
	}
	return out, nil
}
