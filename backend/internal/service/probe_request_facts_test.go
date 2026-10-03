//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type probeFactsRecorderStub struct {
	rows     []ProbeRequestFact
	canceled bool
}

func (r *probeFactsRecorderStub) RecordProbeRequestFacts(ctx context.Context, facts []ProbeRequestFact) error {
	r.canceled = ctx.Err() != nil
	r.rows = append(r.rows, facts...)
	return nil
}

func observedProbeResponse(t *testing.T, collector *pelicanTestUsageCollector, status int, contentType, body string) *http.Response {
	t.Helper()
	ctx := context.WithValue(context.Background(), pelicanTestUsageKey{}, collector)
	req := httptest.NewRequest(http.MethodPost, "http://upstream.test/v1/responses", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	resp, err := observeProbeHTTPRequest(req, func() (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	require.NoError(t, err)
	return resp
}

func TestProbeRequestFactsHTTPFailuresAndIncompleteUsage(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformOpenAI}
	for _, tc := range []struct {
		name, kind string
		status     int
		sendErr    error
	}{
		{"http429", "http", 429, nil}, {"http503", "http", 503, nil},
		{"transport", "transport", 0, errors.New("connection refused")}, {"timeout", "timeout", 0, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := &pelicanTestUsageCollector{model: "test-model", role: "generation"}
			ctx := context.WithValue(context.Background(), pelicanTestUsageKey{}, collector)
			req := httptest.NewRequest(http.MethodPost, "http://upstream.test/v1/responses", nil).WithContext(ctx)
			resp, err := observeProbeHTTPRequest(req, func() (*http.Response, error) {
				if tc.sendErr != nil {
					return nil, tc.sendErr
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`))}, nil
			})
			if tc.sendErr != nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			facts := collector.facts(nil, account)
			require.Len(t, facts, 1)
			require.False(t, facts[0].APISuccess)
			require.Equal(t, tc.kind, *facts[0].ErrorKind)
			require.Nil(t, facts[0].InputTokens)
			require.Nil(t, facts[0].OutputTokens)
			require.Nil(t, facts[0].FirstTokenMs)
			require.False(t, facts[0].UsageComplete)
			require.Nil(t, facts[0].UpstreamCostUSD)
			require.False(t, facts[0].CostComplete)
		})
	}
}

func TestProbeRequestFactsSuccessQualityAndTrueTTFT(t *testing.T) {
	collector := &pelicanTestUsageCollector{model: "model", role: "generation"}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"wrong answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	resp := observedProbeResponse(t, collector, 200, "text/event-stream", stream)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(context.WithValue(context.Background(), pelicanTestUsageKey{}, collector))
	require.NoError(t, (&AccountTestService{}).processOpenAIStream(c, resp.Body))
	_ = resp.Body.Close()
	// The stream parser and transport observation refer to one physical request.
	require.Len(t, collector.requests, 1)
	facts := collector.facts(nil, &Account{ID: 1, Platform: PlatformOpenAI})
	require.True(t, facts[0].APISuccess) // It does not matter whether the answer is correct.
	require.NotNil(t, facts[0].FirstTokenMs)
	require.True(t, facts[0].UsageComplete)
	require.Equal(t, int64(10), *facts[0].InputTokens)
	nonstream := &pelicanTestUsageCollector{model: "model"}
	resp = observedProbeResponse(t, nonstream, 200, "application/json", `{"status":"completed","usage":{"input_tokens":4,"output_tokens":1}}`)
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	nonstreamFacts := nonstream.facts(nil, &Account{ID: 1, Platform: PlatformOpenAI})
	require.True(t, nonstreamFacts[0].APISuccess)
	require.Nil(t, nonstreamFacts[0].FirstTokenMs)
}

func TestProbeRequestFactsMissingTerminalFailsAndMetadataIsNotTTFT(t *testing.T) {
	collector := &pelicanTestUsageCollector{model: "model"}
	resp := observedProbeResponse(t, collector, 200, "text/event-stream", "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":10}}}\n\n")
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	facts := collector.facts(nil, &Account{ID: 1, Platform: PlatformOpenAI})
	require.False(t, facts[0].APISuccess)
	require.Equal(t, "stream", *facts[0].ErrorKind)
	require.Nil(t, facts[0].FirstTokenMs)
	require.False(t, facts[0].UsageComplete)
}

func TestProbeRequestFactsExcludeHEADAndUnscopedRequests(t *testing.T) {
	collector := &pelicanTestUsageCollector{model: "model"}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req := httptest.NewRequest(method, "http://upstream.test/v1/responses", nil).WithContext(context.WithValue(context.Background(), pelicanTestUsageKey{}, collector))
		_, _ = observeProbeHTTPRequest(req, func() (*http.Response, error) { return nil, nil })
	}
	req := httptest.NewRequest(http.MethodPost, "http://upstream.test/v1/responses", nil)
	_, _ = observeProbeHTTPRequest(req, func() (*http.Response, error) { return nil, nil })
	require.Empty(t, collector.requests)
}

func TestProbeRequestFactsGroupRetriesAndQualityKeepOneFinalOutcome(t *testing.T) {
	plan := groupTestPlan(1)
	plan.PelicanConfig.QuestionKind = "candy"
	router := &groupTestRouterFake{steps: []routeStep{{account: &Account{ID: 1, Name: "first", Platform: PlatformOpenAI}}, {account: &Account{ID: 2, Name: "second", Platform: PlatformOpenAI}}}}
	recorder := &probeFactsRecorderStub{}
	svc, _ := newGroupTestService(newGroupTestRepoFake(plan), router, func(ctx context.Context, id int64, _ string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
		collector := pelicanUsageFromContext(ctx)
		if id == 1 {
			req := httptest.NewRequest(http.MethodPost, "http://upstream.test/v1/responses", nil).WithContext(ctx)
			_, err := observeProbeHTTPRequest(req, func() (*http.Response, error) { return nil, context.DeadlineExceeded })
			return nil, err
		}
		resp := observedProbeResponse(t, collector, 200, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"incorrect candy answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n")
		_, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		// The quality result failed but the actual API generation succeeded.
		return &ScheduledTestResult{Status: "failed", ResponseText: "incorrect candy answer", ErrorMessage: "Wrong answer"}, nil
	})
	svc.probeRequestFactRecorder = recorder
	result := svc.runSample(context.Background(), plan, &Group{ID: plan.GroupID, Platform: PlatformOpenAI})
	require.Equal(t, "failed", result.Status)
	require.Len(t, recorder.rows, 2)
	first, last := recorder.rows[0], recorder.rows[1]
	require.False(t, first.IsFinal)
	require.True(t, last.IsFinal)
	require.False(t, first.APISuccess)
	require.True(t, last.APISuccess)
	require.Equal(t, first.LogicalRequestID, last.LogicalRequestID)
	require.Equal(t, []int{1, 2}, []int{first.AttemptNumber, last.AttemptNumber})
	require.Equal(t, plan.GroupID, *last.GroupID)
	require.Equal(t, int64(2), last.AccountID)
	require.Equal(t, "generation", last.Role)
	require.False(t, recorder.canceled)
	require.True(t, last.FinishedAt.After(time.Time{}))
}

func TestProbeRequestFactsBufferedMeteringAndNestedTransportRecordedOnce(t *testing.T) {
	collector := &pelicanTestUsageCollector{model: "model"}
	ctx := context.WithValue(context.Background(), pelicanTestUsageKey{}, collector)
	req := httptest.NewRequest(http.MethodPost, "http://upstream.test/v1/responses", nil).WithContext(ctx)
	wire := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	resp, err := observeProbeHTTPRequest(req, func() (*http.Response, error) {
		// Gateway and account transport decorate the same real HTTP request.
		return observeProbeHTTPRequest(req, func() (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
		})
	})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	recordPelicanTestSSE(ctx, "openai", "model", body)
	require.Len(t, collector.requests, 1)
	snapshot := collector.snapshot()
	require.Equal(t, int64(1), snapshot.RequestCount)
	require.Equal(t, int64(10), snapshot.InputTokens)
	require.Equal(t, int64(2), snapshot.OutputTokens)
	require.Len(t, collector.facts(nil, &Account{ID: 1, Platform: PlatformOpenAI}), 1)
}

func TestProbeRequestFactsTokenQueriesAreNotGeneration(t *testing.T) {
	collector := &pelicanTestUsageCollector{model: "model"}
	for _, path := range []string{"/v1/responses/input_tokens", "/v1/messages/count_tokens"} {
		req := httptest.NewRequest(http.MethodPost, "http://upstream.test"+path, nil).WithContext(context.WithValue(context.Background(), pelicanTestUsageKey{}, collector))
		_, _ = observeProbeHTTPRequest(req, func() (*http.Response, error) { return nil, nil })
	}
	require.Empty(t, collector.requests)
}

func TestProbeRequestFactsWebsocketSuccessAndFailure(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI}
	for _, tc := range []struct {
		name    string
		sendErr error
		events  []string
	}{
		{name: "success", events: []string{`{"type":"response.output_text.delta","delta":"answer"}`, `{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":1}}}`}},
		{name: "write failed", sendErr: errors.New("connection closed")},
		{name: "read timeout", sendErr: context.DeadlineExceeded},
		{name: "stream failed", events: []string{`{"type":"response.failed","response":{"error":{"message":"overloaded"}}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := &pelicanTestUsageCollector{model: "model", role: "generation"}
			ctx := context.WithValue(context.Background(), pelicanTestUsageKey{}, collector)
			u := beginProbeStreamAttempt(ctx, "model")
			for _, event := range tc.events {
				u.read(event)
			}
			finishProbeStreamAttempt(u, tc.sendErr, "transport")
			facts := collector.facts(nil, account)
			require.Len(t, facts, 1)
			require.Equal(t, tc.name == "success", facts[0].APISuccess)
			if tc.name == "success" {
				require.NotNil(t, facts[0].FirstTokenMs)
				require.True(t, facts[0].UsageComplete)
			} else {
				require.NotNil(t, facts[0].ErrorKind)
			}
			require.Equal(t, "openai_ws", facts[0].Protocol)
		})
	}
}

func TestProbeRequestFactsCompletedNullErrorIsSuccess(t *testing.T) {
	collector := &pelicanTestUsageCollector{model: "model"}
	resp := observedProbeResponse(t, collector, 200, "application/json", `{"status":"completed","error":null,"usage":{"input_tokens":4,"output_tokens":1}}`)
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	facts := collector.facts(nil, &Account{ID: 1, Platform: PlatformOpenAI})
	require.True(t, facts[0].APISuccess)
	require.Nil(t, facts[0].ErrorKind)
}

func TestProbeRequestFactsRealWSv2ForwardRecordsActualAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	collector := &pelicanTestUsageCollector{model: "gpt-5.5", requestedModel: "gpt-5.5", role: "generation"}
	ctx := context.WithValue(context.Background(), pelicanTestUsageKey{}, collector)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.output_text.delta","delta":"API answer"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_probe_fact","model":"gpt-5.5","status":"completed","error":null,"output":[{"type":"message","content":[{"type":"output_text","text":"API answer"}]}],"usage":{"input_tokens":9,"output_tokens":3}}}`),
	}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	account := &Account{ID: 9876, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "test"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Len(t, conn.writes, 1)
	facts := collector.facts(nil, account)
	require.Len(t, facts, 1)
	require.True(t, facts[0].APISuccess)
	require.Equal(t, "openai_ws", facts[0].Protocol)
	require.NotNil(t, facts[0].FirstTokenMs)
	require.Equal(t, int64(9), *facts[0].InputTokens)
	require.Equal(t, int64(3), *facts[0].OutputTokens)
}

func TestProbeRequestFactsManualAccountTestHasNoProjectedGroup(t *testing.T) {
	account := &Account{ID: 9877, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": "http://upstream.example"}}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	resp := newJSONResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n")
	resp.Header.Set("Content-Type", "text/event-stream")
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	recorder := &probeFactsRecorderStub{}
	svc := NewAccountTestService(repo, nil, nil, nil, nil, upstream, rawChatCompletionsTestConfig(), nil)
	svc.SetProbeRequestFactRecorder(recorder)
	c, _ := newTestContext()
	require.NoError(t, svc.TestAccountConnection(c, account.ID, "gpt-5.5", "hello", AccountTestModeDefault))
	require.Len(t, recorder.rows, 1)
	require.Equal(t, "account_check", recorder.rows[0].Role)
	require.Nil(t, recorder.rows[0].GroupID)
	require.Equal(t, account.ID, recorder.rows[0].AccountID)
	require.True(t, recorder.rows[0].APISuccess)
	require.True(t, recorder.rows[0].IsFinal)
}
