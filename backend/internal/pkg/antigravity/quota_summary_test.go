//go:build unit

package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRetrieveUserQuotaSummaryGroupsAndMissingFraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer quota-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing upstream authentication or content type")
		}
		var request RetrieveUserQuotaSummaryRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Project != "quota-project" {
			t.Errorf("unexpected request body: %+v, %v", request, err)
		}
		_, _ = w.Write([]byte(`{"description":"Current limits","groups":[{"displayName":"Gemini Models","description":"Shared Gemini limits","buckets":[{"bucketId":"gemini-5h","displayName":"Five-hour limit","window":"5h","remainingFraction":0,"resetTime":"2026-10-20T09:00:00Z"},{"bucketId":"gemini-weekly","displayName":"Weekly limit","window":"7d","description":"Resets next week","remainingFraction":0.625}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","remainingAmount":"42"},{"bucketId":"3p-5h","disabled":true}]},{"displayName":"Gemini 4 Argon","buckets":[{"bucketId":"argon-5h","remainingFraction":0.4}]}]}`))
	}))
	defer server.Close()
	withMockBaseURLs(t, []string{server.URL})

	response, raw, err := mustNewClient(t, "").RetrieveUserQuotaSummary(context.Background(), "quota-token", "quota-project", 8192)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Groups) != 3 || response.Description != "Current limits" || raw["groups"] == nil {
		t.Fatalf("quota groups or original response lost: %+v", response)
	}
	gemini := response.Groups[0]
	if gemini.Buckets[0].RemainingFraction == nil || *gemini.Buckets[0].RemainingFraction != 0 || gemini.Buckets[0].ResetTime != "2026-10-20T09:00:00Z" {
		t.Fatalf("explicit exhausted quota/reset missing: %+v", gemini.Buckets[0])
	}
	if *gemini.Buckets[1].RemainingFraction != .625 || gemini.Buckets[1].Description != "Resets next week" {
		t.Fatalf("weekly data lost: %+v", gemini.Buckets[1])
	}
	thirdParty := response.Groups[1].Buckets
	if thirdParty[0].RemainingFraction != nil || thirdParty[0].RemainingAmount == nil || thirdParty[0].RemainingAmount.String() != "42" {
		t.Fatalf("amount must not be fabricated as a fraction: %+v", thirdParty[0])
	}
	if thirdParty[1].RemainingFraction != nil || !thirdParty[1].Disabled {
		t.Fatalf("disabled quota must preserve unknown remaining usage: %+v", thirdParty[1])
	}
}

