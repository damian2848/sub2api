package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Every Gemini Web call is recorded as one JSON line under
// $DATA_DIR/gemini-web-analytics/attempts-YYYYMMDD.jsonl (UTC dates, 30 days
// kept), so the rate of canned failure replies can be studied over weeks:
// container logs only hold about a day. backend/scripts/gemini_web_attempts_report.py
// summarizes the files. A record holds request facts and the outcome, never
// the prompt or credentials; reply text is kept only for short canned,
// refusal and suspected replies.
const (
	geminiWebAnalyticsRetention  = 30 * 24 * time.Hour
	geminiWebAnalyticsFilePrefix = "attempts-"
)

// geminiWebAnalyticsDir is where attempt records are written. Tests replace it.
var geminiWebAnalyticsDir = func() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "gemini-web-analytics")
}

var geminiWebAnalytics = &geminiWebAnalyticsWriter{}

type geminiWebAnalyticsWriter struct {
	mu         sync.Mutex
	prunedDay  string
	lastFailed time.Time
}

// geminiWebAttemptRecord is one Gemini Web call (type "attempt") or one
// account cooldown (type "cooldown").
type geminiWebAttemptRecord struct {
	Type      string    `json:"type"`
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id,omitempty"`
	UserID    int64     `json:"user_id,omitempty"`
	GroupID   int64     `json:"group_id,omitempty"`
	AccountID int64     `json:"account_id"`

	Model         string `json:"model,omitempty"`
	ResponseModel string `json:"response_model,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Stream        bool   `json:"stream,omitempty"`
	Tools         int    `json:"tools,omitempty"`
	Thinking      bool   `json:"thinking,omitempty"`
	PromptChars   int    `json:"prompt_chars,omitempty"`
	// Attempt is the account switch count of the request: 0 for the first
	// account, 1 after one failover, and so on.
	Attempt int `json:"attempt"`
	// AccountInflight counts Gemini Web calls running on the account when this
	// one started, including itself.
	AccountInflight int `json:"account_inflight,omitempty"`
	// SinceAccountLastMs is the time since the previous call on the account
	// started; -1 when none was seen since the process started.
	SinceAccountLastMs int64 `json:"since_account_last_ms"`

	Outcome          string `json:"outcome,omitempty"`
	GenerationFailed bool   `json:"generation_failed,omitempty"`
	PhraseMatch      bool   `json:"phrase_match,omitempty"`
	Refusal          bool   `json:"refusal,omitempty"`
	Suspected        bool   `json:"suspected,omitempty"`
	UpstreamStatus   int    `json:"upstream_status,omitempty"`
	TextChars        int    `json:"text_chars"`
	Text             string `json:"text,omitempty"`
	Error            string `json:"error,omitempty"`

	SessionMs   int64 `json:"session_ms"`
	HeadersMs   int64 `json:"headers_ms"`
	FirstTextMs int64 `json:"first_text_ms"`
	TotalMs     int64 `json:"total_ms"`

	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

// geminiWebCallInfo describes the client request behind a Gemini Web call;
// the forward paths attach it to the context for the attempt record.
type geminiWebCallInfo struct {
	Protocol string
	Stream   bool
	Tools    int
}

type geminiWebCallInfoKey struct{}

func withGeminiWebCallInfo(ctx context.Context, info geminiWebCallInfo) context.Context {
	return context.WithValue(ctx, geminiWebCallInfoKey{}, info)
}

func geminiWebCallInfoFrom(ctx context.Context) geminiWebCallInfo {
	info, _ := ctx.Value(geminiWebCallInfoKey{}).(geminiWebCallInfo)
	return info
}

// geminiWebAccountActivity tracks in-flight calls and the last call start per
// account, process-local.
type geminiWebAccountActivity struct {
	mu       sync.Mutex
	inflight map[int64]int
	last     map[int64]time.Time
}

// begin registers a call and returns the in-flight count (including it), the
// time since the previous call on the account (-1 if none) and a done func.
func (a *geminiWebAccountActivity) begin(accountID int64, now time.Time) (int, int64, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight == nil {
		a.inflight = make(map[int64]int)
		a.last = make(map[int64]time.Time)
	}
	since := int64(-1)
	if last, ok := a.last[accountID]; ok {
		since = now.Sub(last).Milliseconds()
	}
	a.last[accountID] = now
	a.inflight[accountID]++
	inflight := a.inflight[accountID]
	var once sync.Once
	return inflight, since, func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.inflight[accountID]--; a.inflight[accountID] <= 0 {
				delete(a.inflight, accountID)
			}
		})
	}
}

// geminiWebAttemptOutcome classifies a call for analysis.
func geminiWebAttemptOutcome(result geminiweb.Result, err error, phrase bool) string {
	switch {
	case err == nil && (result.GenerationFailed || phrase):
		return "canned"
	case err == nil && geminiweb.IsPolicyRefusal(result.Text):
		return "refusal"
	case err == nil:
		return "ok"
	case errors.Is(err, geminiweb.ErrInvalidCredential):
		return "credential"
	case errors.Is(err, geminiweb.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, geminiweb.ErrEmptyResponse):
		return "empty"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case strings.Contains(err.Error(), "BardErrorInfo"):
		return "bard_error"
	case strings.Contains(err.Error(), "session rejected"):
		return "session_rejected"
	}
	return "error"
}

func (s *GeminiMessagesCompatService) recordGeminiWebAttempt(ctx context.Context, account *Account, model string, thinking bool, inflight int, sinceLast int64, result geminiweb.Result, err error) {
	if account == nil {
		return
	}
	phrase := err == nil && s.geminiWebCannedReplies(ctx).Is(result.Text)
	info := geminiWebCallInfoFrom(ctx)
	t := result.Timing
	rec := geminiWebAttemptRecord{
		Type: "attempt", Time: time.Now().UTC(), AccountID: account.ID,
		Model: model, ResponseModel: result.ResponseModel, Protocol: info.Protocol, Stream: info.Stream,
		Tools: info.Tools, Thinking: thinking, PromptChars: t.PromptChars,
		AccountInflight: inflight, SinceAccountLastMs: sinceLast,
		Outcome: geminiWebAttemptOutcome(result, err, phrase), GenerationFailed: result.GenerationFailed,
		PhraseMatch: phrase, UpstreamStatus: t.Status, TextChars: utf8.RuneCountInString(result.Text),
		SessionMs: geminiWebStageMs(t.Session), HeadersMs: geminiWebStageMs(t.Headers),
		FirstTextMs: geminiWebStageMs(t.FirstText), TotalMs: geminiWebStageMs(t.Total),
	}
	geminiWebFillRequestFacts(ctx, &rec)
	if err != nil {
		rec.Error = truncateRunes(err.Error(), 200)
	} else {
		rec.Refusal = geminiweb.IsPolicyRefusal(result.Text)
		rec.Suspected = geminiweb.SuspectedCannedReply(result.Text)
		if (rec.Outcome != "ok" || rec.Suspected) && rec.TextChars <= 2*geminiweb.SuspectedCannedReplyMaxChars {
			rec.Text = result.Text
		}
	}
	geminiWebAnalytics.write(ctx, rec)
}

func recordGeminiWebCooldown(ctx context.Context, account *Account, until time.Time) {
	rec := geminiWebAttemptRecord{Type: "cooldown", Time: time.Now().UTC(), AccountID: account.ID, SinceAccountLastMs: -1, CooldownUntil: &until}
	geminiWebFillRequestFacts(ctx, &rec)
	geminiWebAnalytics.write(ctx, rec)
}

func geminiWebFillRequestFacts(ctx context.Context, rec *geminiWebAttemptRecord) {
	rec.RequestID, _ = ctx.Value(ctxkey.RequestID).(string)
	rec.UserID, _ = ctx.Value(ctxkey.UserID).(int64)
	if group, ok := ctx.Value(ctxkey.Group).(*Group); ok && group != nil {
		rec.GroupID = group.ID
	}
	if switches, ok := AccountSwitchCountFromContext(ctx); ok {
		rec.Attempt = switches
	}
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

// write appends rec in the background; recording must never slow down or
// fail a request.
func (w *geminiWebAnalyticsWriter) write(ctx context.Context, rec geminiWebAttemptRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	dir := geminiWebAnalyticsDir()
	l := logger.FromContext(ctx)
	go func() {
		defer func() { _ = recover() }()
		if err := w.append(dir, rec.Time, line); err != nil {
			w.mu.Lock()
			report := time.Since(w.lastFailed) > time.Minute
			if report {
				w.lastFailed = time.Now()
			}
			w.mu.Unlock()
			if report {
				l.Warn("gemini_web.analytics_write_failed", zap.Error(err))
			}
		}
	}()
}

func (w *geminiWebAnalyticsWriter) append(dir string, at time.Time, line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	day := at.UTC().Format("20060102")
	f, err := os.OpenFile(filepath.Join(dir, geminiWebAnalyticsFilePrefix+day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if w.prunedDay != day {
		w.prunedDay = day
		pruneGeminiWebAnalytics(dir, at, geminiWebAnalyticsRetention)
	}
	return err
}

// pruneGeminiWebAnalytics removes attempt files older than retention. File
// names carry the UTC date, so no stat is needed.
func pruneGeminiWebAnalytics(dir string, now time.Time, retention time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.UTC().Add(-retention).Format("20060102")
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, geminiWebAnalyticsFilePrefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		day := strings.TrimSuffix(strings.TrimPrefix(name, geminiWebAnalyticsFilePrefix), ".jsonl")
		if day < cutoff {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
