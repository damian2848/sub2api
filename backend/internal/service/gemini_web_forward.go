package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/googleapi"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	GeminiWebCredentialRejectedReason        GatewayFailureReason = "gemini_web_credential_rejected"
	GeminiWebCredentialRejectedClientMessage string               = "Gemini Web Cookie is invalid or expired; update the account Cookie and retry"
	// GeminiWebCannedReplyReason marks a reply Gemini Web replaced with its
	// built-in failure message. The request fails (and is not billed) unless
	// another account answers.
	GeminiWebCannedReplyReason        GatewayFailureReason = "gemini_web_canned_reply"
	GeminiWebCannedReplyClientMessage string               = "Gemini Web failed to generate a reply (upstream returned its built-in error message); this request was not billed, please retry"
	// Admission rejections did not attempt generation and do not consume the
	// handler's upstream failover budget.
	GeminiWebHealthCooldownReason GatewayFailureReason = "gemini_web_health_cooldown"
	GeminiWebQuotaPrecheckReason  GatewayFailureReason = "gemini_web_quota_precheck"
)

type geminiWebQuotaPrecheckError struct{ cause error }

func (e *geminiWebQuotaPrecheckError) Error() string { return e.cause.Error() }
func (e *geminiWebQuotaPrecheckError) Unwrap() error { return e.cause }

