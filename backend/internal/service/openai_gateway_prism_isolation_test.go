//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func prismIsolationContext(id int64, session string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: id})
	if session != "" {
		c.Request.Header.Set("session_id", session)
	}
	return c
}

func TestPrismProjectScopeTrustBoundary(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	one, two := make(http.Header), make(http.Header)
	one["x-prism-key-scope"] = []string{"spoofed-tenant"}
	one.Set("X-Prism-Session-Scope", "spoofed-session")
	setPrismIsolationHeaders(prismIsolationContext(10, "private-session"), h.account, one)
	setPrismIsolationHeaders(prismIsolationContext(11, "private-session"), h.account, two)
	require.Len(t, one.Get("X-Prism-Key-Scope"), 64)
	require.Len(t, one.Get("X-Prism-Session-Scope"), 64)
	require.NotEqual(t, one.Get("X-Prism-Key-Scope"), two.Get("X-Prism-Key-Scope"))
	require.NotEqual(t, one.Get("X-Prism-Session-Scope"), two.Get("X-Prism-Session-Scope"))
	require.NotContains(t, one.Get("X-Prism-Session-Scope"), "private-session")
	require.NotContains(t, one, "x-prism-key-scope")
	stable := make(http.Header)
	setPrismIsolationHeaders(prismIsolationContext(10, "private-session"), h.account, stable)
	require.Equal(t, one, stable)
	for _, id := range []int64{0, -1} {
		header := make(http.Header)
		header.Set("X-Prism-Key-Scope", "spoof")
		header.Set("X-Prism-Session-Scope", "spoof")
		setPrismIsolationHeaders(prismIsolationContext(id, "private-session"), h.account, header)
		require.Empty(t, header)
	}
	setPrismIsolationHeaders(prismIsolationContext(10, "private-session"), &Account{Type: AccountTypeAPIKey}, one)
	require.Empty(t, one)
}

func TestPrismProjectScopeRequiresExplicitConversation(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	for _, body := range []string{`{"input":"same content"}`, `{"metadata":{"user_id":"not-a-session"}}`, `{"instructions":"shared prefix"}`} {
		header := make(http.Header)
		setPrismIsolationHeaders(prismIsolationContext(10, ""), h.account, header, []byte(body))
		require.NotEmpty(t, header.Get("X-Prism-Key-Scope"))
		require.Empty(t, header.Get("X-Prism-Session-Scope"))
	}
	for _, body := range []string{`{"prompt_cache_key":"thread"}`, `{"client_metadata":{"session_id":"thread"}}`,
		`{"metadata":{"user_id":"{\"session_id\":\"thread\"}"}}`} {
		header := make(http.Header)
		setPrismIsolationHeaders(prismIsolationContext(10, ""), h.account, header, []byte(body))
		require.Equal(t, prismScopeDigest("prism-session:v1:10:thread"), header.Get("X-Prism-Session-Scope"))
	}
	c := prismIsolationContext(10, "")
	rememberOpenCodeInboundBody(c, []byte(`{"metadata":{"user_id":"{\"session_id\":\"original-session\"}"}}`))
	header := make(http.Header)
	setPrismIsolationHeaders(c, h.account, header, []byte(`{"messages":[]}`))
	require.Equal(t, prismScopeDigest("prism-session:v1:10:original-session"), header.Get("X-Prism-Session-Scope"))
}

func TestPrismProjectScopeRejectsWrongTypedSessionFields(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	for _, body := range []string{`{"prompt_cache_key":false}`, `{"prompt_cache_key":123}`, `{"prompt_cache_key":[]}`,
		`{"client_metadata":{"session_id":{},"thread_id":false}}`, `{"metadata":{"user_id":{"session_id":"thread"}}}`,
		`{"metadata":{"user_id":"{\"session_id\":false}"}}`} {
		header := make(http.Header)
		setPrismIsolationHeaders(prismIsolationContext(10, ""), h.account, header, []byte(body))
		require.Empty(t, header.Get("X-Prism-Session-Scope"), body)
	}
}

func TestPrismProjectScopeDoesNotAdoptDerivedOutboundCacheKey(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	c := prismIsolationContext(10, "")
	rememberOpenCodeInboundBody(c, []byte(`{"messages":[{"role":"user","content":"shared prefix"}]}`))
	header := make(http.Header)
	setPrismIsolationHeaders(c, h.account, header, []byte(`{"prompt_cache_key":"gateway-derived-prefix"}`))
	require.NotEmpty(t, header.Get("X-Prism-Key-Scope"))
	require.Empty(t, header.Get("X-Prism-Session-Scope"))
}

func TestPrismProjectScopeInNormalAndPassthroughBuilders(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		h := newPrismGatewayHarness(t, nil, nil)
		c := prismIsolationContext(10, "thread")
		c.Request.Header.Set("X-Prism-Key-Scope", "spoof")
		var request *http.Request
		var err error
		if passthrough {
			request, err = h.svc.buildUpstreamRequestOpenAIPassthrough(c.Request.Context(), c, h.account, []byte(`{"model":"gpt-5.6-sol"}`), "token")
		} else {
			request, err = h.svc.buildUpstreamRequest(c.Request.Context(), c, h.account, []byte(`{"model":"gpt-5.6-sol"}`), "token", false, "", false)
		}
		require.NoError(t, err)
		require.Equal(t, prismScopeDigest("prism-key:v1:10"), request.Header.Get("X-Prism-Key-Scope"))
		require.Equal(t, prismScopeDigest("prism-session:v1:10:thread"), request.Header.Get("X-Prism-Session-Scope"))
	}
}
