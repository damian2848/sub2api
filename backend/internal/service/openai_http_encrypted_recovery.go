package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIHTTPEncryptedRecoveryKey = "openai_http_encrypted_recovery"

// One state is shared by HTTP errors and SSE failures within a forward attempt.
// The retry remains in that attempt's loop, so it cannot switch accounts.
type openAIHTTPEncryptedRecovery struct {
	body      []byte
	entryBody []byte
	attempted bool
	disabled  bool
}

type openAIHTTPEncryptedRecoverySignal struct {
	body    []byte
	digests []string
}

func (e *openAIHTTPEncryptedRecoverySignal) Error() string {
	return "upstream encrypted reasoning rejected before output"
}

func asOpenAIHTTPEncryptedRecoverySignal(err error) (*openAIHTTPEncryptedRecoverySignal, bool) {
	var signal *openAIHTTPEncryptedRecoverySignal
	ok := errors.As(err, &signal)
	return signal, ok && signal != nil
}

func beginOpenAIHTTPEncryptedRecovery(ctx context.Context, c *gin.Context, account *Account, body []byte) *openAIHTTPEncryptedRecovery {
	state := &openAIHTTPEncryptedRecovery{
		body: body, entryBody: body,
		disabled: isControlledExperiment(ctx) || account == nil || account.Platform != PlatformOpenAI || account.IsCopilotSDKEnabled(),
	}
	c.Set(openAIHTTPEncryptedRecoveryKey, state)
	return state
}

func isOpenAIEncryptedContentRejection(payload []byte) bool {
	code := strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String())
	if code == "" {
		code = extractUpstreamErrorCode(payload)
	}
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(payload, "code").String())
	}
	message := extractOpenAISSEErrorMessage(payload)
	return code == "invalid_encrypted_content" ||
		(code == "thinking_signature_invalid" &&
			strings.Contains(message, "The encrypted content") &&
			strings.Contains(message, "could not be verified") &&
			strings.Contains(message, "could not be decrypted or parsed"))
}

// Only direct reasoning ciphertext is recoverable. Compaction may contain the
// only remaining task history, so never remove it to make a request pass. Mixed
// requests with other encrypted content cannot safely be attributed to reasoning.
func openAIHTTPEncryptedInputRecoverable(body []byte) bool {
	root := gjson.ParseBytes(body)
	input := root.Get("input")
	found := false
	var inspect func(gjson.Result, bool) bool
	inspect = func(value gjson.Result, directInput bool) bool {
		if !value.IsObject() && !value.IsArray() {
			return true
		}
		switch strings.TrimSpace(value.Get("type").String()) {
		case "compaction", "compaction_summary", "encrypted_content":
			return false
		}
		if len(value.Get("encrypted_function_args").Array()) > 0 {
			return false
		}
		if directInput {
			digests := make(map[string]struct{})
			openAICollectEncryptedItemDigests(digests, value)
			if len(digests) > 0 {
				return false
			}
		}
		valid := true
		value.ForEach(func(key, child gjson.Result) bool {
			if key.String() == "encrypted_content" {
				if !directInput || strings.TrimSpace(value.Get("type").String()) != "reasoning" ||
					child.Type != gjson.String || child.String() == "" {
					valid = false
					return false
				}
				found = true
				return true
			}
			valid = inspect(child, false)
			return valid
		})
		return valid
	}
	valid := true
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			valid = inspect(item, true)
			return valid
		})
	} else {
		valid = inspect(input, true)
	}
	return valid && found
}

// amsg carries task content, not expendable reasoning. Its failure must not
// trigger reasoning cleanup or poison the reasoning ciphertext lineage.
func openAIEncryptedReasoningRecoverySafe(body []byte, diagnostic string) bool {
	return !strings.Contains(strings.ToLower(diagnostic), "amsg_") && openAIHTTPEncryptedInputRecoverable(body)
}

