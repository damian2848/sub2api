package service

import "strings"

// ProviderEndpoints 是多协议 API Key 供应商在某个接入模式下的官方默认端点与
// 内置分流规则。账号凭证里的 base_url / api_base_urls / protocol_rules 仍优先于这里。
type ProviderEndpoints struct {
	// BaseURLs 以上游协议（chat_completions / responses / anthropic）为键，
	// 值为该协议的官方默认基址。缺少某协议的条目即表示该模式不提供该原生端点。
	// Anthropic 基址的上游路径为 {base}/v1/messages。
	BaseURLs map[string]string
	// ProtocolRules 是按模型分流的内置默认表（首条命中生效）；为空表示该模式
	// 不按模型分流，由账号 api_protocol 决定上游协议。
	ProtocolRules []OpenCodeGoProtocolRule
}

// ProviderProfile 描述一个多协议 API Key 供应商（国产厂商或多模型聚合平台）的
// 静态数据：各接入模式的默认端点、内置分流规则与端点路径约定。
// 额度解析、会话头等行为差异不在这里，仍由各自的代码路径处理。
type ProviderProfile struct {
	Platform string
	// DefaultMode 在 credentials.account_mode 缺失或不在 Modes 中时使用。
	DefaultMode string
	Modes       map[string]ProviderEndpoints
	// ResponsesPath 覆盖 Responses 端点路径（按 buildOpenAIEndpointURL 的版本感知
	// 规则拼接），空值为 /v1/responses。对自定义 base_url 的账号同样生效。
	ResponsesPath string
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

// DefaultBaseURL 返回指定接入模式与上游协议的官方默认基址；该模式不提供此协议时为空串。
func (p *ProviderProfile) DefaultBaseURL(mode, protocol string) string {
	return p.Endpoints(mode).BaseURLs[protocol]
}

// SupportsProtocol 报告指定接入模式是否提供该协议的原生端点。
func (p *ProviderProfile) SupportsProtocol(mode, protocol string) bool {
	return p.DefaultBaseURL(mode, protocol) != ""
}

// providerProfiles 是多协议 API Key 供应商的单一数据来源。新增同类供应商时在此
// 登记一条即可获得默认端点、原生 Responses 能力判定（有 responses 条目即支持）、
// Responses 路径约定与 IsMultiProtocolAPIKeyProvider 归属。
// 默认端点需与前端 credentialsBuilder.ts 的预设保持一致。
var providerProfiles = map[string]*ProviderProfile{
	PlatformKimi: {
		Platform:    PlatformKimi,
		DefaultMode: AccountModePayG,
		Modes: map[string]ProviderEndpoints{
			AccountModePayG: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultKimiPayGBaseURL,
					APIProtocolResponses:       DefaultKimiPayGBaseURL,
					APIProtocolAnthropic:       DefaultKimiPayGAnthropicBaseURL,
				},
			},
			AccountModeCoding: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultKimiCodingBaseURL,
					APIProtocolResponses:       DefaultKimiCodingBaseURL,
					APIProtocolAnthropic:       DefaultKimiCodingAnthropicBaseURL,
				},
			},
		},
	},
	PlatformZhipu: {
		Platform:    PlatformZhipu,
		DefaultMode: AccountModePayG,
		// 智谱没有原生 Responses 端点，故不登记 responses 条目。
		Modes: map[string]ProviderEndpoints{
			AccountModePayG: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultZhipuPayGBaseURL,
					APIProtocolAnthropic:       DefaultZhipuAnthropicBaseURL,
				},
			},
			AccountModeCoding: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultZhipuCodingBaseURL,
					APIProtocolAnthropic:       DefaultZhipuAnthropicBaseURL,
				},
			},
		},
	},
	PlatformDeepseek: {
		Platform:    PlatformDeepseek,
		DefaultMode: AccountModePayG,
		// DeepSeek 官方 Responses 端点为 /responses（无 /v1 前缀，适配 Codex）。
		ResponsesPath: "/responses",
		Modes: map[string]ProviderEndpoints{
			AccountModePayG: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultDeepseekBaseURL,
					APIProtocolResponses:       DefaultDeepseekBaseURL,
					APIProtocolAnthropic:       DefaultDeepseekAnthropicBaseURL,
				},
			},
		},
	},
	PlatformMiniMax: {
		Platform:    PlatformMiniMax,
		DefaultMode: AccountModePayG,
		Modes: map[string]ProviderEndpoints{
			// 按量付费与 Coding/Token Plan 共用推理域名，靠 API Key 区分套餐。
			AccountModePayG: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultMiniMaxBaseURL,
					APIProtocolResponses:       DefaultMiniMaxBaseURL,
					APIProtocolAnthropic:       DefaultMiniMaxAnthropicBaseURL,
				},
			},
			AccountModeCoding: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultMiniMaxBaseURL,
					APIProtocolResponses:       DefaultMiniMaxBaseURL,
					APIProtocolAnthropic:       DefaultMiniMaxAnthropicBaseURL,
				},
			},
		},
	},
	PlatformOpenCodeGo: {
		Platform:    PlatformOpenCodeGo,
		DefaultMode: AccountModeGo,
		Modes: map[string]ProviderEndpoints{
			AccountModeGo: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultOpenCodeGoBaseURL,
					APIProtocolResponses:       DefaultOpenCodeGoBaseURL,
					APIProtocolAnthropic:       DefaultOpenCodeGoAnthropicBaseURL,
				},
				ProtocolRules: DefaultOpenCodeGoProtocolRules(),
			},
			AccountModeZen: {
				BaseURLs: map[string]string{
					APIProtocolChatCompletions: DefaultOpenCodeZenBaseURL,
					APIProtocolResponses:       DefaultOpenCodeZenBaseURL,
					APIProtocolAnthropic:       DefaultOpenCodeZenAnthropicBaseURL,
				},
				ProtocolRules: DefaultOpenCodeZenProtocolRules(),
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

// providerSupportsProtocol 报告账号所在供应商在其 account_mode 下是否提供该协议的原生端点。
func (a *Account) providerSupportsProtocol(protocol string) bool {
	profile := a.providerProfile()
	return profile != nil && profile.SupportsProtocol(a.GetCredential("account_mode"), protocol)
}
