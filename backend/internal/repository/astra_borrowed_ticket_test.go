package repository

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAstraBorrowedTicketProbeAndBusinessRequestUseSameSource(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	cfg.Gateway.CodexGatewayPin.IPAffinity = true
	wrapper := &astraRoutingUpstream{cfg: cfg}
	sourceTicket := "donor-ticket-one"
	targetCalls := 0
	wrapper.delegate = gatewayPinDelegate{call: func(req *http.Request, proxy string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if id == 299 {
			resp := pinResponse("__oailb=donor-route; Path=/; Secure; Max-Age=3600")
			resp.Header.Set("X-Codex-Turn-State", sourceTicket)
			resp.Header.Add("Set-Cookie", "__cflb=donor-cf; Path=/; Secure; Max-Age=3600")
			resp.Header.Add("Set-Cookie", "chatgpt_session=do-not-copy; Path=/; Secure")
			return resp, nil
		}
		targetCalls++
		require.Equal(t, "source-exit", proxy)
		require.Equal(t, "Bearer target-token", req.Header.Get("Authorization"))
		require.Equal(t, "target-account", req.Header.Get("ChatGPT-Account-ID"))
		require.Equal(t, sourceTicket, req.Header.Get("X-Codex-Turn-State"))
		require.Equal(t, "__oailb=donor-route; __cflb=donor-cf", req.Header.Get("Cookie"))
		resp := pinResponse("")
		resp.Header.Set("X-Codex-Turn-State", "replacement-must-not-enter-pool")
		return resp, nil
	}}
	capture := func() {
		resp, err := wrapper.Do(pinRequest(t), "source-exit", 299, 1)
		consumeAffinityResponse(t, resp, err)
	}
	target := pinRequest(t)
	target.Header.Set("Authorization", "Bearer target-token")
	target.Header.Set("ChatGPT-Account-ID", "target-account")
	target.Header.Set("X-Codex-Turn-State", "unrelated-target-ticket")
	capture()
	pool := wrapper.current(t.Context())
	expiry := pool.routes[299].expires
	resp, err := wrapper.Do(target, "target-exit", 300, 1)
	consumeAffinityResponse(t, resp, err)
	require.Equal(t, 3, targetCalls, "two same-ticket probes followed by real request")
	require.Equal(t, sourceTicket, pool.routes[299].ticket)
	require.Equal(t, expiry, pool.routes[299].expires)
	require.Equal(t, "unrelated-target-ticket", target.Header.Get("X-Codex-Turn-State"), "caller request remains unmodified")
	snapshot := wrapper.AstraGatewaySnapshot(t.Context())
	require.Equal(t, 1, snapshot.ReadyRoutes)
	require.Equal(t, "target_probe_ticket_rotated", snapshot.Targets[0].Reason)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, secret := range []string{sourceTicket, "donor-cf", "Bearer target-token", "replacement-must-not-enter-pool"} {
		require.NotContains(t, string(raw), secret)
	}
	resp, err = wrapper.Do(target, "target-exit", 300, 1)
	consumeAffinityResponse(t, resp, err)
	require.Equal(t, 4, targetCalls, "cache reuses the same ticket without another probe")
	require.Equal(t, expiry, pool.routes[299].expires)
	// New donor state with identical cookies invalidates the previous target pass.
	sourceTicket = "donor-ticket-two"
	capture()
	require.Zero(t, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
	resp, err = wrapper.Do(target, "target-exit", 300, 1)
	consumeAffinityResponse(t, resp, err)
	require.Equal(t, 7, targetCalls)
	// WS dialing restores this same bundle after ordinary header assembly.
	headers := target.Header.Clone()
	lease, err := wrapper.CodexGatewayPinWSRequest(t.Context(), headers, "target-exit", 300, 1)
	require.NoError(t, err)
	defer lease.Release()
	headers.Set("X-Codex-Turn-State", "ordinary-ticket-would-overwrite")
	require.NoError(t, lease.Apply(headers))
	require.Equal(t, sourceTicket, headers.Get("X-Codex-Turn-State"))
	require.Equal(t, "__oailb=donor-route; __cflb=donor-cf", headers.Get("Cookie"))
	cfg.Gateway.CodexGatewayPin.Enabled = false
	require.ErrorContains(t, lease.Apply(headers), "configuration_changed")
}

func TestAstraBorrowedTicketLifetimeCannotBeRenewed(t *testing.T) {
	now := time.Now()
	pool := &codexGatewayPinUpstream{config: pinConfig()}
	first := codexGatewayRouteFromResponse(pinResponse("__oailb=original; Path=/; Secure; Max-Age=3600"), "/backend-api/codex/responses", now)
	require.Equal(t, now.Add(240*time.Second), first.expires)
	pool.recordSource(299, now, first, true)
	replay := *first
	replay.expires = now.Add(300 * time.Second)
	replay.cookie.Value = "reissued-cookie"
	pool.recordSource(299, now.Add(time.Second), &replay, true)
	require.Equal(t, first.expires, pool.routes[299].expires)
	shorter := pool.routes[299]
	shorter.expires = now.Add(30 * time.Second)
	pool.recordSource(299, now.Add(1500*time.Millisecond), &shorter, true)
	require.Equal(t, shorter.expires, pool.routes[299].expires, "shorter Cookie expiry must take effect even with identical values")
	expired := pool.routes[299]
	expired.expires = now.Add(-time.Second)
	pool.routes[299] = expired
	cookie, _ := pool.currentCookie("/backend-api/codex/responses", now)
	require.Nil(t, cookie)
	pool.recordSource(299, now.Add(2*time.Second), &replay, true)
	cookie, _ = pool.currentCookie("/backend-api/codex/responses", now)
	require.Nil(t, cookie, "rediscovered expired ticket cannot restart TTL")
	require.ErrorContains(t, expired.apply(http.Header{}), "route_expired")
	fresh := replay
	fresh.ticket = "newly-issued-ticket"
	fresh.expires = now.Add(time.Minute)
	pool.recordSource(299, now.Add(3*time.Second), &fresh, true)
	cookie, _ = pool.currentCookie("/backend-api/codex/responses", now)
	require.NotNil(t, cookie)
}

func TestAstraBorrowingRequiresSourceTicketAndExpiresDuringProbe(t *testing.T) {
	missing := pinResponse("__oailb=route; Path=/; Secure; Max-Age=3600")
	missing.Header.Del("X-Codex-Turn-State")
	require.Nil(t, codexGatewayRouteFromResponse(missing, "/backend-api/codex/responses", time.Now()))
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	calls := 0
	wrapper.delegate = gatewayPinDelegate{call: func(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if id == 299 {
			return pinResponse("__oailb=route; Path=/; Secure; Max-Age=240"), nil
		}
		require.True(t, service.IsOpenAICodexStateProbeRequest(req.Context()), "expired bundle must never reach business request")
		calls++
		if calls == 2 {
			pool := wrapper.current(t.Context())
			pool.mu.Lock()
			r := pool.routes[299]
			r.expires = time.Now().Add(-time.Second)
			pool.routes[299] = r
			pool.mu.Unlock()
		}
		return pinResponse(""), nil
	}}
	resp, err := wrapper.Do(pinRequest(t), "", 299, 1)
	consumeAffinityResponse(t, resp, err)
	_, err = wrapper.Do(pinRequest(t), "", 300, 1)
	require.Error(t, err)
	require.Equal(t, 2, calls)
	require.Zero(t, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
}