func geminiWebAuthUser(account *Account) (int, bool) {
	if account == nil || account.Credentials == nil {
		return 0, false
	}
	_, present := account.Credentials["auth_user"]
	if !present {
		return 0, false
	}
	raw := strings.TrimSpace(account.GetCredential("auth_user"))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

func (s *GeminiMessagesCompatService) geminiWebClient() *geminiweb.Client {
	if s.geminiWeb == nil {
		s.geminiWeb = geminiweb.NewClient()
	}
	return s.geminiWeb
}

func (s *GeminiMessagesCompatService) callGeminiWeb(ctx context.Context, account *Account, model, prompt string, thinking bool) (geminiweb.Result, error) {
	return s.callGeminiWebWithCallback(ctx, account, model, geminiWebPromptPlan{Prompt: prompt}, thinking, nil)
}

func (s *GeminiMessagesCompatService) callGeminiWebWithCallback(ctx context.Context, account *Account, model string, plan geminiWebPromptPlan, thinking bool, onResult func(geminiweb.Result) error) (geminiweb.Result, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, ok := geminiweb.LookupModel(model); !ok {
		return geminiweb.Result{}, fmt.Errorf("unsupported Gemini Web model: %s", model)
	}
	cookie := account.GetCredential("gemini_web_cookie")
	if strings.TrimSpace(cookie) == "" {
		return geminiweb.Result{}, geminiweb.ErrInvalidCredential
	}
	if s.httpUpstream == nil {
		return geminiweb.Result{}, errors.New("upstream HTTP client is not configured")
	}
	if err := requestCtx.Err(); err != nil {
		return geminiweb.Result{}, err
	}
	healthAttempt, admitted := s.geminiWebHealth.begin(account, time.Now())
	if !admitted {
		return geminiweb.Result{}, errGeminiWebHealthCooling
	}
	// HTTP recovery may catch a panic in an upstream adapter. Never leave a
	// recovery probe owned forever; finishing an already-finished token is safe.
	defer func() {
		s.geminiWebHealth.finish(healthAttempt, geminiWebHealthNeutral, time.Now())
	}()
	// Only the selected account is checked. A rejected admission is not an
	// upstream generation attempt and must release a half-open health probe.
	// The quota helper owns credential/429 persistence for this read.
	if err := s.precheckGeminiWebGenerationQuota(requestCtx, account); err != nil {
		return geminiweb.Result{}, &geminiWebQuotaPrecheckError{cause: err}
	}
	endHistoryRequest, err := beginGeminiWebHistoryRequest(requestCtx, s.accountRepo, account.ID)
	if err != nil {
		return geminiweb.Result{}, &geminiWebQuotaPrecheckError{cause: err}
	}
	defer endHistoryRequest()
	cookie = account.GetCredential("gemini_web_cookie")
	authUser, authUserSet := geminiWebAuthUser(account)
	cred := geminiweb.Credential{CacheGeneration: account.GetCredential(geminiWebCookieGeneration),
		Cookie: cookie, AuthUser: authUser, AuthUserSet: authUserSet,
		BaseURL: "https://gemini.google.com", UserAgent: account.GetCredential("user_agent"),
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	do := func(req *http.Request) (*http.Response, error) {
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	inflight, sinceLast, done := s.geminiWebActivity.begin(account.ID, time.Now())
	defer done()
	result, err := s.geminiWebClient().Complete(requestCtx, cred, geminiweb.Request{Model: model, Prompt: plan.Prompt, Attachments: plan.Attachments, Thinking: thinking, OnResult: onResult}, do)
	done()
	s.finishGeminiWebHealth(requestCtx, account, healthAttempt, result, err)
	logGeminiWebTiming(ctx, account, model, result, err)
	s.recordGeminiWebAttempt(ctx, account, model, thinking, inflight, sinceLast, result, err)
	if err == nil {
		s.observeGeminiWebShortReply(ctx, account, model, result)
	}
	if err == nil || (!errors.Is(err, geminiweb.ErrInvalidCredential) && !errors.Is(err, geminiweb.ErrRateLimited)) {
		if live := s.geminiWebClient().CurrentCookie(cred); live != "" {
			result.UpdatedCookie = live
		}
	}
	if !errors.Is(err, geminiweb.ErrInvalidCredential) && !errors.Is(err, geminiweb.ErrRateLimited) && result.UpdatedCookie != "" && result.UpdatedCookie != cookie {
		persistLock := geminiWebCredentialPersistLock(account.ID)
		persistLock.Lock()
		defer persistLock.Unlock()
		updated := shallowCopyMap(account.Credentials)
		updated["gemini_web_cookie"] = result.UpdatedCookie
		updated[geminiWebCookieLastRefresh] = time.Now().UTC().Format(time.RFC3339Nano)
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer persistCancel()
		if persistErr := persistAccountCredentials(persistCtx, s.accountRepo, account, updated); persistErr != nil {
			// A rotated cookie is useful but a persistence failure must not turn a
			// successful model response into a failed request.
			account.Credentials = updated
		}
	}
	if !errors.Is(err, geminiweb.ErrInvalidCredential) {
		s.refreshGeminiWebQuotaAfterRequest(account)
	}
	if err != nil {
		if errors.Is(err, geminiweb.ErrInvalidCredential) {
			s.geminiWebClient().InvalidateCredential(geminiWebCacheCredential(account, cookie))
			if s.accountRepo != nil && account.ID > 0 {
				markGeminiWebCookieRejected(ctx, s.accountRepo, account, "Gemini Web cookie expired or rejected")
			}
		} else if errors.Is(err, geminiweb.ErrRateLimited) && s.accountRepo != nil && account.ID > 0 {
			applyGeminiWebShortRateLimit(ctx, s.accountRepo, account)
		}
		return result, err
	}
	return result, nil
}

// logGeminiWebTiming writes one line per Gemini Web completion with the time
// each stage took. Gemini Web returns no usage or latency data of its own, so
// this line is the only way to see whether a slow request was waiting on
// Google's headers, on the first frame, or on the answer after thinking.
func logGeminiWebTiming(ctx context.Context, account *Account, model string, result geminiweb.Result, err error) {
	t := result.Timing
	fields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("model", model),
		zap.Int("prompt_chars", t.PromptChars),
		zap.Int("attachments", t.Attachments),
		zap.Int64("upload_ms", geminiWebStageMs(t.Upload)),
		zap.Int("upstream_status", t.Status),
		zap.Int64("session_ms", geminiWebStageMs(t.Session)),
		zap.Int64("headers_ms", geminiWebStageMs(t.Headers)),
		zap.Int64("first_frame_ms", geminiWebStageMs(t.FirstFrame)),
		zap.Int64("first_thinking_ms", geminiWebStageMs(t.FirstThinking)),
		zap.Int64("first_text_ms", geminiWebStageMs(t.FirstText)),
		zap.Int64("total_ms", geminiWebStageMs(t.Total)),
		zap.Int("thinking_chars", utf8.RuneCountInString(result.Thinking)),
		zap.Int("text_chars", utf8.RuneCountInString(result.Text)),
	}
	if err != nil {
		logger.FromContext(ctx).Warn("gemini_web.completion_timing", append(fields, zap.Error(err))...)
		return
	}
	logger.FromContext(ctx).Info("gemini_web.completion_timing", fields...)
}

// geminiWebStageMs reports a stage that was never reached as -1, so it cannot
// be mistaken for an instant one.
func geminiWebStageMs(d time.Duration) int64 {
	if d <= 0 {
		return -1
	}
	return d.Milliseconds()
}

// geminiWebAttachmentTokens is the input estimate for one attachment. Gemini
// Web reports no usage; this is what the Gemini API charges for an image.
const geminiWebAttachmentTokens = 258

func geminiWebInputTokens(plan geminiWebPromptPlan) int {
	return estimateTokensForText(plan.Prompt) + geminiWebAttachmentTokens*len(plan.Attachments)
}

func webUsageWithThinking(plan geminiWebPromptPlan, answer, thinking string) ClaudeUsage {
	return ClaudeUsage{InputTokens: geminiWebInputTokens(plan), OutputTokens: estimateTokensForText(answer) + estimateTokensForText(thinking)}
}

func (s *GeminiMessagesCompatService) forwardGeminiWebNative(ctx context.Context, c *gin.Context, account *Account, originalModel, action string, stream bool, body []byte) (*ForwardResult, error) {
	start := time.Now()
	if action == "countTokens" {
		plan, err := geminiWebBuildPrompt(body)
		if err != nil {
			return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
		}
		c.JSON(http.StatusOK, gin.H{"totalTokens": geminiWebInputTokens(plan)})
		return &ForwardResult{Model: originalModel, UpstreamModel: account.GetMappedModel(originalModel), Usage: ClaudeUsage{}}, nil
	}
	if action != "generateContent" && action != "streamGenerateContent" {
		return nil, s.writeGoogleError(c, http.StatusNotFound, "Unsupported action: "+action)
	}
	plan, err := geminiWebBuildPrompt(body)
	if err != nil {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
	}
	mapped := account.GetMappedModel(originalModel)
	ctx = withGeminiWebCallInfo(ctx, geminiWebCallInfo{Protocol: "native", Stream: stream || action == "streamGenerateContent", Tools: len(plan.Tools)})
	var streamWriter *geminiWebNativeStreamWriter
	var onResult func(geminiweb.Result) error
	if stream || action == "streamGenerateContent" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		streamWriter = newGeminiWebNativeStreamWriter(c, originalModel, cancel)
		streamWriter.plan = plan
		streamWriter.canned = s.geminiWebCannedReplies(ctx)
		streamWriter.start = start
		onResult = streamWriter.WriteResult
	}
	result, err := s.callGeminiWebWithCallback(ctx, account, mapped, plan, strings.Contains(strings.ToLower(originalModel), "thinking"), onResult)
	if err == nil {
		err = s.checkGeminiWebCannedReply(ctx, account, result, s.geminiWebCannedReplies(ctx))
	}
	if err != nil {
		if streamWriter != nil {
			return nil, streamWriter.Abort(err)
		}
		return nil, geminiWebFailoverError(err)
	}
	usage := webUsageWithThinking(plan, result.Text, result.Thinking)
	if stream || action == "streamGenerateContent" {
		if err := streamWriter.Finish(result, usage); err != nil {
			return nil, err
		}
	} else {
		text, calls := geminiWebExtractToolCalls(result.Text, plan)
		writeGeminiWebNativeJSON(c, originalModel, text, calls, usage)
	}
	forwardResult := &ForwardResult{
		RequestID: result.RequestID, UpstreamHeaders: make(http.Header), Usage: usage,
		Model: originalModel, UpstreamModel: mapped, Stream: stream, Duration: time.Since(start),
		UpstreamResponseModel: result.ResponseModel, UpstreamResponseModelConflict: result.ResponseModelConflict,
	}
	if streamWriter != nil {
		forwardResult.FirstTokenMs = streamWriter.firstTokenMs
	}
	return forwardResult, nil
}

func geminiWebFailoverError(err error) error {
	status := http.StatusBadGateway
	stage := GatewayFailureStageInference
	scope := GatewayFailureScopeProvider
	if errors.Is(err, geminiweb.ErrInvalidCredential) {
		status = http.StatusUnauthorized
		stage = GatewayFailureStageAccountAuth
		scope = GatewayFailureScopeAccount
	}
	if errors.Is(err, geminiweb.ErrRateLimited) {
		status = http.StatusTooManyRequests
	}
	reason := GatewayFailureReason("gemini_web")
	message := ""
	if errors.Is(err, geminiweb.ErrEmptyResponse) {
		reason = "gemini_web_empty_response"
	}
	var upstream *geminiweb.UpstreamHTTPError
	if errors.As(err, &upstream) && upstream.StatusCode >= 500 && upstream.StatusCode <= 599 {
		reason = GatewayFailureReason("gemini_web_upstream_" + strconv.Itoa(upstream.StatusCode))
	}
	if errors.Is(err, errGeminiWebHealthCooling) {
		reason = GeminiWebHealthCooldownReason
		scope = GatewayFailureScopeAccount
	}
	if errors.Is(err, geminiweb.ErrInvalidCredential) {
		reason = GeminiWebCredentialRejectedReason
		message = GeminiWebCredentialRejectedClientMessage
	}
	if errors.Is(err, geminiweb.ErrCannedReply) {
		reason = GeminiWebCannedReplyReason
		message = GeminiWebCannedReplyClientMessage
	}
	var precheck *geminiWebQuotaPrecheckError
	if errors.As(err, &precheck) {
		reason = GeminiWebQuotaPrecheckReason
		scope = GatewayFailureScopeAccount
	}
	return &UpstreamFailoverError{StatusCode: status, Stage: stage, Scope: scope, Reason: reason, ClientStatusCode: status, ClientMessage: message}
}

func geminiWebUsageMetadata(usage ClaudeUsage) gin.H {
	return gin.H{"promptTokenCount": usage.InputTokens, "candidatesTokenCount": usage.OutputTokens, "totalTokenCount": usage.InputTokens + usage.OutputTokens}
}

func writeGeminiWebNativeJSON(c *gin.Context, model, text string, calls []geminiWebToolCall, usage ClaudeUsage) {
	parts := []any{}
	if text != "" || len(calls) == 0 {
		parts = append(parts, gin.H{"text": text})
	}
	parts = append(parts, geminiWebFunctionCallParts(calls)...)
	c.JSON(http.StatusOK, gin.H{"candidates": []any{gin.H{"content": gin.H{"role": "model", "parts": parts}, "finishReason": "STOP", "index": 0}}, "modelVersion": model, "usageMetadata": geminiWebUsageMetadata(usage)})
}

// geminiWebNativeStreamWriter forwards cumulative browser snapshots as native
// Gemini SSE deltas. Text from the first <tool_call> on is held back and sent
// as functionCall parts once the answer is complete. Gemini Web can spend a long time between answer frames,
// so the writer sends blank SSE heartbeats every five seconds until the
// request finishes, keeping idle connections alive without triggering the
// older Gemini CLI parsers that reject comment heartbeats.
type geminiWebNativeStreamWriter struct {
	c            *gin.Context
	model        string
	plan         geminiWebPromptPlan
	start        time.Time
	firstTokenMs *int
	mu           sync.Mutex
	text         string
	thinking     string
	released     bool
	canned       geminiweb.CannedReplies
	committed    bool
	// contentWritten is set once a data event (not a heartbeat) was written.
	// Until then a failed attempt can still switch accounts on this response.
	contentWritten bool
	// sizeBefore is GeminiWebPingAdjustedWrittenSize when the writer started.
	sizeBefore    int
	stopped       bool
	writeErr      error
	cancel        context.CancelFunc
	stopKeepAlive chan struct{}
	keepAliveDone chan struct{}
	stopOnce      sync.Once
}

func newGeminiWebNativeStreamWriter(c *gin.Context, model string, cancel context.CancelFunc) *geminiWebNativeStreamWriter {
	w := &geminiWebNativeStreamWriter{
		c:             c,
		model:         model,
		cancel:        cancel,
		stopKeepAlive: make(chan struct{}),
		keepAliveDone: make(chan struct{}),
		sizeBefore:    GeminiWebPingAdjustedWrittenSize(c),
	}
	go w.keepAlive()
	return w
}

// geminiWebNativeHeartbeatInterval is how often native Gemini Web streams send
// a blank-line heartbeat while no frame is due.
var geminiWebNativeHeartbeatInterval = 5 * time.Second

func (w *geminiWebNativeStreamWriter) keepAlive() {
	ticker := time.NewTicker(geminiWebNativeHeartbeatInterval)
	defer ticker.Stop()
	defer close(w.keepAliveDone)
	for {
		select {
		case <-ticker.C:
			w.mu.Lock()
			if w.stopped {
				w.mu.Unlock()
				return
			}
			err := w.writeHeartbeatLocked()
			w.mu.Unlock()
			if err != nil {
				return
			}
		case <-w.stopKeepAlive:
			return
		case <-w.c.Request.Context().Done():
			return
		}
	}
}

func (w *geminiWebNativeStreamWriter) stop() {
	w.mu.Lock()
	w.stopped = true
	w.stopOnce.Do(func() { close(w.stopKeepAlive) })
	w.mu.Unlock()
	<-w.keepAliveDone
}

func (w *geminiWebNativeStreamWriter) commitLocked() {
	if w.committed {
		return
	}
	// An earlier attempt that failed over after heartbeats already committed
	// the response; keep writing on it.
	if w.c.Writer.Written() {
		w.committed = true
		return
	}
	w.c.Writer.Header().Set("Content-Type", "text/event-stream")
	w.c.Writer.Header().Set("Cache-Control", "no-cache")
	w.c.Writer.Header().Set("Connection", "keep-alive")
	w.c.Writer.WriteHeader(http.StatusOK)
	w.committed = true
	MarkResponseCommitted(w.c)
}

func (w *geminiWebNativeStreamWriter) flushLocked() {
	if f, ok := w.c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *geminiWebNativeStreamWriter) writeLocked(data []byte) error {
	return w.writeFrameLocked(data, false)
}

func (w *geminiWebNativeStreamWriter) writeFrameLocked(data []byte, heartbeat bool) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.c.Request != nil && w.c.Request.Context().Err() != nil {
		return w.c.Request.Context().Err()
	}
	w.commitLocked()
	n, err := w.c.Writer.Write(data)
	if heartbeat {
		addGeminiWebPingBytes(w.c, n)
	} else if n > 0 {
		w.contentWritten = true
	}
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.writeErr = err
		if w.cancel != nil {
			w.cancel()
		}
		return err
	}
	w.flushLocked()
	return nil
}

