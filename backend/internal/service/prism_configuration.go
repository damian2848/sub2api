package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	PrismConfigurationRequestLimit  = 16 << 10
	prismConfigurationResponseLimit = 64 << 10
	prismConfigurationTimeout       = 5 * time.Second
	prismConfigurationMemoryMax     = 1048576
)

// PrismConfigurationOptions contains only sidecar startup flags, never gateway
// connection settings, credentials, file paths, or arbitrary environment keys.
type PrismConfigurationOptions struct {
	// Enabled is the Prism master routing switch. It is persisted by the
	// backend as a live admission gate and mirrored to the sidecar's startup
	// configuration for restart-safe snapshots.
	Enabled          bool `json:"enabled"`
	ProjectIsolation bool `json:"project_isolation"`
	HTTPCache        bool `json:"http_cache"`
	MultiplexPages   bool `json:"multiplex_pages"`
	PrewarmChat      bool `json:"prewarm_chat"`
	StreamReasoning  bool `json:"stream_reasoning"`
	MemoryLimitMiB   int  `json:"memory_limit_mib"`
	MemoryReserveMiB int  `json:"memory_reserve_mib"`
}

type PrismConfigurationState struct {
	Effective       PrismConfigurationOptions `json:"effective"`
	Desired         PrismConfigurationOptions `json:"desired"`
	RestartRequired bool                      `json:"restart_required"`
	Source          string                    `json:"source"`
	ApplyMode       string                    `json:"apply_mode"`
}

type PrismGatewayConfiguration struct {
	Enabled                 bool   `json:"enabled"`
	Configured              bool   `json:"configured"`
	BaseURL                 string `json:"base_url"`
	ManagementKeyConfigured bool   `json:"management_key_configured"`
}

type PrismConfigurationResult struct {
	Gateway       PrismGatewayConfiguration `json:"gateway"`
	Availability  string                    `json:"availability"`
	Configuration *PrismConfigurationState  `json:"configuration"`
}

var prismConfigurationOptionKeys = []string{
	"enabled", "project_isolation", "http_cache", "multiplex_pages", "prewarm_chat", "stream_reasoning",
	"memory_limit_mib", "memory_reserve_mib",
}

// prismConfigurationObject rejects missing, unknown, duplicate, and null fields.
// Unlike struct decoding it also rejects case variants of the public keys.
func prismConfigurationObject(data []byte, keys []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid configuration object")
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] {
			return nil, errors.New("invalid configuration field")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate configuration field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("invalid configuration value")
		}
		fields[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != len(keys) {
		return nil, errors.New("incomplete configuration object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing configuration data")
	}
	return fields, nil
}

func (options PrismConfigurationOptions) valid() bool {
	return options.MemoryLimitMiB >= 0 && options.MemoryLimitMiB <= prismConfigurationMemoryMax &&
		options.MemoryReserveMiB >= 0 && options.MemoryReserveMiB <= prismConfigurationMemoryMax &&
		(options.MemoryLimitMiB == 0 || options.MemoryLimitMiB > options.MemoryReserveMiB)
}

func decodePrismConfigurationOptions(data []byte) (PrismConfigurationOptions, error) {
	var options PrismConfigurationOptions
	if _, err := prismConfigurationObject(data, prismConfigurationOptionKeys); err != nil {
		return options, err
	}
	if err := json.Unmarshal(data, &options); err != nil || !options.valid() {
		return PrismConfigurationOptions{}, errors.New("invalid configuration options")
	}
	return options, nil
}

// DecodePrismConfigurationOptions bounds and validates the complete public PUT
// document. All parsing errors are deliberately reduced to a fixed safe error.
func DecodePrismConfigurationOptions(reader io.Reader) (PrismConfigurationOptions, error) {
	if reader == nil {
		return PrismConfigurationOptions{}, invalidPrismConfiguration()
	}
	data, err := io.ReadAll(io.LimitReader(reader, PrismConfigurationRequestLimit+1))
	if err != nil || len(data) > PrismConfigurationRequestLimit {
		return PrismConfigurationOptions{}, invalidPrismConfiguration()
	}
	options, err := decodePrismConfigurationOptions(data)
	if err != nil {
		return PrismConfigurationOptions{}, invalidPrismConfiguration()
	}
	return options, nil
}

func invalidPrismConfiguration() error {
	return infraerrors.BadRequest("PRISM_SETTINGS_INVALID", "Invalid Prism startup configuration")
}

