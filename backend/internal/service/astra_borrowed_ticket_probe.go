package service

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

func (s *OpenAIGatewayService) usesAstraBorrowedTicket(ctx context.Context, account *Account, model string) bool {
	if s == nil || s.cfg == nil || account == nil || !account.IsOpenAIOAuthLike() || model != OpenAICodexStateProbeDefaultModel || account.IsExcelBPSEnabledForModel(model) {
		return false
	}
	pool := s.cfg.AstraRouting(ctx).CookiePool
	return pool.Enabled && slices.Contains(pool.TargetAccountIDs, account.ID)
}

// An embedded issue time, when available, may shorten but never extend the
// measured four-minute lifetime from acquisition.
func AstraBorrowedTicketExpiry(ticket string, acquired time.Time) time.Time {
	expires := acquired.Add(240 * time.Second)
	if shape, err := parseOpenAICodexTicketShape(ticket); err == nil && !shape.IssuedAt.IsZero() {
		if issuedExpiry := shape.IssuedAt.Add(240 * time.Second); issuedExpiry.Before(expires) {
			expires = issuedExpiry
		}
	}
	return expires
}

// ProbeAstraBorrowedTicket tests connectivity with the same donor ticket and
// routing cookies on both shots. Replacement tickets are observations only:
// they neither fail validation nor replace or renew the borrowed ticket.
func ProbeAstraBorrowedTicket(ctx context.Context, upstream HTTPUpstream, template *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) *OpenAICodexStateProbeResult {
	r := &OpenAICodexStateProbeResult{AccountID: accountID, Model: OpenAICodexStateProbeDefaultModel, Verdict: OpenAICodexStateInconclusive, StartedAt: time.Now()}
	defer func() {
		r.FinishedAt = time.Now()
		r.LatencyMs = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	}()
	if upstream == nil || template == nil {
		r.fail(OpenAICodexStateFailureUnsupported, "缺少借票验证请求", "")
		return r
	}
	ticket := strings.TrimSpace(template.Header.Get(openAICodexTurnStateHeader))
	pinned, err := template.Cookie("__oailb")
	if err != nil || pinned.Value == "" || ticket == "" {
		r.fail(OpenAICodexStateFailureNoTicket, "缺少来源门票或路由 Cookie", "")
		return r
	}
	r.TicketLength = len(ticket)
	for i, step := range []string{"借票首轮", "同票复用"} {
		routeChanged := false
		shot, err := fireOpenAICodexStateShotRequest(ctx, template.Header, r.Model, ticket, template.Header.Get("Cookie"), func(req *http.Request) (*http.Response, error) {
			req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
			resp, err := upstream.DoWithTLS(req, proxy, accountID, concurrency, profile)
			if resp != nil {
				for _, got := range resp.Cookies() {
					if got.Name == "__oailb" && (got.Value != pinned.Value || got.MaxAge < 0 || (!got.Expires.IsZero() && !time.Now().Before(got.Expires))) {
						routeChanged = true
					}
				}
			}
			if routeChanged {
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, errors.New("target_route_changed")
			}
			return resp, err
		})
		if i == 0 {
			r.MintStatus = shot.status
		} else {
			r.ContinueStatus = shot.status
			r.ContinueTicketLength = len(shot.state)
		}
		r.NewTicket = r.NewTicket || (shot.state != "" && shot.state != ticket)
		if routeChanged {
			r.fail("route_changed", "验证期间网关路由已改变", "")
			return r
		}
		if !r.shotUsable(ctx, step, shot, err) {
			return r
		}
		r.ReportedModel = shot.model
	}
	r.Verdict = OpenAICodexStateHealthy
	r.Reason = "同一来源门票的两轮请求均完整成功；换票不影响连通性判定，不代表模型能力。"
	return r
}

// AstraGatewayWSRoute carries a process-local lease, never an admin response.
// Apply is repeated immediately before dialing, after ordinary ticket assembly.
type AstraGatewayWSRoute struct {
	Cookie   string
	ProxyURL string
	Apply    func(http.Header) error
	Release  func()
}
