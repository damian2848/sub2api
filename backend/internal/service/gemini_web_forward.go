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

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/googleapi"
	"github.com/gin-gonic/gin"
)

const (
	GeminiWebCredentialRejectedReason        GatewayFailureReason = "gemini_web_credential_rejected"
	GeminiWebCredentialRejectedClientMessage string               = "Gemini Web Cookie is invalid or expired; update the account Cookie and retry"
)

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
	return s.callGeminiWebWithCallback(ctx, account, model, prompt, thinking, nil)
}

func (s *GeminiMessagesCompatService) callGeminiWebWithCallback(ctx context.Context, account *Account, model, prompt string, thinking bool, onResult func(geminiweb.Result) error) (geminiweb.Result, error) {
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
	authUser, authUserSet := geminiWebAuthUser(account)
	cred := geminiweb.Credential{
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
	result, err := s.geminiWebClient().Complete(requestCtx, cred, geminiweb.Request{Model: model, Prompt: prompt, Thinking: thinking, OnResult: onResult}, do)
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
	if err != nil {
		if errors.Is(err, geminiweb.ErrInvalidCredential) {
			s.geminiWebClient().Invalidate(cookie)
			if s.accountRepo != nil && account.ID > 0 {
				until := time.Now().Add(10 * time.Minute)
				_ = s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, "Gemini Web cookie expired or rejected")
				account.TempUnschedulableUntil = &until
			}
		} else if errors.Is(err, geminiweb.ErrRateLimited) && s.accountRepo != nil && account.ID > 0 {
			now := time.Now()
			resetAt := now.Add(time.Minute)
			_ = s.accountRepo.SetRateLimited(ctx, account.ID, resetAt)
			account.RateLimitedAt = &now
			account.RateLimitResetAt = &resetAt
		}
		return result, err
	}
	return result, nil
}

func webUsageWithThinking(prompt, answer, thinking string) ClaudeUsage {
	return ClaudeUsage{InputTokens: estimateTokensForText(prompt), OutputTokens: estimateTokensForText(answer) + estimateTokensForText(thinking)}
}

func (s *GeminiMessagesCompatService) forwardGeminiWebNative(ctx context.Context, c *gin.Context, account *Account, originalModel, action string, stream bool, body []byte) (*ForwardResult, error) {
	start := time.Now()
	if action == "countTokens" {
		prompt, err := geminiWebPrompt(body)
		if err != nil {
			return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
		}
		c.JSON(http.StatusOK, gin.H{"totalTokens": estimateTokensForText(prompt)})
		return &ForwardResult{Model: originalModel, UpstreamModel: account.GetMappedModel(originalModel), Usage: ClaudeUsage{}}, nil
	}
	if action != "generateContent" && action != "streamGenerateContent" {
		return nil, s.writeGoogleError(c, http.StatusNotFound, "Unsupported action: "+action)
	}
	plan, err := geminiWebBuildPrompt(body)
	if err != nil {
		return nil, s.writeGoogleError(c, http.StatusBadRequest, err.Error())
	}
	prompt := plan.Prompt
	mapped := account.GetMappedModel(originalModel)
	var streamWriter *geminiWebNativeStreamWriter
	var onResult func(geminiweb.Result) error
	if stream || action == "streamGenerateContent" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		streamWriter = newGeminiWebNativeStreamWriter(c, originalModel, cancel)
		streamWriter.plan = plan
		streamWriter.start = start
		onResult = streamWriter.WriteResult
	}
	result, err := s.callGeminiWebWithCallback(ctx, account, mapped, prompt, strings.Contains(strings.ToLower(originalModel), "thinking"), onResult)
	if err != nil {
		if streamWriter != nil {
			return nil, streamWriter.Abort(err)
		}
		return nil, geminiWebFailoverError(err)
	}
	usage := webUsageWithThinking(prompt, result.Text, result.Thinking)
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
	if errors.Is(err, geminiweb.ErrInvalidCredential) {
		reason = GeminiWebCredentialRejectedReason
		message = GeminiWebCredentialRejectedClientMessage
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
	c             *gin.Context
	model         string
	plan          geminiWebPromptPlan
	start         time.Time
	firstTokenMs  *int
	mu            sync.Mutex
	text          string
	thinking      string
	committed     bool
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
	}
	go w.keepAlive()
	return w
}

