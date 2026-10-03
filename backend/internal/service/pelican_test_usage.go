package service

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Collect only group-test requests. Each sample owns its collector, and each
// upstream stream owns a snapshot so cumulative usage is never added twice.
type pelicanTestUsageKey struct{}

type pelicanTestUsageCollector struct {
	activeHTTPRequests            map[*http.Request]bool
	model                         string
	requestedModel                string
	runID, logicalRequestID, role string
	groupID                       *int64
	account                       *Account
	recorder                      ProbeRequestFactRecorder
	requests                      []*pelicanTestUsage
}

type pelicanTestUsage struct {
	attemptID                          string
	startedAt, finishedAt              time.Time
	httpStatus                         *int
	errorKind                          *string
	firstTokenMs                       *int64
	apiFailed, networkAttempt, canTTFT bool
	protocol                           string
	model                              string
	serviceTier                        string
	tokens                             UsageTokens
	input                              int
	output                             int
	thoughts                           int
	reasoning                          int
	seen                               bool
	inputSeen                          bool
	outputSeen                         bool
	complete                           bool
}

func startPelicanTestStream(c *gin.Context, protocol string) *pelicanTestUsage {
	if c.Request == nil {
		return nil
	}
	return startPelicanTestUsage(c.Request.Context(), protocol)
}

func recordPelicanTestSSE(ctx context.Context, protocol, model string, body []byte) {
	var u *pelicanTestUsage
	// Buffered adapters already passed through the transport observation. Reuse
	// that completed attempt instead of charging its cumulative usage twice.
	if collector := pelicanUsageFromContext(ctx); collector != nil && len(collector.requests) > 0 {
		latest := collector.requests[len(collector.requests)-1]
		if latest.networkAttempt {
			u = latest
			u.protocol = protocol
		}
	}
	if u == nil {
		u = startPelicanTestUsage(ctx, protocol)
	}
	if u == nil {
		return
	}
	u.model = model
	for _, line := range strings.Split(string(body), "\n") {
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			u.read(strings.TrimSpace(data))
		}
	}
}

func pelicanUsageFromContext(ctx context.Context) *pelicanTestUsageCollector {
	u, _ := ctx.Value(pelicanTestUsageKey{}).(*pelicanTestUsageCollector)
	return u
}

func startPelicanTestUsage(ctx context.Context, protocol string) *pelicanTestUsage {
	collector := pelicanUsageFromContext(ctx)
	if collector == nil {
		return nil
	}
	// HTTP observations start before the network call, so the response parser
	// enriches that same attempt rather than inventing a second request.
	if n := len(collector.requests); n > 0 {
		latest := collector.requests[n-1]
		if latest.networkAttempt && latest.finishedAt.IsZero() {
			latest.protocol = protocol
			return latest
		}
	}
	u := &pelicanTestUsage{protocol: protocol, model: collector.model, attemptID: uuid.NewString(), startedAt: time.Now()}
	collector.requests = append(collector.requests, u)
	return u
}

