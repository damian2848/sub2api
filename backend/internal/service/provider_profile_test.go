//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyProviderDefaultBaseURL 复述 provider profile 引入前 account.go 中逐平台
// switch 的默认端点逻辑，作为等价性基准。
func legacyProviderDefaultBaseURL(platform, mode, protocol string) string {
	coding := mode == AccountModeCoding
	zen := mode == AccountModeZen
	switch protocol {
	case APIProtocolAnthropic:
		switch platform {
		case PlatformKimi:
			if coding {
				return DefaultKimiCodingAnthropicBaseURL
			}
			return DefaultKimiPayGAnthropicBaseURL
		case PlatformZhipu:
			return DefaultZhipuAnthropicBaseURL
		case PlatformDeepseek:
			return DefaultDeepseekAnthropicBaseURL
		case PlatformMiniMax:
			return DefaultMiniMaxAnthropicBaseURL
		case PlatformOpenCodeGo:
			if zen {
				return DefaultOpenCodeZenAnthropicBaseURL
			}
			return DefaultOpenCodeGoAnthropicBaseURL
		}
	case APIProtocolChatCompletions, APIProtocolResponses:
		switch platform {
		case PlatformKimi:
			if coding {
				return DefaultKimiCodingBaseURL
			}
			return DefaultKimiPayGBaseURL
		case PlatformZhipu:
			if coding {
				return DefaultZhipuCodingBaseURL
			}
			return DefaultZhipuPayGBaseURL
		case PlatformDeepseek:
			return DefaultDeepseekBaseURL
		case PlatformMiniMax:
			return DefaultMiniMaxBaseURL
		case PlatformOpenCodeGo:
			if zen {
				return DefaultOpenCodeZenBaseURL
			}
			return DefaultOpenCodeGoBaseURL
		}
	}
	return ""
}

func TestProviderProfile_DefaultBaseURLsMatchLegacySwitches(t *testing.T) {
	t.Parallel()

	platforms := []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo}
	modes := []string{"", AccountModePayG, AccountModeCoding, AccountModeZen, AccountModeGo, " coding ", "unknown"}

	for _, platform := range platforms {
		for _, mode := range modes {
			legacyMode := mode
			if legacyMode == " coding " {
				legacyMode = AccountModeCoding
			}
			newAccount := func(protocol string) *Account {
				return &Account{
					Platform: platform,
					Type:     AccountTypeAPIKey,
					Credentials: map[string]any{
						"api_key":      "sk-test",
						"account_mode": mode,
						"api_protocol": protocol,
					},
				}
			}
			name := platform + "/" + mode

			chat := newAccount(APIProtocolChatCompletions)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolChatCompletions), chat.GetOpenAIBaseURL(), name)

			anthropic := newAccount(APIProtocolAnthropic)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolAnthropic), anthropic.GetAnthropicProtocolBaseURL(), name)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolChatCompletions), anthropic.GetOpenAIFormatBaseURL(), name)

			adaptive := newAccount(APIProtocolAdaptive)
			for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolResponses, APIProtocolAnthropic, "unknown"} {
				require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, protocol), adaptive.GetCNProtocolBaseURL(protocol), name+"/"+protocol)
			}
		}
	}
}

func TestProviderProfile_ProfilesMatchMultiProtocolPlatforms(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformComposite, "",
	} {
		want := IsCNProvider(platform) || platform == PlatformOpenCodeGo
		require.Equal(t, want, IsMultiProtocolAPIKeyProvider(platform), platform)
		require.Equal(t, want, LookupProviderProfile(platform) != nil, platform)
	}

	for platform, profile := range providerProfiles {
		require.Equal(t, platform, profile.Platform)
		_, ok := profile.Modes[profile.DefaultMode]
		require.True(t, ok, "%s default mode %q must be registered", platform, profile.DefaultMode)
	}
}

func TestProviderProfile_NativeResponsesMatchesLegacy(t *testing.T) {
	t.Parallel()

	legacy := map[string]bool{
		PlatformDeepseek:   true,
		PlatformKimi:       true,
		PlatformMiniMax:    true,
		PlatformOpenCodeGo: true,
	}
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey}
		require.Equal(t, legacy[platform], account.SupportsNativeCNResponses(), platform)
	}
	require.False(t, (*Account)(nil).SupportsNativeCNResponses())
}

func TestProviderProfile_OpenCodeProtocolRulesByMode(t *testing.T) {
	t.Parallel()

	require.Equal(t, DefaultOpenCodeGoProtocolRules(), defaultOpenCodeProtocolRules(AccountModeGo))
	require.Equal(t, DefaultOpenCodeGoProtocolRules(), defaultOpenCodeProtocolRules(""))
	require.Equal(t, DefaultOpenCodeZenProtocolRules(), defaultOpenCodeProtocolRules(AccountModeZen))
	require.Empty(t, LookupProviderProfile(PlatformKimi).Endpoints(AccountModeCoding).ProtocolRules)
}