func (w *geminiWebNativeStreamWriter) writeHeartbeatLocked() error {
	// Keep the heartbeat as an empty SSE event. Gemini CLI's bundled parser
	// accepts the blank-line form across its supported SDK versions, while
	// comment heartbeats are rejected by some older versions. Heartbeats are
	// not content: they are left out when deciding whether to fail over.
	return w.writeFrameLocked([]byte("\n\n"), true)
}

func geminiWebCumulativeDelta(previous, current string) (delta, next string) {
	if current == "" {
		return "", previous
	}
	if previous == "" || strings.HasPrefix(current, previous) {
		return current[len(previous):], current
	}
	if strings.HasPrefix(previous, current) {
		return "", previous
	}
	// Gemini Web occasionally replaces a provisional thought/answer snapshot.
	// Preserve the already emitted prefix and append the replacement as a new
	// delta instead of duplicating the whole response.
	return current, current
}

func (w *geminiWebNativeStreamWriter) writeResultLocked(result geminiweb.Result) error {
	// With tools, answer text is held until the reply is complete, like the
	// compat streams: Gemini Web can replace a half-written answer with a
	// canned failure message, and only an unsent answer can fail over.
	text := ""
	if !w.plan.toolsEnabled() {
		text, w.released = geminiWebHoldPossibleCannedReply(result, result.Text, w.text, w.released, w.canned)
	}
	return w.writeSnapshotLocked(text, result.Thinking, nil, nil)
}

