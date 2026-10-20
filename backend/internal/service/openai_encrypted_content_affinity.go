package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIEncryptedContentMaxDigests           = 512
	openAIAccountScheduleLayerEncryptedContent = "encrypted_content"
)

// OpenAIEncryptedContentAffinityError reports a request-specific continuity
// problem. Its public fields contain neither ciphertext nor upstream identity.
type OpenAIEncryptedContentAffinityError struct {
	Code    string
	Message string
	status  int
	cause   error
}

func (e *OpenAIEncryptedContentAffinityError) Error() string   { return e.Message }
func (e *OpenAIEncryptedContentAffinityError) HTTPStatus() int { return e.status }
func (e *OpenAIEncryptedContentAffinityError) Unwrap() error   { return e.cause }

func openAIEncryptedContentError(code, message string, status int, cause error) error {
	return &OpenAIEncryptedContentAffinityError{Code: code, Message: message, status: status, cause: cause}
}

func openAIEncryptedContentCacheError(cause error) error {
	return openAIEncryptedContentError("encrypted_content_affinity_unavailable", "Encrypted message routing is temporarily unavailable. Retry the request without changing its contents.", http.StatusServiceUnavailable, cause)
}

func openAIEncryptedContentConflictError() error {
	return openAIEncryptedContentError("encrypted_content_account_conflict", "This request combines encrypted messages from incompatible upstream sessions. Start a new conversation and resend the original task.", http.StatusBadRequest, nil)
}

type openAIEncryptedContentAffinityContextKey struct{}
type openAIEncryptedContentAffinity struct {
	apiKeyID int64
	digests  []string
	err      error
}

// WithOpenAIEncryptedContentAffinity retains authenticated scope and only
// ciphertext hashes. Child threads can reuse a producer without sharing a
// thread ID, while a different API key or group cannot claim its binding.
func WithOpenAIEncryptedContentAffinity(ctx context.Context, apiKeyID int64, body []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	digests, err := openAIEncryptedInputDigests(body)
	return context.WithValue(ctx, openAIEncryptedContentAffinityContextKey{}, openAIEncryptedContentAffinity{apiKeyID: apiKeyID, digests: digests, err: err})
}

func openAIEncryptedContentAffinityFromContext(ctx context.Context) openAIEncryptedContentAffinity {
	if ctx == nil {
		return openAIEncryptedContentAffinity{}
	}
	affinity, _ := ctx.Value(openAIEncryptedContentAffinityContextKey{}).(openAIEncryptedContentAffinity)
	return affinity
}

func openAIAddEncryptedDigest(digests map[string]struct{}, value gjson.Result) {
	if value.Type == gjson.String && value.Str != "" {
		digests[openAIEncryptedContentDigest(value.Str)] = struct{}{}
	}
}

func openAICollectEncryptedParts(digests map[string]struct{}, parts gjson.Result) {
	if !parts.IsArray() {
		return
	}
	for _, part := range parts.Array() {
		if part.Get("type").String() == "encrypted_content" {
			openAIAddEncryptedDigest(digests, part.Get("encrypted_content"))
			openAIAddEncryptedDigest(digests, part.Get("data"))
		}
	}
}

func openAICollectFunctionArgumentDigests(digests map[string]struct{}, item gjson.Result) {
	if item.Get("type").String() != "function_call" {
		return
	}
	args := item.Get("arguments")
	if args.Type != gjson.String || !gjson.Valid(args.Str) {
		return
	}
	fields := item.Get("encrypted_function_args")
	arguments := gjson.Parse(args.Str).Map()
	if fields.IsArray() {
		declared := fields.Array()
		if len(declared) == 0 {
			return
		}
		for _, field := range declared {
			if field.Type == gjson.String {
				openAIAddEncryptedDigest(digests, arguments[field.Str])
			}
		}
	}
	// Codex's collaboration router treats only an explicit [] as plaintext.
	// Missing/null metadata still delivers arguments.message as ciphertext;
	// this behavior does not depend on whether tools were repeated this turn.
	namespace, name := item.Get("namespace").String(), item.Get("name").String()
	if namespace == "" && strings.HasPrefix(name, "collaboration.") {
		namespace, name = "collaboration", strings.TrimPrefix(name, "collaboration.")
	}
	if namespace == "collaboration" {
		switch name {
		case "spawn_agent", "send_message", "followup_task":
			openAIAddEncryptedDigest(digests, arguments["message"])
		}
	}
}

func openAICollectEncryptedItemDigests(digests map[string]struct{}, item gjson.Result) {
	switch item.Get("type").String() {
	case "agent_message", "message":
		openAICollectEncryptedParts(digests, item.Get("content"))
		if item.Get("type").String() == "agent_message" {
			openAIAddEncryptedDigest(digests, item.Get("encrypted_content"))
		}
	case "function_call_output", "custom_tool_call_output":
		openAICollectEncryptedParts(digests, item.Get("output"))
	case "function_call":
		openAICollectFunctionArgumentDigests(digests, item)
	}
}

