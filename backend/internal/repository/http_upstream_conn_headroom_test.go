package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 回归：账户隔离模式下 MaxConnsPerHost 不再恰好等于账户并发数。
// 上游实际只走 HTTP/1.1，账户并发=1 时一条长流占住唯一连接，
// 不占账号槽的旁路请求（count_tokens、Gemini Web 配额读取）以前要等长流结束；
// 现在有 accountConnHeadroom 条余量可用，但总连接数仍有硬上限。
func TestAccountIsolationConnHeadroomUnblocksSideRequests(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(doRelease)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowPrivateHosts: true}},
		Gateway:  config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount},
	}
	up := NewHTTPUpstream(cfg)

	const accountID, concurrency = 7, 1

	openSlow := func() *http.Response {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/slow", nil)
		require.NoError(t, err)
		resp, err := up.Do(req, "", accountID, concurrency)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp
	}
	doFast := func(timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/fast", nil)
		require.NoError(t, err)
		resp, err := up.Do(req, "", accountID, concurrency)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}

	var slows []*http.Response
	t.Cleanup(func() {
		doRelease()
		for _, r := range slows {
			_ = r.Body.Close()
		}
	})

	// 一条长流占满账户并发，旁路短请求应立即拿到额外连接。
	slows = append(slows, openSlow())
	start := time.Now()
	require.NoError(t, doFast(2*time.Second), "side request must not wait for the long stream")
	require.Less(t, time.Since(start), time.Second)

	// 硬上限仍在：并发+余量条长流全部在途时，新请求必须等待连接。
	for i := 0; i < accountConnHeadroom; i++ {
		slows = append(slows, openSlow())
	}
	require.Error(t, doFast(300*time.Millisecond), "MaxConnsPerHost must still cap connections at concurrency+headroom")
}