func (w *geminiWebNativeStreamWriter) writeSnapshotLocked(text, thinking string, calls []geminiWebToolCall, extra gin.H) error {
	textDelta, nextText := geminiWebCumulativeDelta(w.text, text)
	thinkingDelta, nextThinking := geminiWebCumulativeDelta(w.thinking, thinking)
	w.text, w.thinking = nextText, nextThinking
	if textDelta == "" && thinkingDelta == "" && len(calls) == 0 && extra == nil {
		return nil
	}
	if w.firstTokenMs == nil && (textDelta != "" || thinkingDelta != "" || len(calls) > 0) && !w.start.IsZero() {
		ms := int(time.Since(w.start).Milliseconds())
		w.firstTokenMs = &ms
	}
	parts := make([]any, 0, 2+len(calls))
	if thinkingDelta != "" {
		parts = append(parts, gin.H{"text": thinkingDelta, "thought": true})
	}
	if textDelta != "" {
		parts = append(parts, gin.H{"text": textDelta})
	}
	parts = append(parts, geminiWebFunctionCallParts(calls)...)
	candidate := gin.H{"index": 0}
	if len(parts) > 0 {
		candidate["content"] = gin.H{"role": "model", "parts": parts}
	}
	payload := gin.H{"candidates": []any{candidate}, "modelVersion": w.model}
	for key, value := range extra {
		if key == "finishReason" {
			candidate[key] = value
			continue
		}
		payload[key] = value
	}
	encoded, _ := json.Marshal(payload)
	return w.writeLocked([]byte("data: " + string(encoded) + "\n\n"))
}

