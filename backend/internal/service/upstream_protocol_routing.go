package service

import (
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// resolveUpstreamProtocol 决定 OpenAI 网关本次请求发往上游的协议
// （chat_completions / responses / anthropic），是 /v1/responses、
// /v1/chat/completions、/v1/messages 三个入站入口共用的唯一判定。
// 只做判断，不改写请求体、不发请求；各入口再按「入站 × 上游」选择转换路径。
//
// inbound 为入站协议；/v1/chat/completions 收到 Responses 形状请求体时按
// responses 计。model 仅按模型分流的账号使用，取 upstreamRoutingModel 的结果。
//
// 优先级：
//  1. 按模型分流的供应商（多模型聚合平台，如 OpenCode）：显式 api_protocol →
//     账号 protocol_rules → profile 内置规则 → Chat Completions，与入站协议无关。
//  2. adaptive（按入站协议分流）：供应商提供入站协议的原生端点则走同协议，
//     否则回落 Chat Completions。
//  3. 显式 anthropic 协议 → Anthropic。
//  4. shouldForwardOpenAIResponsesViaRawChatCompletions（显式 chat_completions、
//     探针确认不支持 Responses 等）→ Chat Completions。
//  5. 其余 → Responses。
//
// Grok 在 Responses / Chat Completions 入口有专属链路，须在调用本函数前分流。
func resolveUpstreamProtocol(account *Account, inbound, model string) string {
	if account.routesByModel() {
		return account.resolveModelRoutedProtocol(model)
	}
	if account.IsAdaptiveAPIProtocol() {
		if account.providerSupportsProtocol(inbound) {
			return inbound
		}
		return APIProtocolChatCompletions
	}
	if account.IsAnthropicProtocol() {
		return APIProtocolAnthropic
	}
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return APIProtocolChatCompletions
	}
	return APIProtocolResponses
}

// resolveModelRoutedProtocol 按模型名解析按模型分流账号的上游协议。
// 显式 pinned 协议优先；adaptive 先走 credentials.protocol_rules，未配置时回落
// profile 对应接入模式的内置规则；规则未命中或结果未知一律 Chat Completions，
// 避免落入 Responses 转换链。
func (a *Account) resolveModelRoutedProtocol(model string) string {
	switch proto := a.GetAPIProtocol(); proto {
	case APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolResponses:
		return proto
	}
	rules, present := a.configuredProtocolRules()
	if !present {
		rules = a.providerProfile().Endpoints(a.GetCredential("account_mode")).ProtocolRules
	}
	return matchProtocolRules(model, rules)
}

// upstreamRoutingModel 返回参与协议分流的模型名：仅按模型分流的账号需要，按账号
// 模型映射与上游归一后的名字匹配规则；其余账号的分流与模型无关，返回空串。
func upstreamRoutingModel(account *Account, body []byte, defaultMappedModel string) string {
	if !account.routesByModel() {
		return ""
	}
	return resolveMappedUpstreamModel(account, body, defaultMappedModel)
}

// convertResponsesShapedChatBody 把发到 /v1/chat/completions 的 Responses 形状
// 请求体（Cursor 兼容）转换为 Chat Completions 请求体。
func (s *OpenAIGatewayService) convertResponsesShapedChatBody(body []byte) ([]byte, error) {
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		return nil, fmt.Errorf("parse responses-shaped chat completions request: %w", err)
	}
	chatReq, err := apicompat.ResponsesToChatCompletionsRequestWithOptions(
		&responsesReq,
		&apicompat.ResponsesToChatOptions{ReasoningContentByID: s.reasoningContentByID},
	)
	if err != nil {
		return nil, fmt.Errorf("convert responses-shaped chat completions request: %w", err)
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal converted chat completions request: %w", err)
	}
	return chatBody, nil
}
