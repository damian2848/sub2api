package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	// geminiWebDiagnosticsMaxFiles caps the saved raw responses; the oldest
	// files are removed first.
	geminiWebDiagnosticsMaxFiles = 200
	// geminiWebDiagnosticsMaxRawBytes caps one saved raw response.
	geminiWebDiagnosticsMaxRawBytes = 4 << 20
)

// geminiWebDiagnosticsDir is where raw responses of canned or suspected canned
// replies are saved, next to request captures under DATA_DIR. Tests replace it.
var geminiWebDiagnosticsDir = func() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "gemini-web-diagnostics")
}

var geminiWebDiagnosticsMu sync.Mutex

func (s *GeminiMessagesCompatService) geminiWebCannedReplies(ctx context.Context) geminiweb.CannedReplies {
	if s == nil || s.settingService == nil {
		return geminiweb.CannedReplies{}
	}
	return s.settingService.GeminiWebCannedReplies(ctx)
}

// observeGeminiWebShortReply logs every short Gemini Web reply in full, so new
// canned failure phrases can be spotted, and saves the raw response when the
// reply is a known canned phrase or worded like one.
func (s *GeminiMessagesCompatService) observeGeminiWebShortReply(ctx context.Context, account *Account, model string, result geminiweb.Result) {
	if utf8.RuneCountInString(result.Text) > geminiweb.SuspectedCannedReplyMaxChars && !result.GenerationFailed {
		return
	}
	canned := result.GenerationFailed || s.geminiWebCannedReplies(ctx).Is(result.Text)
	suspected := !canned && geminiweb.SuspectedCannedReply(result.Text)
	logger.FromContext(ctx).Info("gemini_web.short_reply",
		zap.Int64("account_id", account.ID),
		zap.String("model", model),
		zap.String("response_model", result.ResponseModel),
		zap.Int("prompt_chars", result.Timing.PromptChars),
		zap.Int("text_chars", utf8.RuneCountInString(result.Text)),
		zap.Bool("canned", canned),
		zap.Bool("generation_failed", result.GenerationFailed),
		zap.Bool("suspected", suspected),
		zap.String("text", result.Text),
	)
	if !canned && !suspected {
		return
	}
	entry := geminiWebDiagnostic{
		Time: time.Now().UTC(), AccountID: account.ID, Model: model, ResponseModel: result.ResponseModel,
		PromptChars: result.Timing.PromptChars, Canned: canned, Suspected: suspected, Text: result.Text,
		Raw: string(truncateBytes(result.Raw, geminiWebDiagnosticsMaxRawBytes)),
	}
	dir := geminiWebDiagnosticsDir()
	l := logger.FromContext(ctx)
	go func() {
		defer func() { _ = recover() }()
		path, err := saveGeminiWebDiagnostic(dir, entry)
		if err != nil {
			l.Warn("gemini_web.diagnostic_save_failed", zap.Int64("account_id", entry.AccountID), zap.Error(err))
			return
		}
		l.Info("gemini_web.diagnostic_saved", zap.Int64("account_id", entry.AccountID), zap.String("path", path))
	}()
}

// geminiWebDiagnostic is one saved reply. It holds the response only, never
// the prompt or credentials.
type geminiWebDiagnostic struct {
	Time          time.Time `json:"time"`
	AccountID     int64     `json:"account_id"`
	Model         string    `json:"model"`
	ResponseModel string    `json:"response_model"`
	PromptChars   int       `json:"prompt_chars"`
	Canned        bool      `json:"canned"`
	Suspected     bool      `json:"suspected"`
	Text          string    `json:"text"`
	Raw           string    `json:"raw"`
}

func saveGeminiWebDiagnostic(dir string, entry geminiWebDiagnostic) (string, error) {
	geminiWebDiagnosticsMu.Lock()
	defer geminiWebDiagnosticsMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	encoded, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-account%d-%d.json", entry.Time.Format("20060102T150405Z"), entry.AccountID, entry.Time.UnixNano()%1_000_000_000)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return "", err
	}
	pruneGeminiWebDiagnostics(dir, geminiWebDiagnosticsMaxFiles)
	return path, nil
}

// pruneGeminiWebDiagnostics keeps the newest keep files. Names start with a
// UTC timestamp, so name order is age order.
func pruneGeminiWebDiagnostics(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func truncateBytes(b []byte, limit int) []byte {
	if len(b) > limit {
		return b[:limit]
	}
	return b
}