func (w *geminiWebNativeStreamWriter) keepAlive() {
	ticker := time.NewTicker(5 * time.Second)
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
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.c.Request != nil && w.c.Request.Context().Err() != nil {
		return w.c.Request.Context().Err()
	}
	w.commitLocked()
	n, err := w.c.Writer.Write(data)
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
	// comment heartbeats are rejected by some older versions.
	return w.writeLocked([]byte("\n\n"))
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
	return w.writeSnapshotLocked(geminiWebStreamableText(result.Text, w.plan.toolsEnabled()), result.Thinking, nil, nil)
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
	// Account switching is only safe before any response has been sent. The
	// concurrency-wait path may have committed a heartbeat before this writer
	// was created, so inspect the Gin writer as well as our own state.
	if !w.committed && !w.c.Writer.Written() {
		return geminiWebFailoverError(cause)
	}
	status, message := http.StatusBadGateway, "Gemini Web upstream request failed"
	if errors.Is(cause, geminiweb.ErrInvalidCredential) {
		status, message = http.StatusUnauthorized, GeminiWebCredentialRejectedClientMessage
	} else if errors.Is(cause, geminiweb.ErrRateLimited) {
		status, message = http.StatusTooManyRequests, "Gemini Web account is rate limited; retry later"
	}
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
		model: req.Model, stream: req.Stream, plan: plan, start: start,
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
		model: req.Model, stream: req.Stream, plan: plan, start: start,
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
	mapped := account.GetMappedModel(req.model)
	thinking := strings.Contains(strings.ToLower(req.model), "thinking")
	if !req.stream {
		result, err := s.callGeminiWeb(ctx, account, mapped, req.plan.Prompt, thinking)
		if err != nil {
			return nil, geminiWebFailoverError(err)
		}
		usage := webUsageWithThinking(req.plan.Prompt, result.Text, result.Thinking)
		text, calls := geminiWebExtractToolCalls(result.Text, req.plan)
		raw, geminiResp := geminiWebGeminiResponse(text, calls, usage, result.ResponseModel)
		req.writeJSON(raw, geminiResp, usage)
		return geminiWebCompatForwardResult(req, mapped, result, usage, nil), nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pipe := newGeminiWebPipeStream(req.plan, req.includeThought, cancel, req.handleStream)
	pipe.pingUntilFirstToken(c)
	result, err := s.callGeminiWebWithCallback(ctx, account, mapped, req.plan.Prompt, thinking, pipe.OnResult)
	if err != nil {
		if !pipe.Abort(err) && !c.Writer.Written() {
			if ctxErr := c.Request.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, geminiWebFailoverError(err)
		}
		if ctxErr := c.Request.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, geminiWebCommittedStreamError(c, err, req.writeStreamError)
	}
	usage := webUsageWithThinking(req.plan.Prompt, result.Text, result.Thinking)
	streamResult, err := pipe.Finish(result, usage)
	if err != nil {
		return nil, err
	}
	return geminiWebCompatForwardResult(req, mapped, result, usage, streamResult), nil
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
	status, message := http.StatusBadGateway, "Gemini Web upstream request failed"
	if errors.Is(cause, geminiweb.ErrInvalidCredential) {
		status, message = http.StatusUnauthorized, GeminiWebCredentialRejectedClientMessage
	} else if errors.Is(cause, geminiweb.ErrRateLimited) {
		status, message = http.StatusTooManyRequests, "Gemini Web account is rate limited; retry later"
	}
	SetOpsUpstreamError(c, status, message, "")
	MarkOpsStreamFailure(c, "upstream_error", "gemini_web", message, status)
	write(message)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return errors.New(message)
}
