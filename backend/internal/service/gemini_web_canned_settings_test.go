package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Tests that hit canned replies must not write raw captures into the package
// directory; tests that check captures use useGeminiWebDiagnosticsDir.
func init() {
	dir := filepath.Join(os.TempDir(), "sub2api-test-gemini-web-diagnostics")
	geminiWebDiagnosticsDir = func() string { return dir }
	analytics := filepath.Join(os.TempDir(), "sub2api-test-gemini-web-analytics")
	geminiWebAnalyticsDir = func() string { return analytics }
}

// geminiWebCannedSettingRepo is a map-backed setting store; getErr makes
// reads fail like a database outage.
type geminiWebCannedSettingRepo struct {
	SettingRepository
	mu     sync.Mutex
	values map[string]string
	getErr error
}

func (r *geminiWebCannedSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return "", r.getErr
	}
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *geminiWebCannedSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

func newGeminiWebCannedSettings() (*SettingService, *geminiWebCannedSettingRepo) {
	repo := &geminiWebCannedSettingRepo{values: map[string]string{}}
	return NewSettingService(repo, &config.Config{}), repo
}

func TestGeminiWebCannedReplySettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, repo := newGeminiWebCannedSettings()

	got, err := svc.GetGeminiWebCannedReplySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, geminiweb.BuiltInCannedReplies(), got.BuiltIn)
	require.Empty(t, got.Custom)
	require.False(t, svc.GeminiWebCannedReplies(ctx).Is("Something went wrong."))

	saved, err := svc.SetGeminiWebCannedReplySettings(ctx, []string{
		"  Something went wrong.  ", "", "something   WENT wrong.",
		"I seem to be encountering an error. Can I try something else for you?",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"Something went wrong."}, saved, "trimmed, blanks and duplicates (incl. built-ins) dropped")
	require.JSONEq(t, `["Something went wrong."]`, repo.values[SettingKeyGeminiWebCannedReplies])
	require.True(t, svc.GeminiWebCannedReplies(ctx).Is("something went wrong."), "applies immediately")

	got, err = svc.GetGeminiWebCannedReplySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"Something went wrong."}, got.Custom)
}

func TestGeminiWebCannedReplySettingsValidation(t *testing.T) {
	ctx := context.Background()
	svc, repo := newGeminiWebCannedSettings()
	_, err := svc.SetGeminiWebCannedReplySettings(ctx, []string{strings.Repeat("x", geminiWebCannedReplyMaxChars+1)})
	require.Error(t, err)
	many := make([]string, geminiWebCannedReplyMaxPhrases+1)
	for i := range many {
		many[i] = "phrase " + strings.Repeat("y", i+1)
	}
	_, err = svc.SetGeminiWebCannedReplySettings(ctx, many)
	require.Error(t, err)
	require.NotContains(t, repo.values, SettingKeyGeminiWebCannedReplies, "rejected lists are not stored")
}

func TestGeminiWebCannedRepliesFallBackToBuiltInsOnReadError(t *testing.T) {
	svc, repo := newGeminiWebCannedSettings()
	repo.getErr = errors.New("database down")
	matcher := svc.GeminiWebCannedReplies(context.Background())
	require.True(t, matcher.Is("I encountered an error doing what you asked. Could you try again?"))
}

// A phrase added in settings fails a reply exactly like a built-in one.
func TestGeminiWebCustomCannedPhraseFailsRequest(t *testing.T) {
	settings, _ := newGeminiWebCannedSettings()
	_, err := settings.SetGeminiWebCannedReplySettings(context.Background(), []string{"Gemini is busy, try later."})
	require.NoError(t, err)
	frames, _ := geminiWebFramesStub(t, "Gemini is", "Gemini is busy, try later.")
	svc := &GeminiMessagesCompatService{httpUpstream: frames, settingService: settings}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	result, err := svc.Forward(context.Background(), c, geminiWebTestAccount(), geminiWebCannedRequest("messages", true, false))
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover), "err = %v", err)
	require.Equal(t, GeminiWebCannedReplyReason, failover.Reason)
	require.NotContains(t, recorder.Body.String(), "Gemini is", "held text never reached the client")
}

func useGeminiWebDiagnosticsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := geminiWebDiagnosticsDir
	geminiWebDiagnosticsDir = func() string { return dir }
	t.Cleanup(func() { geminiWebDiagnosticsDir = previous })
	return dir
}

func geminiWebDiagnosticFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	return matches
}

func TestGeminiWebShortReplyLoggingAndRawCapture(t *testing.T) {
	for _, tc := range []struct {
		name, text           string
		canned, suspected    bool
		wantLog, wantCapture bool
	}{
		{"normal short", "Hello! How can I help you today?", false, false, true, false},
		{"suspected", "Sorry, something went wrong. Please try again later.", false, true, true, true},
		{"canned", geminiWebCannedText, true, false, true, true},
		{"long", strings.Repeat("a long and perfectly normal answer ", 20), false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := useGeminiWebDiagnosticsDir(t)
			core, logs := observer.New(zap.InfoLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			frames, _ := geminiWebFramesStub(t, tc.text)
			svc := &GeminiMessagesCompatService{httpUpstream: frames}
			account := geminiWebTestAccount()
			account.ID = 245

			result, err := svc.callGeminiWeb(ctx, account, "gemini-4-argon", "draw a pelican", false)
			require.NoError(t, err)
			require.Equal(t, tc.text, result.Text)

			entries := logs.FilterMessage("gemini_web.short_reply").All()
			if !tc.wantLog {
				require.Empty(t, entries)
			} else {
				require.Len(t, entries, 1)
				fields := entries[0].ContextMap()
				require.Equal(t, tc.text, fields["text"], "the reply is logged in full")
				require.Equal(t, int64(245), fields["account_id"])
				require.Equal(t, tc.canned, fields["canned"])
				require.Equal(t, tc.suspected, fields["suspected"])
			}

			if !tc.wantCapture {
				time.Sleep(50 * time.Millisecond)
				require.Empty(t, geminiWebDiagnosticFiles(t, dir))
				return
			}
			require.Eventually(t, func() bool { return len(geminiWebDiagnosticFiles(t, dir)) == 1 }, 2*time.Second, 10*time.Millisecond)
			raw, err := os.ReadFile(geminiWebDiagnosticFiles(t, dir)[0])
			require.NoError(t, err)
			var saved geminiWebDiagnostic
			require.NoError(t, json.Unmarshal(raw, &saved))
			require.Equal(t, tc.text, saved.Text)
			require.Equal(t, int64(245), saved.AccountID)
			require.Contains(t, saved.Raw, "wrb.fr", "the raw upstream body is kept")
			require.NotContains(t, string(raw), "draw a pelican", "the prompt is never saved")
			require.NotContains(t, string(raw), "psid", "credentials are never saved")
		})
	}
}

func TestPruneGeminiWebDiagnosticsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20261018T010000Z-a.json", "20261018T020000Z-b.json", "20261018T030000Z-c.json", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
	}
	pruneGeminiWebDiagnostics(dir, 2)
	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	require.ElementsMatch(t, []string{"20261018T020000Z-b.json", "20261018T030000Z-c.json", "notes.txt"}, names)
}
