//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 以下为重构前各处手写的平台列表 / switch，作为平台清单派生结果的等价基准。
var (
	legacyAllPlatforms = []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
	}
	legacySchedulerSnapshotPlatforms = []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
	}
	legacyCompositeMatchingPlatforms = legacySchedulerSnapshotPlatforms
	legacyMultiProtocolProviders     = []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo}
	platformProbeValues              = append(append([]string{}, legacyAllPlatforms...), PlatformComposite, "", "moonshot", "Kimi", "openai ", "glm", "bogus")
	platformProbeAccountTypes        = []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken, AccountTypeUpstream, ""}
)

func legacyIsCNProvider(platform string) bool {
	switch platform {
	case PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax:
		return true
	}
	return false
}

func legacyIsOpenAICompatible(platform string) bool {
	return platform == PlatformOpenAI || platform == PlatformGrok || legacyIsCNProvider(platform) || platform == PlatformOpenCodeGo
}

func legacyNormalizeOpenAICompatiblePlatform(platform string) string {
	switch platform {
	case PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo:
		return platform
	}
	return PlatformOpenAI
}

func legacyIsUpstreamBillingProbeIdentity(platform, accountType string) bool {
	if accountType != AccountTypeAPIKey {
		return false
	}
	switch platform {
	case PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo,
		PlatformTypeSafe:
		return true
	}
	return false
}

func legacyIsHeaderOverrideEligible(platform, accountType string) bool {
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo:
		return accountType == AccountTypeAPIKey
	case PlatformGrok:
		return accountType == AccountTypeAPIKey || accountType == AccountTypeOAuth
	}
	return false
}

func legacyIsConcreteRequestPlatform(platform string) bool {
	for _, p := range legacyAllPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

func TestPlatformListDerivedListsMatchLegacy(t *testing.T) {
	require.Equal(t, legacyAllPlatforms, AllowedQuotaPlatforms)
	require.Equal(t, legacySchedulerSnapshotPlatforms, schedulerSnapshotPlatforms())
	require.Equal(t, legacyCompositeMatchingPlatforms, matchingPlatforms(PlatformComposite))
	require.Equal(t, []string{PlatformKimi}, matchingPlatforms(PlatformKimi))

	var multiProtocol []string
	for _, platform := range legacyAllPlatforms {
		if IsMultiProtocolAPIKeyProvider(platform) {
			multiProtocol = append(multiProtocol, platform)
		}
	}
	require.Equal(t, legacyMultiProtocolProviders, multiProtocol)
}

func TestPlatformListDerivedPredicatesMatchLegacy(t *testing.T) {
	for _, platform := range platformProbeValues {
		require.Equal(t, legacyIsCNProvider(platform), IsCNProvider(platform), platform)
		require.Equal(t, legacyIsConcreteRequestPlatform(platform), isConcreteRequestPlatform(platform), platform)
		require.Equal(t, legacyNormalizeOpenAICompatiblePlatform(platform), NormalizeOpenAICompatiblePlatform(platform), platform)
		require.Equal(t, legacyIsOpenAICompatible(platform), (&Account{Platform: platform}).IsOpenAICompatible(), platform)
		for _, accountType := range platformProbeAccountTypes {
			require.Equal(t, legacyIsUpstreamBillingProbeIdentity(platform, accountType),
				IsUpstreamBillingProbeIdentity(platform, accountType), "%s/%s", platform, accountType)
			require.Equal(t, legacyIsHeaderOverrideEligible(platform, accountType),
				(&Account{Platform: platform, Type: accountType}).IsHeaderOverrideEligible(), "%s/%s", platform, accountType)
		}
	}
}

// 多协议 API Key 供应商经 OpenAI 网关转发（NormalizeOpenAICompatiblePlatform、
// countTokens 路由等依赖此约束），ProviderProfile 登记的平台必须在平台清单中声明 openai 网关。
func TestProviderProfilesUseOpenAIGateway(t *testing.T) {
	for platform := range providerProfiles {
		spec, ok := domain.LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, domain.PlatformGatewayOpenAI, spec.Gateway, platform)
	}
}
