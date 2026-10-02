package service

import (
	"context"
	"encoding/json"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Requests a managed Prism account cannot serve. The reasons match the
// sidecar's own rejection codes and double as the client-facing error code once
// no other account in the group is left to try.
const (
	PrismNonTextInputReason                = GatewayFailureReason("image_input_not_supported")
	PrismPreviousResponseUnsupportedReason = GatewayFailureReason("previous_response_not_supported")
	PrismCompactUnsupportedReason          = GatewayFailureReason("compact_not_supported")
)

// Images and files are uploaded by the sidecar before the native Prism turn.
// Audio remains unsupported; keep the existing rejection code for clients.
var prismUnsupportedPartTypes = map[string]struct{}{
	"input_audio": {}, "audio": {},
}

// prismMaxPartDepth bounds the content scan: real parts sit three levels below
// input/messages, so anything deeper is not worth following.
const prismMaxPartDepth = 16

const prismStreamCommittedKey = "prism_stream_committed"

// MarkPrismStreamCommitted prevents replay once a Prism response ID is public.
// It is separate from the terminal-response marker so failures can still be sent.
func MarkPrismStreamCommitted(c *gin.Context) {
	if c != nil {
		c.Set(prismStreamCommittedKey, true)
	}
}

func IsPrismStreamCommitted(c *gin.Context) bool {
	return c != nil && c.GetBool(prismStreamCommittedKey)
}

func prismStreamLifecycleStartsOutput(account *Account, eventType string) bool {
	return account.IsManagedPrismAccount() && (eventType == "response.created" || eventType == "response.in_progress")
}

func openaiAccountUpstreamContext(ctx context.Context, account *Account) (context.Context, context.CancelFunc) {
	if account.IsManagedPrismAccount() {
		if ctx == nil {
			ctx = context.Background()
		}
		return ctx, func() {}
	}
	return detachUpstreamContext(ctx)
}

// isPrismWSHTTPBridge reports whether a managed Prism account serves WebSocket
// clients by bridging each turn to HTTP/SSE. The account's own mode decides, so
// the gateway-wide mode router flag does not matter: Prism has no WebSocket
// upstream that the legacy ctx_pool path could use.
func (a *Account) isPrismWSHTTPBridge() bool {
	return a.IsManagedPrismAccount() &&
		a.ResolveOpenAIResponsesWebSocketV2Mode(OpenAIWSIngressModeOff) == OpenAIWSIngressModeHTTPBridge
}

// prismWSIngressAvailable applies the gateway's WebSocket kill switches to the
// bridge: it is still WebSocket ingress for an API-key account.
func (s *OpenAIGatewayService) prismWSIngressAvailable() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	ws := s.cfg.Gateway.OpenAIWS
	return ws.Enabled && ws.APIKeyEnabled && !ws.ForceHTTP
}

// IsPrismRequestUnsupported reports whether a managed Prism account declined the
// request itself. Another account may still serve it and Prism is not at fault,
// so the failover loops must neither penalize nor cool down the account.
func (e *UpstreamFailoverError) IsPrismRequestUnsupported() bool {
	if e == nil {
		return false
	}
	switch e.Reason {
	case PrismNonTextInputReason, PrismPreviousResponseUnsupportedReason, PrismCompactUnsupportedReason:
		return true
	}
	return false
}

// PrismSessionUnavailableReason marks a managed Prism account whose browser
// session cannot serve requests right now (reconnecting, sidecar unreachable,
// source OAuth account unusable). Unlike a declined request this is an account
// fault: the account is cooled down and reported like any unavailable upstream.
const PrismSessionUnavailableReason = GatewayFailureReason("prism_session_unavailable")

