//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func probeOriginTestCipher() SecretEncryptor {
	return &liveAttestationAES{key: sha256.Sum256([]byte("monitor-probe-test-key"))}
}

func signedProbeOriginTestRequest(t *testing.T, cipher SecretEncryptor) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions?version=1", strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Authorization", "Bearer test-api-key")
	req.Header.Set("X-Api-Key", "anthropic-test-key")
	req.Header.Set("X-Goog-Api-Key", "google-test-key")
	require.NoError(t, SignChannelMonitorProbeRequest(WithChannelMonitorProbeSigner(context.Background(), cipher), req))
	return req
}

func rewriteProbeOriginTestToken(t *testing.T, cipher SecretEncryptor, req *http.Request, mutate func(*monitorProbeOriginToken)) {
	t.Helper()
	plain, err := cipher.Decrypt(req.Header.Get(ChannelMonitorProbeOriginHeader))
	require.NoError(t, err)
	var token monitorProbeOriginToken
	require.NoError(t, json.Unmarshal([]byte(plain), &token))
	mutate(&token)
	plainBytes, err := json.Marshal(token)
	require.NoError(t, err)
	sealed, err := cipher.Encrypt(string(plainBytes))
	require.NoError(t, err)
	req.Header.Set(ChannelMonitorProbeOriginHeader, sealed)
}

func TestChannelMonitorProbeOriginValidatesAndStripsMarker(t *testing.T) {
	cipher := probeOriginTestCipher()
	cases := []struct {
		name   string
		mutate func(*testing.T, *http.Request)
		probe  bool
	}{
		{name: "valid", probe: true},
		{name: "missing", mutate: func(_ *testing.T, r *http.Request) { r.Header.Del(ChannelMonitorProbeOriginHeader) }},
		{name: "forged", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set(ChannelMonitorProbeOriginHeader, "probe") }},
		{name: "tampered", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(ChannelMonitorProbeOriginHeader, r.Header.Get(ChannelMonitorProbeOriginHeader)+"a")
		}},
		{name: "expired", mutate: func(t *testing.T, r *http.Request) {
			rewriteProbeOriginTestToken(t, cipher, r, func(v *monitorProbeOriginToken) { v.ExpiresAt = time.Now().Add(-time.Minute).Unix() })
		}},
		{name: "future", mutate: func(t *testing.T, r *http.Request) {
			rewriteProbeOriginTestToken(t, cipher, r, func(v *monitorProbeOriginToken) { v.ExpiresAt = time.Now().Add(time.Hour).Unix() })
		}},
		{name: "different-domain", mutate: func(t *testing.T, r *http.Request) {
			rewriteProbeOriginTestToken(t, cipher, r, func(v *monitorProbeOriginToken) { v.Kind = "live-attestation/v1" })
		}},
		{name: "method", mutate: func(_ *testing.T, r *http.Request) { r.Method = http.MethodPut }},
		{name: "host", mutate: func(_ *testing.T, r *http.Request) { r.Host = "other.example:8080" }},
		{name: "path", mutate: func(_ *testing.T, r *http.Request) { r.URL.Path = "/v1/responses" }},
		{name: "query", mutate: func(_ *testing.T, r *http.Request) { r.URL.RawQuery = "version=2" }},
		{name: "authorization", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("Authorization", "Bearer other-key") }},
		{name: "anthropic-key", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("X-Api-Key", "other-key") }},
		{name: "google-key", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("X-Goog-Api-Key", "other-key") }},
		{name: "body", mutate: func(_ *testing.T, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"model":"other-model"}`))
		}},
		{name: "oversized-body", mutate: func(_ *testing.T, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.Repeat("b", monitorProbeOriginMaxBody+20)))
		}},
		{name: "oversized-marker", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(ChannelMonitorProbeOriginHeader, strings.Repeat("a", 8193))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := signedProbeOriginTestRequest(t, cipher)
			if tc.mutate != nil {
				tc.mutate(t, req)
			}
			var gotSource string
			var authCalled, billingCalled bool
			router := gin.New()
			router.Use((&ChannelMonitorService{encryptor: cipher}).ProbeOriginMiddleware())
			router.Use(func(c *gin.Context) {
				authCalled = true
				require.Equal(t, req.Header.Get("Authorization"), c.GetHeader("Authorization"))
				c.Next()
			})
			router.Any("/*path", func(c *gin.Context) {
				gotSource = ChannelMonitorRequestSource(c.Request.Context())
				require.Empty(t, c.GetHeader(ChannelMonitorProbeOriginHeader))
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.NotEmpty(t, body)
				billingCalled = true
				c.Status(http.StatusOK)
			})
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			want := RequestSourceBusiness
			if tc.probe {
				want = RequestSourceProbe
			}
			require.Equal(t, want, gotSource)
			require.True(t, authCalled)
			require.True(t, billingCalled)
			require.Equal(t, http.StatusOK, res.Code)
		})
	}
}

func TestChannelMonitorProbeOriginLoopbackRoundTrip(t *testing.T) {
	cipher := probeOriginTestCipher()
	router := gin.New()
	router.Use((&ChannelMonitorService{encryptor: cipher}).ProbeOriginMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"source":        ChannelMonitorRequestSource(c.Request.Context()),
			"authorization": c.GetHeader("Authorization"),
			"body":          string(body),
			"marker":        c.GetHeader(ChannelMonitorProbeOriginHeader),
		})
	})
	server := httptest.NewServer(router)
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions?version=1", strings.NewReader(`{"model":"test-model"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer billed-key")
	require.NoError(t, SignChannelMonitorProbeRequest(WithChannelMonitorProbeSigner(context.Background(), cipher), req))
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	var got map[string]string
	require.NoError(t, json.NewDecoder(response.Body).Decode(&got))
	require.Equal(t, RequestSourceProbe, got["source"])
	require.Equal(t, "Bearer billed-key", got["authorization"])
	require.Equal(t, `{"model":"test-model"}`, got["body"])
	require.Empty(t, got["marker"])
}

func TestChannelMonitorProbeOriginReplayAndDetachedContext(t *testing.T) {
	cipher := probeOriginTestCipher()
	req := signedProbeOriginTestRequest(t, cipher)
	marker := req.Header.Get(ChannelMonitorProbeOriginHeader)
	var sources []string
	router := gin.New()
	router.Use((&ChannelMonitorService{encryptor: cipher}).ProbeOriginMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		worker := CopyChannelMonitorRequestSource(c.Request.Context(), context.Background())
		sources = append(sources, ChannelMonitorRequestSource(worker))
	})
	for i := 0; i < 2; i++ {
		replay := httptest.NewRequest(http.MethodPost, req.URL.String(), strings.NewReader(`{"model":"test-model"}`))
		replay.Header = req.Header.Clone()
		replay.Header.Set(ChannelMonitorProbeOriginHeader, marker)
		router.ServeHTTP(httptest.NewRecorder(), replay)
	}
	require.Equal(t, []string{RequestSourceProbe, RequestSourceBusiness}, sources)
	require.Equal(t, RequestSourceBusiness, ChannelMonitorRequestSource(nil))
	require.Equal(t, RequestSourceBusiness, ChannelMonitorRequestSource(CopyChannelMonitorRequestSource(nil, nil)))
}

