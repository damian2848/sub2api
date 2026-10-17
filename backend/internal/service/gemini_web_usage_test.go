package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

type geminiWebUsageHTTPStub struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (h *geminiWebUsageHTTPStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.do(req)
}

func TestGeminiWebUsageAllowsSlowQueriesAndForcesFreshQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, bootstraps := 0, 0
		upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
			body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
			if req.Method == http.MethodGet {
				bootstraps++
			} else {
				queries++
				select {
				case <-time.After(5 * time.Second):
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				fraction := "0.25"
				if queries == 2 {
					fraction = "0.5"
				}
				payload, _ := json.Marshal([]any{2, []any{
					[]any{"", fraction, 1, []any{"1730000000", "0"}},
					[]any{"", "0.75", 2, []any{"1731000000", "0"}},
				}})
				outer, _ := json.Marshal([]any{[]any{"wrb.fr", "jSf9Qc", string(payload)}})
				body = ")]}'\n" + string(outer)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}}
		svc := &AccountUsageService{httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
		account := &Account{ID: 42, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
			Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
		usage, err := svc.getGeminiWebUsage(context.Background(), account, false)
		if err != nil || usage.Error != "" || usage.FiveHour == nil || usage.SevenDay == nil {
			t.Fatalf("slow manual query failed: usage=%#v error=%v", usage, err)
		}
		if usage.FiveHour.Utilization != 25 || usage.SevenDay.Utilization != 75 {
			t.Fatalf("unexpected initial usage: five_hour=%#v seven_day=%#v", usage.FiveHour, usage.SevenDay)
		}
		usage, err = svc.getGeminiWebUsage(context.Background(), account, true)
		if err != nil || usage.Error != "" || usage.FiveHour == nil || usage.FiveHour.Utilization != 50 {
			t.Fatalf("forced query did not return fresh quota: usage=%#v error=%v", usage, err)
		}
		if queries != 2 || bootstraps != 1 {
			t.Fatalf("usage requests: queries=%d bootstraps=%d, want 2 and 1", queries, bootstraps)
		}
	})
}

func TestGeminiWebUsageTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}}
		svc := &AccountUsageService{httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
		account := &Account{Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
		start := time.Now()
		usage, err := svc.getGeminiWebUsage(context.Background(), account, true)
		if err != nil || usage.ErrorCode != "network_error" || usage.Error == "" {
			t.Fatalf("timeout was not reported: usage=%#v error=%v", usage, err)
		}
		if elapsed := time.Since(start); elapsed != 20*time.Second {
			t.Fatalf("manual quota timeout = %v, want 20s", elapsed)
		}
	})
}
