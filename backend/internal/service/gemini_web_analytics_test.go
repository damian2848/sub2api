package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/stretchr/testify/require"
)

func useGeminiWebAnalyticsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := geminiWebAnalyticsDir
	geminiWebAnalyticsDir = func() string { return dir }
	t.Cleanup(func() { geminiWebAnalyticsDir = previous })
	return dir
}

func readGeminiWebAttempts(t *testing.T, dir string, want int) []geminiWebAttemptRecord {
	t.Helper()
	var records []geminiWebAttemptRecord
	require.Eventually(t, func() bool {
		records = nil
		matches, _ := filepath.Glob(filepath.Join(dir, "attempts-*.jsonl"))
		for _, path := range matches {
			f, err := os.Open(path)
			if err != nil {
				return false
			}
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				var rec geminiWebAttemptRecord
				if json.Unmarshal(scanner.Bytes(), &rec) == nil {
					records = append(records, rec)
				}
			}
			_ = f.Close()
		}
		return len(records) >= want
	}, 2*time.Second, 10*time.Millisecond)
	return records
}

func geminiWebAnalyticsContext() context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-analytics")
	ctx = context.WithValue(ctx, ctxkey.UserID, int64(43))
	ctx = context.WithValue(ctx, ctxkey.Group, &Group{ID: 51})
	return WithAccountSwitchCount(ctx, 1, false)
}

// Each Gemini Web call becomes one JSON line with the request facts and the
// outcome; canned reply text is kept, ordinary answers are not.
func TestGeminiWebAttemptRecords(t *testing.T) {
	dir := useGeminiWebAnalyticsDir(t)
	upstream, _ := geminiWebAttemptsStub(t, 0,
		[]geminiWebFlaggedFrame{{text: geminiWebCannedText, failed: true}},
		[]geminiWebFlaggedFrame{{text: "Here is the pelican."}})
	svc := &GeminiMessagesCompatService{httpUpstream: upstream}
	account := geminiWebTestAccount()
	account.ID = 245
	body := []byte(`{"tools":[{"functionDeclarations":[{"name":"write_file"}]}],"contents":[{"role":"user","parts":[{"text":"draw a pelican"}]}]}`)

	for range 2 {
		_, c := geminiWebTestContext()
		c.Request = c.Request.WithContext(geminiWebAnalyticsContext())
		_, _ = svc.ForwardNative(c.Request.Context(), c, account, "gemini-4-argon", "generateContent", false, body)
	}

	records := readGeminiWebAttempts(t, dir, 2)
	require.Len(t, records, 2)
	byOutcome := map[string]geminiWebAttemptRecord{}
	for _, rec := range records {
		byOutcome[rec.Outcome] = rec
	}
	canned, ok := byOutcome["canned"]
	require.True(t, ok, "records = %+v", records)
	require.Equal(t, "attempt", canned.Type)
	require.Equal(t, "req-analytics", canned.RequestID)
	require.Equal(t, int64(43), canned.UserID)
	require.Equal(t, int64(51), canned.GroupID)
	require.Equal(t, int64(245), canned.AccountID)
	require.Equal(t, "native", canned.Protocol)
	require.Equal(t, 1, canned.Tools)
	require.Equal(t, 1, canned.Attempt)
	require.True(t, canned.GenerationFailed)
	require.True(t, canned.PhraseMatch)
	require.Equal(t, geminiWebCannedText, canned.Text)
	require.Positive(t, canned.PromptChars)
	require.Equal(t, int64(-1), canned.SinceAccountLastMs, "first call on the account")

	answer, ok := byOutcome["ok"]
	require.True(t, ok)
	require.Empty(t, answer.Text, "ordinary answers are not stored")
	require.GreaterOrEqual(t, answer.SinceAccountLastMs, int64(0))
	require.Equal(t, 1, answer.AccountInflight)

	raw, err := os.ReadFile(filepath.Join(dir, "attempts-"+time.Now().UTC().Format("20060102")+".jsonl"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "draw a pelican", "the prompt is never recorded")
	require.NotContains(t, string(raw), "psid", "credentials are never recorded")
}

func TestGeminiWebAttemptOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result geminiweb.Result
		err    error
		phrase bool
		want   string
	}{
		{"flag", geminiweb.Result{Text: "anything", GenerationFailed: true}, nil, false, "canned"},
		{"phrase", geminiweb.Result{Text: geminiWebCannedText}, nil, true, "canned"},
		{"refusal", geminiweb.Result{Text: "Your request may go against my [guidelines](https://gemini.google/policy-guidelines/). Would you like to try something else?"}, nil, false, "refusal"},
		{"ok", geminiweb.Result{Text: "Hello"}, nil, false, "ok"},
		{"empty", geminiweb.Result{}, geminiweb.ErrEmptyResponse, false, "empty"},
		{"timeout", geminiweb.Result{}, context.DeadlineExceeded, false, "timeout"},
		{"canceled", geminiweb.Result{}, context.Canceled, false, "canceled"},
		{"credential", geminiweb.Result{}, geminiweb.ErrInvalidCredential, false, "credential"},
		{"rate limited", geminiweb.Result{}, geminiweb.ErrRateLimited, false, "rate_limited"},
		{"session", geminiweb.Result{}, errors.New("gemini web session rejected: 400 Bad Request"), false, "session_rejected"},
		{"other", geminiweb.Result{}, errors.New("dial tcp: refused"), false, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, geminiWebAttemptOutcome(tc.result, tc.err, tc.phrase))
		})
	}
}