func TestChannelMonitorProbeOriginDoesNotBypassAuth(t *testing.T) {
	cipher := probeOriginTestCipher()
	req := signedProbeOriginTestRequest(t, cipher)
	req.Header.Del("Authorization")
	require.NoError(t, SignChannelMonitorProbeRequest(WithChannelMonitorProbeSigner(context.Background(), cipher), req))
	router := gin.New()
	router.Use((&ChannelMonitorService{encryptor: cipher}).ProbeOriginMiddleware())
	router.Use(func(c *gin.Context) {
		require.Equal(t, RequestSourceProbe, ChannelMonitorRequestSource(c.Request.Context()))
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
		}
	})
	router.POST("/v1/chat/completions", func(c *gin.Context) { t.Fatal("unauthenticated probe reached handler") })
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, http.StatusUnauthorized, res.Code)
}

type probeOriginErrorEncryptor struct{}

func (probeOriginErrorEncryptor) Encrypt(string) (string, error) {
	return "", errors.New("secret-key-must-not-leak")
}
func (probeOriginErrorEncryptor) Decrypt(string) (string, error) {
	return "", errors.New("secret-key-must-not-leak")
}

type probeOriginCloseReader struct {
	io.Reader
	closed bool
}

func (r *probeOriginCloseReader) Close() error { r.closed = true; return nil }

func TestChannelMonitorProbeSignerPreservesBodyAndRedactsErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cipher    SecretEncryptor
		size      int
		wantError bool
	}{
		{name: "valid", cipher: probeOriginTestCipher(), size: 32},
		{name: "nil-signer", size: 32, wantError: true},
		{name: "failed-encryption", cipher: probeOriginErrorEncryptor{}, size: 32, wantError: true},
		{name: "oversized-body", cipher: probeOriginTestCipher(), size: monitorProbeOriginMaxBody + 20, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("a", tc.size)
			req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
			original := &probeOriginCloseReader{Reader: strings.NewReader(body)}
			req.Body = original
			req.Header.Set(ChannelMonitorProbeOriginHeader, "caller-forged-marker")
			err := SignChannelMonitorProbeRequest(WithChannelMonitorProbeSigner(nil, tc.cipher), req)
			if tc.wantError {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret-key")
				require.Empty(t, req.Header.Get(ChannelMonitorProbeOriginHeader))
			} else {
				require.NoError(t, err)
			}
			got, readErr := io.ReadAll(req.Body)
			require.NoError(t, readErr)
			require.Equal(t, body, string(got))
			require.NoError(t, req.Body.Close())
			require.True(t, original.closed)
		})
	}
	unsigned := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	unsigned.Header.Set(ChannelMonitorProbeOriginHeader, "caller-forged-marker")
	require.NoError(t, SignChannelMonitorProbeRequest(context.Background(), unsigned))
	require.Empty(t, unsigned.Header.Get(ChannelMonitorProbeOriginHeader))
}