// The failure itself can be the only frame that reports completed work. Retrying
// it would replay that work even when no earlier delta reached the client.
func openAIEncryptedRejectionHasOutput(payload []byte) bool {
	for _, path := range []string{"response.output", "output"} {
		value := gjson.GetBytes(payload, path)
		if value.IsArray() {
			if len(value.Array()) > 0 {
				return true
			}
		} else if value.Exists() && value.Type != gjson.Null {
			// Non-array output is malformed, not evidence that the turn was empty.
			return true
		}
	}
	for _, prefix := range []string{"response.usage.", "usage.", ""} {
		for _, field := range []string{"output_tokens", "reasoning_tokens", "output_tokens_details.reasoning_tokens", "completion_tokens", "completion_tokens_details.reasoning_tokens"} {
			if gjson.GetBytes(payload, prefix+field).Float() > 0 {
				return true
			}
		}
	}
	return false
}

// A Codex failure can span several frames (a bare error, then response.failed).
// Every frame of the failure is passed so that any one reporting completed work
// or naming task ciphertext blocks the replay.
func (state *openAIHTTPEncryptedRecovery) prepare(payloads ...[]byte) *openAIHTTPEncryptedRecoverySignal {
	if state == nil || state.disabled || state.attempted {
		return nil
	}
	rejected := false
	for _, payload := range payloads {
		if openAIEncryptedRejectionHasOutput(payload) || !openAIEncryptedReasoningRecoverySafe(state.body, string(payload)) {
			return nil
		}
		rejected = rejected || isOpenAIEncryptedContentRejection(payload)
	}
	if !rejected {
		return nil
	}
	var decoded map[string]any
	if decodeOpenAIJSONUseNumber(state.body, &decoded) != nil || !trimOpenAIEncryptedReasoningItems(decoded) {
		return nil
	}
	body, err := marshalOpenAIUpstreamJSON(decoded)
	if err != nil {
		return nil
	}
	state.attempted = true
	return &openAIHTTPEncryptedRecoverySignal{
		body:    body,
		digests: collectOpenAIEncryptedContentDigestsRaw(state.body),
	}
}

func newOpenAIHTTPEncryptedRecoverySignal(c *gin.Context, payload []byte, outputStarted bool) error {
	return newOpenAIHTTPEncryptedFailureRecoverySignal(c, outputStarted, payload)
}

func newOpenAIHTTPEncryptedFailureRecoverySignal(c *gin.Context, outputStarted bool, payloads ...[]byte) error {
	if c == nil || outputStarted || openAIStreamClientOutputStarted(c, false) {
		return nil
	}
	value, _ := c.Get(openAIHTTPEncryptedRecoveryKey)
	state, _ := value.(*openAIHTTPEncryptedRecovery)
	if signal := state.prepare(payloads...); signal != nil {
		return signal
	}
	return nil
}

func (s *OpenAIGatewayService) applyOpenAIHTTPEncryptedRecovery(c *gin.Context, account *Account, resp *http.Response, state *openAIHTTPEncryptedRecovery, signal *openAIHTTPEncryptedRecoverySignal, passthrough bool) []byte {
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	s.markOpenAIWSInvalidEncryptedContentLineage(
		getOpenAIGroupIDFromContext(c), s.openAIWSLineageSessionHashFromContext(c, state.entryBody), signal.digests,
	)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "retry", Reason: "invalid_encrypted_content", Message: "Retrying once after encrypted reasoning rejection", Passthrough: passthrough,
	})
	logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying HTTP request once after encrypted reasoning rejection (account_id: %d, passthrough: %v)", account.ID, passthrough)
	return signal.body
}

// A buffered SSE response may already contain a tool invocation even though no
// bytes reached a non-streaming client. Such a turn is not safe to replay.
func openAISSEBodyHasSemanticOutput(body string) bool {
	eventType := ""
	for body != "" {
		line, rest, _ := strings.Cut(body, "\n")
		body = rest
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			eventType = ""
			continue
		}
		if event, ok := extractOpenAISSEEventLine(line); ok {
			eventType = event
		}
		if data, ok := extractOpenAISSEDataLine(line); ok {
			event := effectiveOpenAISSEEventType([]byte(data), eventType)
			if event == "error" || event == "response.failed" || strings.TrimSpace(data) == "[DONE]" {
				continue
			}
			if openAIStreamDataStartsClientOutput(data, event) {
				return true
			}
		}
	}
	return false
}