func openAISortedEncryptedDigests(digests map[string]struct{}) ([]string, error) {
	if len(digests) > openAIEncryptedContentMaxDigests {
		return nil, openAIEncryptedContentError("encrypted_content_limit_exceeded", "This request contains too many encrypted messages. Start a new conversation and resend the task.", http.StatusBadRequest, nil)
	}
	result := make([]string, 0, len(digests))
	for digest := range digests {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result, nil
}

func openAIEncryptedInputDigests(body []byte) ([]string, error) {
	digests := make(map[string]struct{})
	input := openAIRequestPayloadView(body).Get("input")
	if input.IsArray() {
		for _, item := range input.Array() {
			openAICollectEncryptedItemDigests(digests, item)
			if len(digests) > openAIEncryptedContentMaxDigests {
				break
			}
		}
	} else if input.IsObject() {
		// Passthrough normalization accepts a single item and wraps it in an
		// array later. Account selection must inspect it before normalization.
		openAICollectEncryptedItemDigests(digests, input)
	}
	return openAISortedEncryptedDigests(digests)
}

func openAIEncryptedOutputDigests(payload []byte) ([]string, error) {
	return openAIEncryptedOutputEventDigests("", payload)
}

func openAIEncryptedOutputEventDigests(eventType string, payload []byte) ([]string, error) {
	digests := make(map[string]struct{})
	root := gjson.ParseBytes(payload)
	if payloadType := root.Get("type").String(); payloadType != "" {
		eventType = payloadType
	}
	switch eventType {
	case "response.output_item.done":
		openAICollectEncryptedItemDigests(digests, root.Get("item"))
	case "response.completed", "response.done":
		for _, item := range root.Get("response.output").Array() {
			openAICollectEncryptedItemDigests(digests, item)
		}
	default:
		for _, item := range root.Get("output").Array() {
			openAICollectEncryptedItemDigests(digests, item)
		}
	}
	return openAISortedEncryptedDigests(digests)
}

func (s *OpenAIGatewayService) resolveOpenAIEncryptedContentAccount(ctx context.Context, groupID *int64, affinity openAIEncryptedContentAffinity) (int64, error) {
	if affinity.err != nil {
		return 0, affinity.err
	}
	if s == nil || affinity.apiKeyID <= 0 || len(affinity.digests) == 0 {
		return 0, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, openAIWSStateStoreRedisTimeout)
	defer cancel()
	store := s.getOpenAIWSStateStore()
	owner := int64(0)
	known := make([]string, 0, len(affinity.digests))
	for _, digest := range affinity.digests {
		accountID, err := store.GetEncryptedContentAccount(lookupCtx, derefGroupID(groupID), affinity.apiKeyID, digest)
		if err != nil {
			return 0, openAIEncryptedContentCacheError(err)
		}
		if accountID <= 0 {
			continue
		} // Historical/foreign ciphertext is never guessed or removed.
		if owner > 0 && owner != accountID {
			return 0, openAIEncryptedContentConflictError()
		}
		owner = accountID
		known = append(known, digest)
	}
	for _, digest := range known {
		if err := store.BindEncryptedContentAccount(lookupCtx, derefGroupID(groupID), affinity.apiKeyID, digest, owner, s.openAIWSResponseStickyTTL()); err != nil {
			var affinityErr *OpenAIEncryptedContentAffinityError
			if errors.As(err, &affinityErr) {
				return 0, err
			}
			return 0, openAIEncryptedContentCacheError(err)
		}
	}
	return owner, nil
}

// ValidateOpenAIEncryptedContentAccount guards a later turn on an already
// selected WebSocket connection; changing accounts inside that connection
// would invalidate its continuation state.
func (s *OpenAIGatewayService) ValidateOpenAIEncryptedContentAccount(ctx context.Context, groupID *int64, apiKeyID, accountID int64, body []byte) error {
	ctx = WithOpenAIEncryptedContentAffinity(ctx, apiKeyID, body)
	owner, err := s.resolveOpenAIEncryptedContentAccount(ctx, groupID, openAIEncryptedContentAffinityFromContext(ctx))
	if err != nil {
		return err
	}
	if owner > 0 && owner != accountID {
		return openAIEncryptedContentError("encrypted_content_account_mismatch", "This connection cannot read an encrypted message from another upstream session. Reconnect and retry the request with its full input history.", http.StatusConflict, nil)
	}
	return nil
}

// OpenAIEncryptedContentObserver must observe finalized items before they are
// delivered to the client, which can immediately launch another agent. A
// per-response set avoids duplicate writes from item.done and response.done.
type OpenAIEncryptedContentObserver struct {
	store                        OpenAIWSStateStore
	ctx                          context.Context
	groupID, apiKeyID, accountID int64
	ttl                          time.Duration
	seen                         map[string]struct{}
}

func (s *OpenAIGatewayService) NewOpenAIEncryptedContentObserver(ctx context.Context, groupID *int64, apiKeyID, accountID int64) *OpenAIEncryptedContentObserver {
	if s == nil || apiKeyID <= 0 || accountID <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &OpenAIEncryptedContentObserver{store: s.getOpenAIWSStateStore(), ctx: ctx, groupID: derefGroupID(groupID), apiKeyID: apiKeyID, accountID: accountID, ttl: s.openAIWSResponseStickyTTL(), seen: make(map[string]struct{})}
}

func (s *OpenAIGatewayService) NewOpenAIEncryptedContentObserverForRequest(ctx context.Context, c *gin.Context, account *Account) *OpenAIEncryptedContentObserver {
	if account == nil {
		return nil
	}
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 {
		apiKeyID = openAIEncryptedContentAffinityFromContext(ctx).apiKeyID
	}
	groupID := getOpenAIGroupIDFromContext(c)
	return s.NewOpenAIEncryptedContentObserver(ctx, &groupID, apiKeyID, account.ID)
}

func (o *OpenAIEncryptedContentObserver) Observe(payload []byte) error {
	return o.ObserveEvent("", payload)
}

// ObserveEvent supports SSE streams that provide the event name only in the
// event: field. The original JSON bytes sent to the client remain untouched.
func (o *OpenAIEncryptedContentObserver) ObserveEvent(eventType string, payload []byte) error {
	if o == nil {
		return nil
	}
	if payloadType := gjson.GetBytes(payload, "type").String(); payloadType != "" {
		eventType = payloadType
	}
	if eventType == "response.created" {
		clear(o.seen)
	}
	digests, err := openAIEncryptedOutputEventDigests(eventType, payload)
	if err != nil {
		return err
	}
	if len(digests) == 0 {
		return nil
	}
	bindCtx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), openAIWSStateStoreRedisTimeout)
	defer cancel()
	for _, digest := range digests {
		if _, seen := o.seen[digest]; seen {
			continue
		}
		owner, err := o.store.GetEncryptedContentAccount(bindCtx, o.groupID, o.apiKeyID, digest)
		if err != nil {
			return openAIEncryptedContentCacheError(err)
		}
		if owner > 0 && owner != o.accountID {
			return openAIEncryptedContentConflictError()
		}
		if err := o.store.BindEncryptedContentAccount(bindCtx, o.groupID, o.apiKeyID, digest, o.accountID, o.ttl); err != nil {
			var affinityErr *OpenAIEncryptedContentAffinityError
			if errors.As(err, &affinityErr) {
				return err
			}
			return openAIEncryptedContentCacheError(err)
		}
		ensureBindingCapacity(o.seen, digest, openAIEncryptedContentMaxDigests)
		o.seen[digest] = struct{}{}
	}
	return nil
}

