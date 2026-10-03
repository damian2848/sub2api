package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	PrismRestartRequestLimit  = PrismConfigurationRequestLimit
	prismRestartResponseLimit = 64 << 10
	prismRestartTimeout       = 5 * time.Second
)

// PrismRestartRequest binds the action to the boot and complete saved startup
// configuration the administrator reviewed. It never carries execution or
// connection instructions.
type PrismRestartRequest struct {
	ExpectedRuntimeID     string                    `json:"expected_runtime_id"`
	ExpectedConfiguration PrismConfigurationOptions `json:"expected_configuration"`
}

// PrismRestartRuntime is the fixed sidecar restart capability snapshot. The
// runtime ID identifies one boot, not an account or a management credential.
type PrismRestartRuntime struct {
	Supported bool   `json:"supported"`
	RuntimeID string `json:"runtime_id"`
	State     string `json:"state"`
}

type PrismRestartResult struct {
	Availability string               `json:"availability"`
	Runtime      *PrismRestartRuntime `json:"runtime"`
}

var prismRestartRuntimeID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (request PrismRestartRequest) valid() bool {
	return prismRestartRuntimeID.MatchString(request.ExpectedRuntimeID) && request.ExpectedConfiguration.valid()
}

func invalidPrismRestart() error {
	return infraerrors.BadRequest("PRISM_RESTART_INVALID", "Invalid Prism restart request")
}

// DecodePrismRestartRequest rejects unknown, missing, duplicate, null and
// case-variant keys in both the action and the complete startup configuration.
// Parsing errors are reduced to one safe public error, never input text.
func DecodePrismRestartRequest(reader io.Reader) (PrismRestartRequest, error) {
	var request PrismRestartRequest
	if reader == nil {
		return request, invalidPrismRestart()
	}
	data, err := io.ReadAll(io.LimitReader(reader, PrismRestartRequestLimit+1))
	if err != nil || len(data) > PrismRestartRequestLimit {
		return request, invalidPrismRestart()
	}
	fields, err := prismConfigurationObject(data, []string{"expected_runtime_id", "expected_configuration"})
	if err != nil || json.Unmarshal(fields["expected_runtime_id"], &request.ExpectedRuntimeID) != nil {
		return PrismRestartRequest{}, invalidPrismRestart()
	}
	request.ExpectedConfiguration, err = DecodePrismConfigurationOptions(bytes.NewReader(fields["expected_configuration"]))
	if err != nil || !request.valid() {
		return PrismRestartRequest{}, invalidPrismRestart()
	}
	return request, nil
}

func decodePrismRestartRuntime(data []byte) (*PrismRestartRuntime, error) {
	if _, err := prismConfigurationObject(data, []string{"supported", "runtime_id", "state"}); err != nil {
		return nil, err
	}
	var runtime PrismRestartRuntime
	if json.Unmarshal(data, &runtime) != nil || !prismRestartRuntimeID.MatchString(runtime.RuntimeID) ||
		(runtime.State != "ready" && runtime.State != "restarting") {
		return nil, errors.New("invalid restart runtime")
	}
	return &runtime, nil
}

func prismRestartUnavailable(availability string) error {
	reason, message := "PRISM_RESTART_UNAVAILABLE", "The Prism restart service is unavailable"
	switch availability {
	case "not_configured":
		reason, message = "PRISM_RESTART_NOT_CONFIGURED", "The Prism management connection is not configured"
	case "unsupported":
		reason, message = "PRISM_RESTART_UNSUPPORTED", "The Prism sidecar does not support managed restart"
	}
	return infraerrors.New(http.StatusServiceUnavailable, reason, message)
}

func prismRestartConflict() error {
	return infraerrors.New(http.StatusConflict, "PRISM_RESTART_CONFLICT", "The Prism runtime or saved startup configuration has changed")
}

// Only this fixed sidecar error code is used to distinguish an unsupported
// managed deployment from an ordinary 503. No upstream message is forwarded.
func prismRestartUnsupportedResponse(data []byte) bool {
	fields, err := prismConfigurationObject(data, []string{"error"})
	if err != nil {
		return false
	}
	detail, err := prismConfigurationObject(fields["error"], []string{"code", "message", "type"})
	if err != nil {
		return false
	}
	var code, message, kind string
	return json.Unmarshal(detail["code"], &code) == nil &&
		json.Unmarshal(detail["message"], &message) == nil &&
		json.Unmarshal(detail["type"], &kind) == nil &&
		code == "prism_restart_unsupported" && kind == "prism_error"
}

// requestRestart only contacts the immutable startup management origin and
// fixed restart path. Routing may be disabled independently of this connection.
// Observer denial precedes validation and every management request.
func (s *PrismAccountService) requestRestart(ctx context.Context, method string, request *PrismRestartRequest) (*PrismRestartResult, error) {
	if _, observer := ObserverGroupIDs(ctx); observer {
		return nil, ErrObserverScope
	}
	var body io.Reader
	if request != nil {
		if !request.valid() {
			return nil, invalidPrismRestart()
		}
		data, err := json.Marshal(request)
		if err != nil {
			return nil, invalidPrismRestart()
		}
		body = bytes.NewReader(data)
	}
	result := &PrismRestartResult{Availability: "not_configured"}
	if s == nil || s.managementCfg.URL == "" || s.managementCfg.ManagementKey == "" {
		if method == http.MethodGet {
			return result, nil
		}
		return nil, prismRestartUnavailable(result.Availability)
	}
	result.Availability = "unavailable"
	requestCtx, cancel := context.WithTimeout(ctx, prismRestartTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, s.managementCfg.URL+"/internal/restart", body)
	if err != nil {
		if method == http.MethodGet {
			return result, nil
		}
		return nil, prismRestartUnavailable("unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+s.managementCfg.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{}
	if s.client != nil {
		client = *s.client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		switch resp.StatusCode {
		case http.StatusNotFound:
			result.Availability = "unsupported"
		case http.StatusConflict:
			if method == http.MethodPost {
				return nil, prismRestartConflict()
			}
		case http.StatusOK, http.StatusAccepted, http.StatusServiceUnavailable:
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, prismRestartResponseLimit+1))
			if readErr != nil || len(data) > prismRestartResponseLimit {
				break
			}
			if resp.StatusCode == http.StatusServiceUnavailable {
				if prismRestartUnsupportedResponse(data) {
					result.Availability = "unsupported"
				}
				break
			}
			if (method == http.MethodGet && resp.StatusCode != http.StatusOK) ||
				(method == http.MethodPost && resp.StatusCode != http.StatusAccepted) {
				break
			}
			runtime, decodeErr := decodePrismRestartRuntime(data)
			if decodeErr != nil {
				break
			}
			if method == http.MethodGet && !runtime.Supported {
				result.Availability = "unsupported"
				break
			}
			if !runtime.Supported || (method == http.MethodPost &&
				(runtime.RuntimeID != request.ExpectedRuntimeID || runtime.State != "restarting")) {
				break
			}
			result.Availability, result.Runtime = "ready", runtime
		}
	}
	if method != http.MethodGet && result.Availability != "ready" {
		return nil, prismRestartUnavailable(result.Availability)
	}
	return result, nil
}

func (s *PrismAccountService) GetRestartStatus(ctx context.Context) (*PrismRestartResult, error) {
	return s.requestRestart(ctx, http.MethodGet, nil)
}

func (s *PrismAccountService) Restart(ctx context.Context, request PrismRestartRequest) (*PrismRestartResult, error) {
	return s.requestRestart(ctx, http.MethodPost, &request)
}
