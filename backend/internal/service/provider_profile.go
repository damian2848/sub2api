package service

import "strings"

// ProviderEndpoints 是多协议 API Key 供应商在某个接入模式下的官方默认端点与
// 内置分流规则。账号凭证里的 base_url / api_base_urls / protocol_rules 仍优先于这里。
type ProviderEndpoints struct {
	// ChatBaseURL 供 Chat Completions / Responses / models 共用。
	ChatBaseURL string
	// AnthropicBaseURL 是 Anthropic 协议基址，上游路径为 {base}/v1/messages。
	AnthropicBaseURL string
	// ProtocolRules 是按模型分流的内置默认表（首条命中生效）；为空表示该模式
	// 不按模型分流，由账号 api_protocol 决定上游协议。
	ProtocolRules []OpenCodeGoProtocolRule
}

// ProviderProfile 描述一个多协议 API Key 供应商（国产厂商或多模型聚合平台）的
// 静态数据：各接入模式的默认端点、能力开关与内置分流规则。
// 额度解析、会话头等行为差异不在这里，仍由各自的代码路径处理。
type ProviderProfile struct {
	Platform string
	// DefaultMode 在 credentials.account_mode 缺失或不在 Modes 中时使用。
	DefaultMode string
	Modes       map[string]ProviderEndpoints
	// NativeResponses 表示上游提供原生 Responses 端点。
	NativeResponses bool
}

// Endpoints 返回指定接入模式的端点；未知模式回落 DefaultMode。
func (p *ProviderProfile) Endpoints(mode string) ProviderEndpoints {
	if p == nil {
		return ProviderEndpoints{}
	}
	if endpoints, ok := p.Modes[strings.TrimSpace(mode)]; ok {
		return endpoints
	}
	return p.Modes[p.DefaultMode]
}

// DefaultBaseURL 返回指定接入模式与上游协议的官方默认基址；协议未知时为空串。
func (p *ProviderProfile) DefaultBaseURL(mode, protocol string) string {
	endpoints := p.Endpoints(mode)
	switch protocol {
	case APIProtocolAnthropic:
		return endpoints.AnthropicBaseURL
	case APIProtocolChatCompletions, APIProtocolResponses:
		return endpoints.ChatBaseURL
	default:
		return ""
	}
}

// providerProfiles 是多协议 API Key 供应商的单一数据来源。新增同类供应商时在此
// 登记一条即可获得默认端点、Responses 能力判定与 IsMultiProtocolAPIKeyProvider 归属。
// 默认端点需与前端 credentialsBuilder.ts 的预设保持一致。
var providerProfiles = map[string]*ProviderProfile{
	PlatformKimi: {
		Platform:        PlatformKimi,
		DefaultMode:     AccountModePayG,
		NativeResponses: true,
		Modes: map[string]ProviderEndpoints{
			AccountModePayG: {
				ChatBaseURL:      DefaultKimiPayGBaseURL,
				AnthropicBaseURL: DefaultKimiPayGAnthropicBaseURL,
			},
			AccountModeCoding: {
				ChatBaseURL:      DefaultKimiCodingBaseURL,
				AnthropicBaseURL: DefaultKimiCodingAnthropicBaseURL,
			},
		},
	},
	PlatformZhipu: {
		Platform:    PlatformZhipu,
		DefaultMode: AccountModePayG,
		Modes: map[string]ProviderEndpoints{
			AccountModePayG: {
				ChatBaseURL:      DefaultZhipuPayGBaseURL,
				AnthropicBaseURL: DefaultZhipuAnthropicBaseURL,
			},
			AccountModeCoding: {
				ChatBaseURL:      DefaultZhipuCodingBaseURL,
				AnthropicBaseURL: DefaultZhipuAnthropicBaseURL,
			},
		},
	},
	PlatformDeepseek: {
		Platform:        PlatformDeepseek,
		DefaultMode:     AccountModePayG,
		NativeResponses: true,
		Modes: map[string]ProviderEndpoints{
			AccountModePayG: {
				ChatBaseURL:      DefaultDeepseekBaseURL,
				AnthropicBaseURL: DefaultDeepseekAnthropicBaseURL,
			},
		},
	},
	PlatformMiniMax: {
		Platform:        PlatformMiniMax,
		DefaultMode:     AccountModePayG,
		NativeResponses: true,
		Modes: map[string]ProviderEndpoints{
			// 按量付费与 Coding/Token Plan 共用推理域名，靠 API Key 区分套餐。
			AccountModePayG: {
				ChatBaseURL:      DefaultMiniMaxBaseURL,
				AnthropicBaseURL: DefaultMiniMaxAnthropicBaseURL,
			},
			AccountModeCoding: {
				ChatBaseURL:      DefaultMiniMaxBaseURL,
				AnthropicBaseURL: DefaultMiniMaxAnthropicBaseURL,
			},
		},
	},
	PlatformOpenCodeGo: {
		Platform:        PlatformOpenCodeGo,
		DefaultMode:     AccountModeGo,
		NativeResponses: true,
		Modes: map[string]ProviderEndpoints{
			AccountModeGo: {
				ChatBaseURL:      DefaultOpenCodeGoBaseURL,
				AnthropicBaseURL: DefaultOpenCodeGoAnthropicBaseURL,
				ProtocolRules:    DefaultOpenCodeGoProtocolRules(),
			},
			AccountModeZen: {
				ChatBaseURL:      DefaultOpenCodeZenBaseURL,
				AnthropicBaseURL: DefaultOpenCodeZenAnthropicBaseURL,
				ProtocolRules:    DefaultOpenCodeZenProtocolRules(),
			},
		},
	},
}

// LookupProviderProfile 返回 platform 对应的多协议供应商 profile；非此类平台返回 nil。
func LookupProviderProfile(platform string) *ProviderProfile {
	return providerProfiles[platform]
}

func (a *Account) providerProfile() *ProviderProfile {
	if a == nil {
		return nil
	}
	return LookupProviderProfile(a.Platform)
}

// defaultProviderBaseURL 按账号 account_mode 返回多协议供应商指定协议的官方默认基址；
// 非多协议供应商或协议未知时为空串。
func (a *Account) defaultProviderBaseURL(protocol string) string {
	profile := a.providerProfile()
	if profile == nil {
		return ""
	}
	return profile.DefaultBaseURL(a.GetCredential("account_mode"), protocol)
}