func (u *pelicanTestUsage) read(raw string) {
	if u == nil {
		return
	}
	data := gjson.Parse(raw)
	eventType := data.Get("type").String()
	if eventType == "error" || eventType == "response.failed" || eventType == "response.incomplete" || pelicanHasAPIError(data.Get("error")) || pelicanHasAPIError(data.Get("response.error")) {
		u.apiFailed = true
	}
	if u.canTTFT && u.firstTokenMs == nil && pelicanUsageHasOutput(data, u.protocol) {
		ms := nonnegativeProbeDuration(time.Since(u.startedAt).Milliseconds())
		u.firstTokenMs = &ms
	}
	if raw == "[DONE]" && (u.protocol == "chat" || u.protocol == "gemini") {
		u.complete = true
	}
	if u.protocol == "anthropic" && data.Get("stop_reason").String() != "" {
		u.complete = true
	}
	if data.Get("status").String() == "completed" {
		u.complete = true
	}
	if eventType == "message_stop" || eventType == "response.completed" || eventType == "response.done" || eventType == "response.failed" || eventType == "response.incomplete" {
		u.complete = true
	}
	if response := data.Get("response"); response.IsObject() {
		data = response
	} else if message := data.Get("message"); message.IsObject() {
		data = message
	}
	if model := strings.TrimSpace(data.Get("model").String()); model != "" {
		u.model = model
	}
	if tier := data.Get("service_tier").String(); tier != "" {
		u.serviceTier = tier
	}
	usage := data.Get("usage")
	if u.protocol == "gemini" {
		usage = data.Get("usageMetadata")
		for _, candidate := range data.Get("candidates").Array() {
			if candidate.Get("finishReason").String() != "" {
				u.complete = true
			}
		}
	}
	for _, choice := range data.Get("choices").Array() {
		if choice.Get("finish_reason").String() != "" {
			u.complete = true
		}
	}
	if !usage.IsObject() {
		return
	}
	set := func(field string, value *int) {
		if n := usage.Get(field); n.Type == gjson.Number && n.Float() >= 0 {
			*value = int(n.Int())
			u.seen = true
			if value == &u.input {
				u.inputSeen = true
			}
			if value == &u.output {
				u.outputSeen = true
			}
		}
	}
	switch u.protocol {
	case "anthropic":
		set("input_tokens", &u.input)
		set("output_tokens", &u.output)
		set("cache_read_input_tokens", &u.tokens.CacheReadTokens)
		set("cache_creation_input_tokens", &u.tokens.CacheCreationTokens)
		set("cache_creation.ephemeral_5m_input_tokens", &u.tokens.CacheCreation5mTokens)
		set("cache_creation.ephemeral_1h_input_tokens", &u.tokens.CacheCreation1hTokens)
	case "gemini":
		set("promptTokenCount", &u.input)
		set("candidatesTokenCount", &u.output)
		set("thoughtsTokenCount", &u.thoughts)
		u.reasoning = u.thoughts
		set("cachedContentTokenCount", &u.tokens.CacheReadTokens)
	case "chat":
		set("prompt_tokens", &u.input)
		set("completion_tokens", &u.output)
		set("prompt_tokens_details.cached_tokens", &u.tokens.CacheReadTokens)
		set("prompt_cache_hit_tokens", &u.tokens.CacheReadTokens)
		set("completion_tokens_details.reasoning_tokens", &u.reasoning)
	default:
		set("input_tokens", &u.input)
		set("output_tokens", &u.output)
		set("input_tokens_details.cached_tokens", &u.tokens.CacheReadTokens)
		set("cache_read_input_tokens", &u.tokens.CacheReadTokens)
		set("cache_creation_input_tokens", &u.tokens.CacheCreationTokens)
		set("output_tokens_details.reasoning_tokens", &u.reasoning)
	}
	u.tokens.InputTokens = u.input
	if u.protocol != "anthropic" {
		// OpenAI/Chat/Gemini input totals already include cache hits.
		u.tokens.InputTokens = max(0, u.input-u.tokens.CacheReadTokens-u.tokens.CacheCreationTokens)
	}
	u.tokens.OutputTokens = u.output + u.thoughts
}

func (c *pelicanTestUsageCollector) snapshot() *ChannelMonitorProbeUsage {
	usage := &ChannelMonitorProbeUsage{Source: "probe", RequestCount: int64(len(c.requests)), UsageIncomplete: len(c.requests) == 0}
	for _, request := range c.requests {
		usage.InputTokens += int64(request.tokens.InputTokens)
		usage.OutputTokens += int64(request.tokens.OutputTokens)
		usage.CacheReadTokens += int64(request.tokens.CacheReadTokens)
		usage.CacheCreationTokens += int64(request.tokens.CacheCreationTokens)
		usage.CacheCreation5mTokens += int64(request.tokens.CacheCreation5mTokens)
		usage.CacheCreation1hTokens += int64(request.tokens.CacheCreation1hTokens)
		usage.ReasoningTokens += int64(request.reasoning)
		usage.UsageIncomplete = usage.UsageIncomplete || !request.seen || !request.complete || !request.inputSeen || !request.outputSeen
	}
	return usage
}

func (c *pelicanTestUsageCollector) upstreamModel() string {
	if len(c.requests) > 0 {
		return c.requests[len(c.requests)-1].model
	}
	return c.model
}

func (c *pelicanTestUsageCollector) probeUsage(billing *BillingService, account *Account) ChannelMonitorProbeUsage {
	usage := c.snapshot()
	usage.CostUSD, usage.CostIncomplete = c.cost(billing, account)
	return *usage
}

// Return a cost snapshot, not a debit. Use the account's upstream cost multiplier,
// never the customer/group selling multiplier. Missing usage/pricing remains
// unknown; a partially metered attempt must not discard other known costs.
func (c *pelicanTestUsageCollector) cost(billing *BillingService, account *Account) (*float64, bool) {
	var total float64
	priced, incomplete := false, len(c.requests) == 0
	for _, u := range c.requests {
		if !u.seen || billing == nil {
			incomplete = true
			continue
		}
		cost, err := billing.CalculateCostWithServiceTier(u.model, u.tokens, 1, u.serviceTier)
		if err != nil || cost == nil {
			incomplete = true
			continue
		}
		multiplier := 1.0
		if account != nil {
			multiplier = account.CostMultiplier()
		}
		amount := cost.TotalCost * multiplier
		if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
			incomplete = true
			continue
		}
		total += amount
		priced = true
		incomplete = incomplete || !u.complete || !u.inputSeen || !u.outputSeen
	}
	if !priced {
		return nil, true
	}
	return &total, incomplete
}

