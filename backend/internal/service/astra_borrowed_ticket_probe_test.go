package service

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAstraBorrowedTicketProbeKeepsOriginalTicket(t *testing.T) {
	for _, returned := range []string{"", "donor-ticket", "replacement-ticket"} {
		t.Run("returned="+returned, func(t *testing.T) {
			upstream := &stateProbeUpstream{replies: []stateProbeReply{
				{status: 200, state: returned, body: stateProbeCompletedStream},
				{status: 200, state: returned, body: stateProbeCompletedStream},
			}}
			req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			req.Header.Set("Authorization", "Bearer target-token")
			req.Header.Set("ChatGPT-Account-ID", "target-account")
			req.Header.Set("X-Codex-Turn-State", "donor-ticket")
			req.Header.Set("Cookie", "__oailb=donor-route; __cflb=donor-cf")
			r := ProbeAstraBorrowedTicket(t.Context(), upstream, req, "same-exit", 300, 1, nil)
			require.Equal(t, OpenAICodexStateHealthy, r.Verdict)
			require.Equal(t, returned == "replacement-ticket", r.NewTicket)
			require.Len(t, upstream.calls, 2)
			for _, call := range upstream.calls {
				require.Equal(t, "donor-ticket", call.header.Get("X-Codex-Turn-State"))
				require.Equal(t, req.Header.Get("Cookie"), call.header.Get("Cookie"))
				require.Equal(t, "Bearer target-token", call.header.Get("Authorization"))
				require.Equal(t, "target-account", call.header.Get("ChatGPT-Account-ID"))
				require.Equal(t, "same-exit", call.proxy)
			}
		})
	}
}

func TestAstraBorrowedTicketProbeStillRejectsUnusableResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   stateProbeReply
		failure string
	}{
		{"auth", stateProbeReply{status: 401}, OpenAICodexStateFailureAccountError},
		{"rate limit", stateProbeReply{status: 429}, OpenAICodexStateFailureRateLimited},
		{"stream failure", stateProbeReply{status: 200, body: stateProbeFailedStream}, OpenAICodexStateFailureStreamError},
		{"truncated", stateProbeReply{status: 200, body: `data: {"type":"response.created"}`}, OpenAICodexStateFailureStreamError},
		{"route changed", stateProbeReply{status: 200, cookies: []string{"__oailb=another-route; Path=/"}, body: stateProbeCompletedStream}, "route_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, state: "rotated", body: stateProbeCompletedStream}, tc.reply}}
			req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			req.Header.Set("X-Codex-Turn-State", "donor-ticket")
			req.Header.Set("Cookie", "__oailb=donor-route")
			r := ProbeAstraBorrowedTicket(t.Context(), upstream, req, "", 300, 1, nil)
			require.Equal(t, OpenAICodexStateInconclusive, r.Verdict)
			require.Equal(t, tc.failure, r.Failure)
			require.Len(t, upstream.calls, 2)
		})
	}
}

func TestAstraBorrowedTicketExpiryNeverExceedsFourMinutes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, age := range []time.Duration{time.Minute, 5 * time.Minute, -time.Minute} {
		raw := make([]byte, 585)
		raw[0] = 0x80
		binary.BigEndian.PutUint64(raw[1:9], uint64(now.Add(-age).Unix()))
		ticket := base64.URLEncoding.EncodeToString(raw)
		want := now.Add(240*time.Second - age)
		if age < 0 {
			want = now.Add(240 * time.Second)
		}
		require.Equal(t, want, AstraBorrowedTicketExpiry(ticket, now))
	}
	require.Equal(t, now.Add(240*time.Second), AstraBorrowedTicketExpiry("opaque-ticket", now))
}

func TestAstraBorrowingDoesNotRequireUnrelatedOwnTicket(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true, Models: []string{"gpt-6-astra", "gpt-6-sol"}}, nil)
	svc.cfg.Gateway.CodexGatewayPin = config.CodexGatewayPinConfig{Enabled: true, SourceAccountIDs: []int64{299}, TargetAccountIDs: []int64{300}}
	account := ticketTestAccount(300)
	h := http.Header{}
	h.Set("X-Codex-Turn-State", "unrelated-own-ticket")
	require.NoError(t, svc.applyOpenAICodexTicket(t.Context(), account, "gpt-6-astra", h))
	require.Empty(t, h.Get("X-Codex-Turn-State"), "donor state is assembled by borrowing transport")
	require.False(t, svc.openAICodexTicketBlocksAccount(account, "gpt-6-astra"))
	require.Error(t, svc.applyOpenAICodexTicket(t.Context(), account, "gpt-6-sol", h))
	require.Error(t, svc.applyOpenAICodexTicket(t.Context(), ticketTestAccount(301), "gpt-6-astra", h))
	svc.cfg.Gateway.CodexGatewayPin.Enabled = false
	require.Error(t, svc.applyOpenAICodexTicket(t.Context(), account, "gpt-6-astra", h))
}

func TestAstraSchedulingAcceptsSuccessfulTicketRotation(t *testing.T) {
	now := time.Now()
	expires := now.Add(time.Minute)
	settings := config.AstraRoutingSettings{Revision: "same", CookiePool: config.CodexGatewayPinConfig{Enabled: true}}
	snapshot := AstraGatewayRuntime{Revision: "same", Targets: []AstraRouteStatus{{AccountID: 300, State: "ready", Reason: "target_probe_ticket_rotated", ExpiresAt: &expires}}}
	require.True(t, astraAccountSchedulingReady(settings, snapshot, 300, now))
	require.False(t, astraAccountSchedulingReady(settings, snapshot, 300, expires))
}

func TestAstraBorrowedWSBindingKeepsAccountAndConfigurationChecks(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true, Models: []string{"gpt-6-astra"}}, nil)
	svc.cfg.Gateway.CodexGatewayPin = config.CodexGatewayPinConfig{Enabled: true, SourceAccountIDs: []int64{299}, TargetAccountIDs: []int64{300}}
	account := ticketTestAccount(300)
	headers := http.Header{}
	headers.Set("X-Codex-Turn-State", "donor-state")
	binding := svc.bindOpenAIWSHandshake(account, "gpt-6-astra", headers)
	require.Error(t, svc.checkOpenAIWSBinding(account, "gpt-6-astra", binding), "an ordinary connection cannot claim donor validation")
	settings := svc.cfg.AstraRouting(t.Context())
	binding.borrowedSettings = &settings
	require.NoError(t, svc.checkOpenAIWSBinding(account, "gpt-6-astra", binding), "donor handshake needs no target-owned inventory ticket")
	require.Error(t, svc.checkOpenAIWSBinding(account, "gpt-6-sol", binding))
	other := *account
	other.Credentials = map[string]any{"access_token": "changed", "chatgpt_account_id": "other-account"}
	require.Error(t, svc.checkOpenAIWSBinding(&other, "gpt-6-astra", binding))
	svc.cfg.Gateway.CodexGatewayPin.TTLSeconds = 60
	require.Error(t, svc.checkOpenAIWSBinding(account, "gpt-6-astra", binding), "changed borrowing settings invalidate old connections")
	svc.cfg.Gateway.CodexGatewayPin = settings.CookiePool
	require.NoError(t, svc.checkOpenAIWSBinding(account, "gpt-6-astra", binding))
	binding.createdAt = time.Now().Add(-openAIWSConnMaxAge)
	require.Error(t, svc.checkOpenAIWSBinding(account, "gpt-6-astra", binding))
}