// newPrismSessionUnavailableError turns a session failure into a failover error
// with account scope. Only the Prism reason code and its fixed message reach
// the ops log; the cause may carry internal addresses.
func newPrismSessionUnavailableError(cause error) *UpstreamFailoverError {
	reason, message := "PRISM_UPSTREAM_UNAVAILABLE", "The Prism browser service is unavailable"
	if infraerrors.Code(cause) != http.StatusInternalServerError {
		reason, message = infraerrors.Reason(cause), infraerrors.Message(cause)
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": reason, "message": message}})
	return &UpstreamFailoverError{
		StatusCode:        http.StatusServiceUnavailable,
		ResponseBody:      body,
		Stage:             GatewayFailureStageInference,
		Scope:             GatewayFailureScopeAccount,
		Reason:            PrismSessionUnavailableReason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusServiceUnavailable,
		ClientMessage:     message,
	}
}

func newPrismUnsupportedRequestError(reason GatewayFailureReason, message string) *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:        http.StatusBadRequest,
		Stage:             GatewayFailureStageInference,
		Scope:             GatewayFailureScopeRequest,
		Reason:            reason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusBadRequest,
		ClientMessage:     message,
	}
}

// prismUnsupportedRequest returns a failover error when body carries something
// the Prism sidecar rejects: audio input, previous_response_id
// or the compact endpoint. Everything else (instructions, history, tools and
// unknown parameters) is the sidecar's to accept or ignore. The error is
// returned before anything is written to the client, so the handler can move on
// to the next account.
func prismUnsupportedRequest(c *gin.Context, body []byte, chat bool) *UpstreamFailoverError {
	inputKey := "messages"
	if !chat {
		inputKey = "input"
		if isOpenAIResponsesCompactPath(c) {
			return newPrismUnsupportedRequestError(PrismCompactUnsupportedReason,
				"Prism accounts do not support /responses/compact")
		}
		if previous := gjson.GetBytes(body, "previous_response_id"); previous.Exists() && previous.Type != gjson.Null {
			return newPrismUnsupportedRequestError(PrismPreviousResponseUnsupportedReason,
				"Prism accounts do not support previous_response_id; send the full conversation in input")
		}
	}
	if prismHasUnsupportedPart(gjson.GetBytes(body, inputKey), 0) {
		return newPrismUnsupportedRequestError(PrismNonTextInputReason,
			"Prism accounts do not support audio input")
	}
	return nil
}

func prismHasUnsupportedPart(value gjson.Result, depth int) bool {
	isObject := value.IsObject()
	if depth > prismMaxPartDepth || !isObject && !value.IsArray() {
		return false
	}
	found := false
	value.ForEach(func(key, item gjson.Result) bool {
		switch {
		case item.IsObject() || item.IsArray():
			found = prismHasUnsupportedPart(item, depth+1)
		case isObject && item.Type == gjson.String && key.String() == "type":
			_, found = prismUnsupportedPartTypes[item.String()]
		}
		return !found
	})
	return found
}

// stripPrismServiceTier removes service_tier: Prism has no Fast tier, so a
// forwarded hint would be ignored upstream yet still billed as Priority.
func stripPrismServiceTier(body []byte) []byte {
	if !gjson.GetBytes(body, "service_tier").Exists() {
		return body
	}
	if stripped, err := sjson.DeleteBytes(body, "service_tier"); err == nil {
		return stripped
	}
	return body
}

// validatePrismGatewayRequest is the gateway's only gate in front of the
// sidecar. It must run before generic compatibility transforms can discard
// caller fields, and it never rewrites the body.
func validatePrismGatewayRequest(c *gin.Context, body []byte, chat bool) error {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return rejectPrismGatewayRequest(c, "invalid_json", "Request body must be a valid JSON object")
	}
	if failoverErr := prismUnsupportedRequest(c, body, chat); failoverErr != nil {
		return failoverErr
	}
	return nil
}

func rejectPrismGatewayRequest(c *gin.Context, code, message string) error {
	err := infraerrors.BadRequest(code, message)
	if c != nil {
		MarkResponseCommitted(c)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
	}
	return err
}