// Only actual output establishes TTFT; role/usage/headers and terminal events do
// not. Non-streaming readers never enable canTTFT.
func pelicanUsageHasOutput(data gjson.Result, protocol string) bool {
	if response := data.Get("response"); response.IsObject() {
		data = response
	}
	switch protocol {
	case "anthropic":
		return data.Get("type").String() == "content_block_delta" && (data.Get("delta.text").String() != "" || data.Get("delta.thinking").String() != "" || data.Get("delta.partial_json").String() != "")
	case "chat":
		for _, choice := range data.Get("choices").Array() {
			if choice.Get("delta.content").String() != "" || choice.Get("message.content").String() != "" {
				return true
			}
		}
	case "gemini":
		for _, candidate := range data.Get("candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				if part.Get("text").String() != "" || part.Get("inlineData.data").String() != "" {
					return true
				}
			}
		}
	default:
		return strings.HasPrefix(data.Get("type").String(), "response.") && strings.HasSuffix(data.Get("type").String(), ".delta") && data.Get("delta").String() != ""
	}
	return false
}

func (c *pelicanTestUsageCollector) facts(billing *BillingService, account *Account) []ProbeRequestFact {
	facts := make([]ProbeRequestFact, 0, len(c.requests))
	if c.runID == "" {
		c.runID = uuid.NewString()
	}
	if c.logicalRequestID == "" {
		c.logicalRequestID = uuid.NewString()
	}
	role := c.role
	if role == "" {
		role = "account_check"
	}
	for _, u := range c.requests {
		if !u.networkAttempt || account == nil {
			continue
		}
		finished := u.finishedAt
		if finished.IsZero() {
			finished = time.Now()
		}
		usageComplete := u.seen && u.complete && u.inputSeen && u.outputSeen
		cost, partial := (&pelicanTestUsageCollector{requests: []*pelicanTestUsage{u}}).cost(billing, account)
		f := ProbeRequestFact{
			RunID: c.runID, LogicalRequestID: c.logicalRequestID, AttemptID: u.attemptID,
			Role: role, Platform: strings.ToLower(account.Platform), GroupID: c.groupID, AccountID: account.ID,
			Protocol: u.protocol, RequestedModel: c.requestedModel, UpstreamModel: u.model,
			StartedAt: u.startedAt, FinishedAt: finished, DurationMs: nonnegativeProbeDuration(finished.Sub(u.startedAt).Milliseconds()),
			FirstTokenMs: u.firstTokenMs, HTTPStatus: u.httpStatus,
			APISuccess: u.errorKind == nil && !u.apiFailed && u.complete,
			ErrorKind:  u.errorKind, UsageComplete: usageComplete, UpstreamCostUSD: cost, CostComplete: !partial,
		}
		if f.RequestedModel == "" {
			f.RequestedModel = c.model
			if f.RequestedModel == "" {
				f.RequestedModel = u.model
			}
		}
		if f.ErrorKind == nil && !f.APISuccess {
			kind := "stream"
			f.ErrorKind = &kind
		}
		// Missing usage remains NULL, not a fabricated zero.
		if u.inputSeen {
			n := int64(u.tokens.InputTokens)
			f.InputTokens = &n
		}
		if u.outputSeen {
			n := int64(u.tokens.OutputTokens)
			f.OutputTokens = &n
		}
		if u.seen {
			n := int64(u.tokens.CacheReadTokens)
			f.CacheReadTokens = &n
			n = int64(u.tokens.CacheCreationTokens)
			f.CacheCreationTokens = &n
			n = int64(u.tokens.CacheCreation5mTokens)
			f.CacheCreation5mTokens = &n
			n = int64(u.tokens.CacheCreation1hTokens)
			f.CacheCreation1hTokens = &n
			n = int64(u.reasoning)
			f.ReasoningTokens = &n
		}
		facts = append(facts, f)
	}
	return facts
}

func nonnegativeProbeDuration(ms int64) int64 {
	if ms < 0 {
		return 0
	}
	return ms
}

func pelicanHasAPIError(value gjson.Result) bool {
	return value.Exists() && value.Type != gjson.Null
}