func (w *geminiWebNativeStreamWriter) WriteResult(result geminiweb.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.stopped {
		return context.Canceled
	}
	if err := w.c.Request.Context().Err(); err != nil {
		return err
	}
	return w.writeResultLocked(result)
}

func (w *geminiWebNativeStreamWriter) Finish(result geminiweb.Result, usage ClaudeUsage) error {
	w.stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	text, calls := geminiWebExtractToolCalls(result.Text, w.plan)
	if err := w.writeSnapshotLocked(text, result.Thinking, calls, nil); err != nil {
		return err
	}
	return w.writeSnapshotLocked(w.text, w.thinking, nil, gin.H{"finishReason": "STOP", "usageMetadata": geminiWebUsageMetadata(usage)})
}

func (w *geminiWebNativeStreamWriter) Abort(cause error) error {
	w.stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	if err := w.c.Request.Context().Err(); err != nil {
		return err
	}
	// Account switching is safe while the client has received no data event:
	// only blank-line heartbeats (ours, or output that was already there when
	// this writer started, such as a concurrency-wait heartbeat). The next
	// account then writes its stream on the same response.
	if !w.contentWritten && GeminiWebPingAdjustedWrittenSize(w.c) == w.sizeBefore {
		return geminiWebFailoverError(cause)
	}
	status, message := geminiWebStreamErrorStatus(cause)
	SetOpsUpstreamError(w.c, status, message, "")
	MarkOpsStreamFailure(w.c, "upstream_error", "gemini_web", message, status)
	payload, _ := json.Marshal(gin.H{"error": gin.H{
		"code": status, "message": message, "status": googleapi.HTTPStatusToGoogleStatus(status),
	}})
	if err := w.writeLocked([]byte("data: " + string(payload) + "\n\n")); err != nil {
		return err
	}
	// Do not wrap the upstream error: it must never be interpreted as another
	// failover attempt after the SSE response is committed.
	return errors.New(message)
}