func decodePrismConfigurationState(data []byte) (*PrismConfigurationState, error) {
	fields, err := prismConfigurationObject(data, []string{"effective", "desired", "restart_required", "source", "apply_mode"})
	if err != nil {
		return nil, err
	}
	state := &PrismConfigurationState{}
	state.Effective, err = decodePrismConfigurationOptions(fields["effective"])
	if err != nil {
		return nil, err
	}
	state.Desired, err = decodePrismConfigurationOptions(fields["desired"])
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(fields["restart_required"], &state.RestartRequired) != nil ||
		json.Unmarshal(fields["source"], &state.Source) != nil || json.Unmarshal(fields["apply_mode"], &state.ApplyMode) != nil ||
		(state.Source != "environment" && state.Source != "saved") || state.ApplyMode != "restart" ||
		state.RestartRequired != (state.Effective != state.Desired) {
		return nil, errors.New("invalid configuration state")
	}
	return state, nil
}

func (s *PrismAccountService) configurationMetadata(ctx context.Context) *PrismConfigurationResult {
	result := &PrismConfigurationResult{Availability: "not_configured"}
	if s == nil {
		return result
	}
	result.Gateway = PrismGatewayConfiguration{
		Enabled: s.globallyEnabled(ctx), BaseURL: s.managementCfg.URL,
		Configured:              s.managementCfg.URL != "" && s.managementCfg.ManagementKey != "",
		ManagementKeyConfigured: s.managementCfg.ManagementKey != "",
	}
	return result
}

func prismConfigurationUnavailable(availability string) error {
	reason, message := "PRISM_SETTINGS_UNAVAILABLE", "The Prism configuration service is unavailable"
	switch availability {
	case "not_configured":
		reason, message = "PRISM_SETTINGS_NOT_CONFIGURED", "The Prism management connection is not configured"
	case "unsupported":
		reason, message = "PRISM_SETTINGS_UNSUPPORTED", "The Prism sidecar does not support startup configuration"
	}
	return infraerrors.New(http.StatusServiceUnavailable, reason, message)
}

// requestConfiguration can only contact the immutable startup origin and one
// fixed management path. Neither the destination nor its bearer comes from the
// browser. Returned bodies are never forwarded or included in an error.
func (s *PrismAccountService) requestConfiguration(ctx context.Context, method string, options *PrismConfigurationOptions) (*PrismConfigurationResult, error) {
	if _, observer := ObserverGroupIDs(ctx); observer {
		return nil, ErrObserverScope
	}
	// Serialize mutations so a concurrent PUT/DELETE cannot persist an older
	// sidecar response after a newer one. The setting is written only after the
	// corresponding sidecar request has returned a valid ready state.
	if method != http.MethodGet && s != nil {
		s.configuration.Lock()
		defer s.configuration.Unlock()
	}
	result := s.configurationMetadata(ctx)
	if !result.Gateway.Configured {
		if method == http.MethodGet {
			return result, nil
		}
		return nil, prismConfigurationUnavailable(result.Availability)
	}
	var body io.Reader
	if options != nil {
		if !options.valid() {
			return nil, invalidPrismConfiguration()
		}
		data, err := json.Marshal(options)
		if err != nil {
			return nil, invalidPrismConfiguration()
		}
		body = bytes.NewReader(data)
	}
	requestCtx, cancel := context.WithTimeout(ctx, prismConfigurationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, s.managementCfg.URL+"/internal/config", body)
	if err != nil {
		return nil, prismConfigurationUnavailable("unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+s.managementCfg.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	result.Availability = "unavailable"
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			result.Availability = "unsupported"
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, prismConfigurationResponseLimit+1))
			if readErr == nil && len(data) <= prismConfigurationResponseLimit {
				if state, decodeErr := decodePrismConfigurationState(data); decodeErr == nil {
					result.Availability, result.Configuration = "ready", state
				}
			}
		}
	}
	if method != http.MethodGet && result.Availability != "ready" {
		return nil, prismConfigurationUnavailable(result.Availability)
	}
	if (method == http.MethodPut || method == http.MethodDelete) && s.settings != nil && result.Configuration != nil {
		// The sidecar is authoritative for the desired state. In particular,
		// DELETE may reset to a sidecar environment value that differs from the
		// backend process environment.
		enabled := result.Configuration.Desired.Enabled
		if err := s.settings.SetPrismEnabled(ctx, enabled); err != nil {
			return nil, prismConfigurationUnavailable("unavailable")
		}
		result.Gateway.Enabled = enabled
	}
	return result, nil
}

func (s *PrismAccountService) GetConfiguration(ctx context.Context) (*PrismConfigurationResult, error) {
	return s.requestConfiguration(ctx, http.MethodGet, nil)
}

func (s *PrismAccountService) UpdateConfiguration(ctx context.Context, options PrismConfigurationOptions) (*PrismConfigurationResult, error) {
	return s.requestConfiguration(ctx, http.MethodPut, &options)
}

func (s *PrismAccountService) ResetConfiguration(ctx context.Context) (*PrismConfigurationResult, error) {
	return s.requestConfiguration(ctx, http.MethodDelete, nil)
}