func TestGeminiWebAccountActivity(t *testing.T) {
	var activity geminiWebAccountActivity
	start := time.Now()
	n1, since1, done1 := activity.begin(7, start)
	n2, since2, done2 := activity.begin(7, start.Add(3*time.Second))
	require.Equal(t, 1, n1)
	require.Equal(t, int64(-1), since1)
	require.Equal(t, 2, n2, "two calls in flight on the account")
	require.Equal(t, int64(3000), since2)
	done1()
	done1()
	n3, _, done3 := activity.begin(7, start.Add(4*time.Second))
	require.Equal(t, 2, n3, "done is idempotent")
	done2()
	done3()
	n4, _, done4 := activity.begin(8, start)
	require.Equal(t, 1, n4, "accounts are counted separately")
	done4()
}

func TestPruneGeminiWebAnalytics(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"attempts-20261019.jsonl", "attempts-20261021.jsonl", "attempts-20261120.jsonl", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600))
	}
	pruneGeminiWebAnalytics(dir, now, geminiWebAnalyticsRetention)
	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	require.ElementsMatch(t, []string{"attempts-20261021.jsonl", "attempts-20261120.jsonl", "notes.txt"}, names)
}

func TestGeminiWebCooldownIsRecorded(t *testing.T) {
	dir := useGeminiWebAnalyticsDir(t)
	upstream, _ := geminiWebAttemptsStub(t, 0, []geminiWebFlaggedFrame{{text: geminiWebCannedText, failed: true}})
	svc := &GeminiMessagesCompatService{httpUpstream: upstream, accountRepo: &geminiWebCooldownRepo{}}
	account := geminiWebTestAccount()
	account.ID = 246
	for range geminiWebCannedCooldownThreshold {
		_, c := geminiWebTestContext()
		_, _ = svc.ForwardNative(context.Background(), c, account, "gemini-4-argon", "generateContent", false,
			[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	}
	records := readGeminiWebAttempts(t, dir, geminiWebCannedCooldownThreshold+1)
	var cooldowns int
	for _, rec := range records {
		if rec.Type == "cooldown" {
			cooldowns++
			require.Equal(t, int64(246), rec.AccountID)
			require.NotNil(t, rec.CooldownUntil)
		}
	}
	require.Equal(t, 1, cooldowns)
}
