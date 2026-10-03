package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrChannelMonitorProbeGroupMismatch = infraerrors.BadRequest(
		"CHANNEL_MONITOR_PROBE_GROUP_MISMATCH",
		"effective probe API key group does not match monitor group; select an API key bound to the monitored group",
	)
	ErrChannelMonitorProbeKeyUnavailable = infraerrors.BadRequest(
		"CHANNEL_MONITOR_PROBE_KEY_UNAVAILABLE",
		"site-target probe requires an existing local API key in its effective authentication headers",
	)
	ErrChannelMonitorProbeRouteUnavailable = infraerrors.InternalServer(
		"CHANNEL_MONITOR_PROBE_ROUTE_UNAVAILABLE", "probe API key routing validation is unavailable",
	)
)

// ChannelMonitorAPIKeyBinding contains routing identity only, never a secret.
// The repository lookup must read fresh state, not the gateway auth cache: key
// rebinding must invalidate a monitor's old group claim at the next execution.
type ChannelMonitorAPIKeyBinding struct {
	APIKeyID int64
	GroupID  *int64
}

type channelMonitorProbeKeyReader interface {
	GetMonitorAPIKeyBinding(context.Context, string) (*ChannelMonitorAPIKeyBinding, error)
}

type channelMonitorProbeSiteReader interface {
	GetChannelMonitorProbeSiteOrigins(context.Context) ([]string, error)
}

// GetChannelMonitorProbeSiteOrigins uses both documented API and frontend
// origins. An installation may expose the gateway on the frontend hostname or
// on a separate hostname in api_base_url. No request Host header is trusted.
func (s *SettingService) GetChannelMonitorProbeSiteOrigins(ctx context.Context) ([]string, error) {
	var origins []string
	if s == nil {
		return origins, nil
	}
	if s.cfg != nil && strings.TrimSpace(s.cfg.Server.FrontendURL) != "" {
		origins = append(origins, s.cfg.Server.FrontendURL)
	}
	if s.settingRepo != nil {
		baseURL, err := s.settingRepo.GetValue(ctx, SettingKeyAPIBaseURL)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			return nil, ErrChannelMonitorProbeRouteUnavailable
		}
		if strings.TrimSpace(baseURL) != "" {
			origins = append(origins, baseURL)
		}
	}
	return origins, nil
}

func (s *ChannelMonitorService) validateStoredProbeRoute(ctx context.Context, m *ChannelMonitor, newPlainKey string, keyUpdated bool) error {
	if defaultCheckMode(m.CheckMode) == MonitorCheckModeQuota {
		return nil
	}
	plainKey := newPlainKey
	if !keyUpdated {
		var err error
		plainKey, err = s.encryptor.Decrypt(m.APIKey)
		if err != nil {
			return ErrChannelMonitorAPIKeyDecryptFailed
		}
	}
	copy := *m
	copy.APIKey = strings.TrimSpace(plainKey)
	return s.validateProbeRoute(ctx, &copy)
}

func (s *ChannelMonitorService) validateProbeRoute(ctx context.Context, m *ChannelMonitor) error {
	if m == nil || defaultCheckMode(m.CheckMode) == MonitorCheckModeQuota {
		return nil
	}
	isSite := false
	if reader, ok := s.settings.(channelMonitorProbeSiteReader); ok {
		origins, err := reader.GetChannelMonitorProbeSiteOrigins(ctx)
		if err != nil {
			return ErrChannelMonitorProbeRouteUnavailable
		}
		for _, origin := range origins {
			if sameChannelMonitorOrigin(m.Endpoint, origin) {
				isSite = true
				break
			}
		}
	}
	reader, ok := s.repo.(channelMonitorProbeKeyReader)
	if !ok {
		// External endpoint monitoring is supported by older adapters/test
		// doubles. A known site endpoint never bypasses route validation.
		if isSite {
			return ErrChannelMonitorProbeRouteUnavailable
		}
		return nil
	}
	credential := effectiveChannelMonitorProbeKey(m)
	if credential == "" {
		if isSite {
			return ErrChannelMonitorProbeKeyUnavailable
		}
		return nil
	}
	binding, err := reader.GetMonitorAPIKeyBinding(ctx, credential)
	if err != nil {
		// Never wrap lookup errors: a driver could include query arguments,
		// and the effective credential must not reach logs or client errors.
		return ErrChannelMonitorProbeRouteUnavailable
	}
	if binding == nil {
		if isSite {
			return ErrChannelMonitorProbeKeyUnavailable
		}
		return nil // a remote provider key is not a local routing identity
	}
	// Matching a local credential also protects site aliases when origins are
	// not configured. It does not bypass or alter ordinary gateway auth.
	if m.GroupID != nil && (binding.GroupID == nil || *binding.GroupID != *m.GroupID) {
		return ErrChannelMonitorProbeGroupMismatch
	}
	return nil
}

func effectiveChannelMonitorProbeKey(m *ChannelMonitor) string {
	adapter, _, ok := providerAdapterFor(m.Provider, m.APIMode)
	if !ok {
		return ""
	}
	merged := mergeHeaders(adapter.buildHeaders(m.APIKey), &CheckOptions{ExtraHeaders: m.ExtraHeaders})
	headers := make(http.Header, len(merged))
	for name, value := range merged {
		// net/http trims surrounding header whitespace on the wire. Use the
		// transmitted value rather than the raw JSON extra_headers value.
		headers.Set(name, strings.Trim(value, " \t"))
	}
	// The Gemini gateway uses native-header precedence; ordinary gateway
	// endpoints use Bearer, x-api-key, then x-goog-api-key precedence.
	if m.Provider == MonitorProviderGemini {
		if value := strings.TrimSpace(headers.Get("X-Goog-Api-Key")); value != "" {
			return value
		}
	}
	authorization := headers.Get("Authorization")
	if m.Provider == MonitorProviderGemini {
		authorization = strings.TrimSpace(authorization)
	}
	parts := strings.SplitN(authorization, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		if value := strings.TrimSpace(parts[1]); value != "" {
			return value
		}
	}
	if value := headers.Get("X-Api-Key"); value != "" {
		if m.Provider == MonitorProviderGemini {
			return strings.TrimSpace(value)
		}
		return value
	}
	return headers.Get("X-Goog-Api-Key")
}

func sameChannelMonitorOrigin(endpoint, siteURL string) bool {
	left, leftErr := url.Parse(strings.TrimSpace(endpoint))
	right, rightErr := url.Parse(strings.TrimSpace(siteURL))
	if leftErr != nil || rightErr != nil || left.Hostname() == "" || right.Hostname() == "" {
		return false
	}
	port := func(u *url.URL) string {
		if value := u.Port(); value != "" {
			return value
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) && port(left) == port(right)
}

// A broken legacy monitor must remain stoppable without sending traffic or
// requiring a replacement key first. Any other supplied change is validated.
func isChannelMonitorDisableOnlyUpdate(p ChannelMonitorUpdateParams) bool {
	return p.Enabled != nil && !*p.Enabled && p.Name == nil && p.Provider == nil &&
		p.APIMode == nil && p.Endpoint == nil && p.APIKey == nil && p.PrimaryModel == nil &&
		p.ExtraModels == nil && p.GroupName == nil && p.GroupID == nil &&
		p.IntervalSeconds == nil && p.JitterSeconds == nil && p.TemplateID == nil &&
		!p.ClearTemplate && p.ExtraHeaders == nil && p.BodyOverrideMode == nil &&
		p.BodyOverride == nil && p.CheckMode == nil && p.AccountID == nil
}