func (s *OpenAIGatewayService) selectOpenAIEncryptedContentAccount(ctx context.Context, req OpenAIAccountScheduleRequest, owner int64) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: openAIAccountScheduleLayerEncryptedContent}
	if s.checkChannelPricingRestriction(ctx, req.GroupID, req.RequestedModel) {
		return nil, decision, openAIEncryptedContentError("encrypted_content_account_unavailable", "The upstream session needed for these encrypted messages is unavailable for this request. Retry later or start a new conversation with the original task.", http.StatusServiceUnavailable, nil)
	}
	if previous := strings.TrimSpace(req.PreviousResponseID); previous != "" {
		previousOwner, err := s.getOpenAIWSStateStore().GetResponseAccount(ctx, derefGroupID(req.GroupID), previous)
		if err != nil {
			return nil, decision, openAIEncryptedContentCacheError(err)
		}
		if previousOwner > 0 && previousOwner != owner {
			return nil, decision, openAIEncryptedContentConflictError()
		}
	}
	req.StickyAccountID = owner
	req.PreserveStickyBinding = true
	req.DisableStickyEscape = true
	if req.SessionHash == "" {
		req.SessionHash = "encrypted-content-owner"
	}
	ctx = withOpenAIEncryptedContentOwnerProbe(ctx, owner)
	scheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
	selection, _, err := scheduler.selectBySessionHash(ctx, req)
	if err != nil {
		return nil, decision, err
	}
	if selection == nil || selection.Account == nil {
		return nil, decision, openAIEncryptedContentError("encrypted_content_account_unavailable", "The upstream session needed for these encrypted messages is unavailable for this request. Retry later or start a new conversation with the original task.", http.StatusServiceUnavailable, nil)
	}
	decision.SelectedAccountID, decision.SelectedAccountType = selection.Account.ID, selection.Account.Type
	if selection.Account.GetRateLimitRemainingTime(req.RequestedModel) > 0 {
		slog.Info("openai.encrypted_content_owner_model_cooldown_probe", "account_id", owner, "model", req.RequestedModel)
	}
	return selection, decision, nil
}

// shouldClearOpenAIStickySession keeps shouldClearStickySession's behavior,
// except that the encrypted-content owner may skip a model-not-found cooldown.
func shouldClearOpenAIStickySession(ctx context.Context, account *Account, requestedModel string) bool {
	if !openAIEncryptedContentOwnerProbeFor(ctx, account) {
		return shouldClearStickySession(account, requestedModel)
	}
	return !account.IsSchedulable() || account.GetRateLimitRemainingTimeWithContext(ctx, requestedModel) > 0
}