func TestRetrieveUserQuotaSummaryLegacyBucketsAndEndpointFallback(t *testing.T) {
	firstCalls := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		http.NotFound(w, r)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"buckets":[{"bucketId":"legacy","remainingFraction":1},{"bucketId":"amount","remainingAmount":100}]}`))
	}))
	defer second.Close()
	withMockBaseURLs(t, []string{first.URL, second.URL})

	response, _, err := mustNewClient(t, "").RetrieveUserQuotaSummary(context.Background(), "token", "project", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || len(response.Buckets) != 2 || response.Buckets[1].RemainingAmount.String() != "100" {
		t.Fatalf("legacy quota/fallback missing: %+v, first calls %d", response, firstCalls)
	}
}

func TestRetrieveUserQuotaSummaryDoesNotFallbackOnAuthenticationError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			}))
			defer server.Close()
			fallbackCalls := 0
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls++
			}))
			defer fallback.Close()
			withMockBaseURLs(t, []string{server.URL, fallback.URL})
			_, _, err := mustNewClient(t, "").RetrieveUserQuotaSummary(context.Background(), "token", "project", 4096)
			var httpError *QuotaSummaryHTTPError
			if !errors.As(err, &httpError) || httpError.StatusCode != status || fallbackCalls != 0 {
				t.Fatalf("expected terminal authentication error, got %v and %d fallback calls", err, fallbackCalls)
			}
		})
	}
}

func TestRetrieveUserQuotaSummaryBoundedResponseAndMalformedJSON(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		limit     int64
		wantError string
	}{
		{name: "oversized", body: strings.Repeat("x", 128), limit: 32, wantError: "exceeds"},
		{name: "malformed", body: `{"groups":`, limit: 4096, wantError: "decode"},
		{name: "invalid limit", body: `{}`, limit: 0, wantError: "positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			withMockBaseURLs(t, []string{server.URL})
			_, _, err := mustNewClient(t, "").RetrieveUserQuotaSummary(context.Background(), "token", "project", tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q error, got %v", tc.wantError, err)
			}
		})
	}
}

func TestRetrieveUserQuotaSummaryRejectsExternalRedirect(t *testing.T) {
	externalCalls := 0
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalCalls++
	}))
	defer external.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(external.URL, "127.0.0.1", "localhost", 1), http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	withMockBaseURLs(t, []string{server.URL})
	_, _, err := mustNewClient(t, "").RetrieveUserQuotaSummary(context.Background(), "token", "project", 4096)
	if err == nil || externalCalls != 0 {
		t.Fatalf("credential request followed unsupported redirect: %v, external calls %d", err, externalCalls)
	}
}

func TestRetrieveUserQuotaSummaryFromURLUsesSelectedQuotaPool(t *testing.T) {
	prodCalls, dailyCalls := 0, 0
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodCalls++
		_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"weekly","remainingFraction":1,"resetTime":"2026-10-20T19:39:00Z"}]}]}`))
	}))
	defer prod.Close()
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dailyCalls++
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("unexpected selected endpoint path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer selected-token" {
			t.Error("selected endpoint must receive the account token")
		}
		_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"weekly","remainingFraction":0.02,"resetTime":"2026-10-24T00:19:00Z"}]}]}`))
	}))
	defer daily.Close()
	withMockBaseURLs(t, []string{prod.URL, daily.URL})

	response, _, err := mustNewClient(t, "").RetrieveUserQuotaSummaryFromURL(context.Background(), " "+daily.URL+"/ ", "selected-token", "project", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if prodCalls != 0 || dailyCalls != 1 {
		t.Fatalf("selected daily quota must not be replaced by prod: prod=%d daily=%d", prodCalls, dailyCalls)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.RemainingFraction == nil || *bucket.RemainingFraction != .02 || bucket.ResetTime != "2026-10-24T00:19:00Z" {
		t.Fatalf("wrong endpoint's quota returned: %+v", bucket)
	}
	if BaseURLs[0] != prod.URL {
		t.Fatal("selecting a quota endpoint must not reorder global endpoints")
	}
}

func TestRetrieveUserQuotaSummaryFromURLDoesNotSwitchQuotaPools(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			otherCalls := 0
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				otherCalls++
				_, _ = w.Write([]byte(`{"buckets":[{"bucketId":"other-pool","remainingFraction":1}]}`))
			}))
			defer other.Close()
			selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusTemporaryRedirect {
					http.Redirect(w, r, other.URL+"/v1internal:retrieveUserQuotaSummary", status)
					return
				}
				w.WriteHeader(status)
			}))
			defer selected.Close()
			withMockBaseURLs(t, []string{other.URL, selected.URL})
			_, _, err := mustNewClient(t, "").RetrieveUserQuotaSummaryFromURL(context.Background(), selected.URL, "token", "project", 4096)
			var httpError *QuotaSummaryHTTPError
			if !errors.As(err, &httpError) || httpError.StatusCode != status || otherCalls != 0 {
				t.Fatalf("fixed endpoint must return its own failure without another pool: err=%v other=%d", err, otherCalls)
			}
		})
	}
}

func TestRetrieveUserQuotaSummaryPreservesForwardingAvailability(t *testing.T) {
	for _, fixedEndpoint := range []bool{false, true} {
		name := "default wrapper"
		if fixedEndpoint {
			name = "fixed endpoint"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"buckets":[{"bucketId":"weekly","remainingFraction":0.5}]}`))
			}))
			defer server.Close()
			other := "https://other.example"
			withMockBaseURLs(t, []string{server.URL, other})
			previousAvailability := DefaultURLAvailability
			availability := NewURLAvailability(time.Minute)
			availability.MarkSuccess(other)
			availability.MarkUnavailable(server.URL)
			DefaultURLAvailability = availability
			t.Cleanup(func() { DefaultURLAvailability = previousAvailability })

			client := mustNewClient(t, "")
			var err error
			if fixedEndpoint {
				_, _, err = client.RetrieveUserQuotaSummaryFromURL(context.Background(), server.URL, "token", "project", 4096)
			} else {
				_, _, err = client.RetrieveUserQuotaSummary(context.Background(), "token", "project", 4096)
			}
			if err != nil {
				t.Fatal(err)
			}
			urls := availability.GetAvailableURLs()
			if availability.IsAvailable(server.URL) || len(urls) != 1 || urls[0] != other {
				t.Fatalf("quota lookup changed forwarding availability: %v", urls)
			}
			availability.mu.RLock()
			lastSuccess := availability.lastSuccess
			availability.mu.RUnlock()
			if lastSuccess != other {
				t.Fatalf("quota lookup changed forwarding priority to %q", lastSuccess)
			}
		})
	}
}