func (s *GeminiMessagesCompatService) forwardGeminiWebClaude(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	start := time.Now()
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Model) == "" {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	geminiBody, err := convertClaudeMessagesToGeminiGenerateContent(body)
	if err != nil {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	plan, err := geminiWebBuildPrompt(geminiWebClaudeToolChoice(geminiBody, body))
	if err != nil {
		return nil, s.writeClaudeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	return s.forwardGeminiWebCompat(ctx, c, account, geminiWebCompatRequest{
		protocol: "claude", model: req.Model, stream: req.Stream, plan: plan, start: start,
		includeThought: geminiWebClaudeThinkingEnabled(body),
		handleStream: func(resp *http.Response) (*geminiStreamResult, error) {
			return s.handleStreamingResponse(c, resp, start, req.Model)
		},
		writeJSON: func(raw []byte, geminiResp map[string]any, _ ClaudeUsage) {
			claudeResp, _ := convertGeminiToClaudeMessage(geminiResp, req.Model, raw, false)
			c.JSON(http.StatusOK, claudeResp)
		},
		writeStreamError: func(message string) {
			writeSSE(c.Writer, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
		},
	})
}

func (s *GeminiMessagesCompatService) forwardGeminiWebChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	start := time.Now()
	var req apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Model) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	responses, err := apicompat.ChatCompletionsToResponses(&req)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	anthropic, err := apicompat.ResponsesToAnthropicRequest(responses)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	anthropic.Stream = req.Stream
	claudeBody, _ := json.Marshal(anthropic)
	geminiBody, err := convertClaudeMessagesToGeminiGenerateContent(claudeBody)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	plan, err := geminiWebBuildPrompt(geminiWebClaudeToolChoice(geminiBody, claudeBody))
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	return s.forwardGeminiWebCompat(ctx, c, account, geminiWebCompatRequest{
		protocol: "chat", model: req.Model, stream: req.Stream, plan: plan, start: start,
		includeThought: geminiWebClaudeThinkingEnabled(claudeBody),
		handleStream: func(resp *http.Response) (*geminiStreamResult, error) {
			return s.handleChatCompletionsStreamingResponseFromGemini(c, resp, start, req.Model, false, includeUsage)
		},
		writeJSON: func(raw []byte, geminiResp map[string]any, usage ClaudeUsage) {
			chatResp, _, err := geminiResponseToChatCompletions(geminiResp, req.Model, raw, &usage)
			if err != nil {
				_ = s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Failed to convert Gemini Web response")
				return
			}
			c.JSON(http.StatusOK, chatResp)
		},
		writeStreamError: func(message string) {
			payload, _ := json.Marshal(gin.H{"error": gin.H{"message": message, "type": "upstream_error"}})
			_, _ = c.Writer.Write([]byte("data: " + string(payload) + "\n\n"))
		},
	})
}

// geminiWebCompatRequest describes a Claude Messages or Chat Completions
// request after it has been reduced to a Gemini Web prompt. Responses reuse
// the converters of regular Gemini accounts, so tool_use / tool_calls,
// stop reasons and stream framing match the other Gemini account types.
type geminiWebCompatRequest struct {
	// protocol is the client protocol ("claude" or "chat"), for analytics.
	protocol         string
	model            string
	stream           bool
	includeThought   bool
	plan             geminiWebPromptPlan
	start            time.Time
	handleStream     func(*http.Response) (*geminiStreamResult, error)
	writeJSON        func(raw []byte, geminiResp map[string]any, usage ClaudeUsage)
	writeStreamError func(message string)
}

func (s *GeminiMessagesCompatService) forwardGeminiWebCompat(ctx context.Context, c *gin.Context, account *Account, req geminiWebCompatRequest) (*ForwardResult, error) {
	ctx = withGeminiWebCallInfo(ctx, geminiWebCallInfo{Protocol: req.protocol, Stream: req.stream, Tools: len(req.plan.Tools)})
	mapped := account.GetMappedModel(req.model)
	thinking := strings.Contains(strings.ToLower(req.model), "thinking")
	if !req.stream {
		result, err := s.callGeminiWebWithCallback(ctx, account, mapped, req.plan, thinking, nil)
		if err == nil {
			err = s.checkGeminiWebCannedReply(ctx, account, result, s.geminiWebCannedReplies(ctx))
		}
		if err != nil {
			return nil, geminiWebFailoverError(err)
		}
		usage := webUsageWithThinking(req.plan, result.Text, result.Thinking)
		text, calls := geminiWebExtractToolCalls(result.Text, req.plan)
		raw, geminiResp := geminiWebGeminiResponse(text, calls, usage, result.ResponseModel)
		req.writeJSON(raw, geminiResp, usage)
		return geminiWebCompatForwardResult(req, mapped, result, usage, nil), nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sizeBefore := GeminiWebPingAdjustedWrittenSize(c)
	pipe := newGeminiWebPipeStream(req.plan, req.includeThought, cancel, req.handleStream)
	pipe.canned = s.geminiWebCannedReplies(ctx)
	pipe.pingUntilFirstToken(c)
	result, err := s.callGeminiWebWithCallback(ctx, account, mapped, req.plan, thinking, pipe.OnResult)
	if err == nil {
		err = s.checkGeminiWebCannedReply(ctx, account, result, pipe.canned)
	}
	if err != nil {
		started := pipe.Abort(err)
		if ctxErr := c.Request.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Only first-token pings reached the client: another account can
		// still answer on the same response.
		if !started && GeminiWebPingAdjustedWrittenSize(c) == sizeBefore {
			return nil, geminiWebFailoverError(err)
		}
		return nil, geminiWebCommittedStreamError(c, err, req.writeStreamError)
	}
	usage := webUsageWithThinking(req.plan, result.Text, result.Thinking)
	streamResult, err := pipe.Finish(result, usage)
	if err != nil {
		return nil, err
	}
	return geminiWebCompatForwardResult(req, mapped, result, usage, streamResult), nil
}

// geminiWebCheckCannedReply turns Gemini Web's canned failure message into
// ErrCannedReply. The forward then fails, so the reply is never billed: it
// switches accounts while the client has received no content, and otherwise
// ends the stream with an error.
func geminiWebCheckCannedReply(ctx context.Context, account *Account, result geminiweb.Result, canned geminiweb.CannedReplies) error {
	phrase := canned.Is(result.Text)
	if !result.GenerationFailed && !phrase {
		return nil
	}
	logger.FromContext(ctx).Warn("gemini_web.canned_reply",
		zap.Int64("account_id", account.ID),
		zap.Bool("generation_failed", result.GenerationFailed),
		zap.Bool("phrase_match", phrase),
		zap.String("text", result.Text))
	return geminiweb.ErrCannedReply
}

// geminiWebStreamErrorStatus is the status and client message for an error
// reported inside an already committed stream.
func geminiWebStreamErrorStatus(cause error) (int, string) {
	switch {
	case errors.Is(cause, geminiweb.ErrInvalidCredential):
		return http.StatusUnauthorized, GeminiWebCredentialRejectedClientMessage
	case errors.Is(cause, geminiweb.ErrRateLimited):
		return http.StatusTooManyRequests, "Gemini Web account is rate limited; retry later"
	case errors.Is(cause, geminiweb.ErrCannedReply):
		return http.StatusBadGateway, GeminiWebCannedReplyClientMessage
	}
	return http.StatusBadGateway, "Gemini Web upstream request failed"
}

// geminiWebHoldPossibleCannedReply holds back streamed answer text while it is
// still the start of a canned failure message, so such a reply never reaches
// the client as content. Once the text diverges it is released. A later
// snapshot that rewrites the text already sent (sent is not its prefix) is
// held again while it could be a canned reply: Gemini Web replaces a failed
// answer wholesale. A snapshot Google flags as a failed generation is always
// held.
func geminiWebHoldPossibleCannedReply(result geminiweb.Result, text, sent string, released bool, canned geminiweb.CannedReplies) (string, bool) {
	if text == "" || result.GenerationFailed {
		return "", released
	}
	if released && strings.HasPrefix(text, sent) {
		return text, true
	}
	if canned.CouldBecome(text) {
		return "", released
	}
	return text, true
}

func geminiWebCompatForwardResult(req geminiWebCompatRequest, mapped string, result geminiweb.Result, usage ClaudeUsage, stream *geminiStreamResult) *ForwardResult {
	out := &ForwardResult{
		RequestID: result.RequestID, UpstreamHeaders: make(http.Header), Usage: usage,
		Model: req.model, UpstreamModel: mapped, Stream: req.stream, Duration: time.Since(req.start),
		UpstreamResponseModel: result.ResponseModel, UpstreamResponseModelConflict: result.ResponseModelConflict,
	}
	if stream != nil {
		out.FirstTokenMs = stream.firstTokenMs
	}
	return out
}

// geminiWebCommittedStreamError reports an upstream failure on a stream that
// has already sent output. Like the native writer, it must not be wrapped as a
// failover error once the client has received bytes.
func geminiWebCommittedStreamError(c *gin.Context, cause error, write func(string)) error {
	status, message := geminiWebStreamErrorStatus(cause)
	SetOpsUpstreamError(c, status, message, "")
	MarkOpsStreamFailure(c, "upstream_error", "gemini_web", message, status)
	write(message)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return errors.New(message)
}
