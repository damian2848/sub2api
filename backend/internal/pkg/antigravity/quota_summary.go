package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
)

// These fields follow the official Antigravity application's embedded
// google/internal/cloud/code/v1internal/quota_summary.proto descriptor.
// The summary supplies actual quota groups/windows; fetchAvailableModels only
// supplies one quota value per model and does not identify its window.
type RetrieveUserQuotaSummaryRequest struct {
	Project string `json:"project"`
}

type QuotaSummaryBucket struct {
	BucketID          string       `json:"bucketId,omitempty"`
	DisplayName       string       `json:"displayName,omitempty"`
	Description       string       `json:"description,omitempty"`
	Window            string       `json:"window,omitempty"`
	RemainingFraction *float64     `json:"remainingFraction,omitempty"`
	RemainingAmount   *json.Number `json:"remainingAmount,omitempty"`
	Disabled          bool         `json:"disabled,omitempty"`
	ResetTime         string       `json:"resetTime,omitempty"`
}

type QuotaSummaryGroup struct {
	DisplayName string               `json:"displayName,omitempty"`
	Description string               `json:"description,omitempty"`
	Buckets     []QuotaSummaryBucket `json:"buckets,omitempty"`
}

type RetrieveUserQuotaSummaryResponse struct {
	Groups      []QuotaSummaryGroup  `json:"groups,omitempty"`
	Buckets     []QuotaSummaryBucket `json:"buckets,omitempty"` // Deprecated upstream, retained for older endpoints.
	Description string               `json:"description,omitempty"`
}

type QuotaSummaryHTTPError struct {
	StatusCode int
	Body       string
}

func (e *QuotaSummaryHTTPError) Error() string {
	return fmt.Sprintf("retrieveUserQuotaSummary failed (HTTP %d): %s", e.StatusCode, e.Body)
}

// RetrieveUserQuotaSummary requests fresh upstream quota windows. The REST
// representation flattens the protobuf remaining oneof into remainingFraction
// or remainingAmount; a missing fraction must never be interpreted as zero.
// It retains the legacy BaseURLs order for callers without a selected endpoint.
// Account usage should use RetrieveUserQuotaSummaryFromURL to query the same
// quota pool as generation requests.
func (c *Client) RetrieveUserQuotaSummary(ctx context.Context, accessToken, projectID string, bodyLimit int64) (*RetrieveUserQuotaSummaryResponse, map[string]any, error) {
	return c.retrieveUserQuotaSummary(ctx, BaseURLs, accessToken, projectID, bodyLimit, false)
}

// RetrieveUserQuotaSummaryFromURL queries the account's selected generation
// endpoint. Different endpoints may expose different quota pools for the same
// account, so neither redirects nor fallback may substitute another endpoint.
func (c *Client) RetrieveUserQuotaSummaryFromURL(ctx context.Context, baseURL, accessToken, projectID string, bodyLimit int64) (*RetrieveUserQuotaSummaryResponse, map[string]any, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, nil, errors.New("retrieveUserQuotaSummary base URL is required")
	}
	return c.retrieveUserQuotaSummary(ctx, []string{baseURL}, accessToken, projectID, bodyLimit, true)
}

func (c *Client) retrieveUserQuotaSummary(ctx context.Context, availableURLs []string, accessToken, projectID string, bodyLimit int64, fixedEndpoint bool) (*RetrieveUserQuotaSummaryResponse, map[string]any, error) {
	if c == nil || c.httpClient == nil {
		return nil, nil, errors.New("antigravity client is not configured")
	}
	if bodyLimit <= 0 {
		return nil, nil, errors.New("retrieveUserQuotaSummary body limit must be positive")
	}
	if len(availableURLs) == 0 {
		return nil, nil, errors.New("retrieveUserQuotaSummary base URL is required")
	}
	body, err := json.Marshal(RetrieveUserQuotaSummaryRequest{Project: projectID})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal quota summary request: %w", err)
	}
	client := c.fetchAvailableModelsHTTPClient()
	if fixedEndpoint {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	for idx, baseURL := range availableURLs {
		req, err := NewAPIRequestWithURL(ctx, baseURL, "retrieveUserQuotaSummary", accessToken, body)
		if err != nil {
			return nil, nil, fmt.Errorf("create quota summary request: %w", err)
		}
		resp, err := servertiming.Do(client, req)
		if err != nil {
			if ctx.Err() == nil && shouldFallbackToNextURL(err, 0) && idx < len(availableURLs)-1 {
				continue
			}
			return nil, nil, fmt.Errorf("retrieveUserQuotaSummary request failed: %w", err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("read quota summary response: %w", readErr)
		}
		if int64(len(responseBody)) > bodyLimit {
			return nil, nil, fmt.Errorf("quota summary response exceeds %d bytes", bodyLimit)
		}
		if shouldFallbackToNextURL(nil, resp.StatusCode) && idx < len(availableURLs)-1 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, nil, &QuotaSummaryHTTPError{StatusCode: resp.StatusCode, Body: string(responseBody)}
		}
		var summary RetrieveUserQuotaSummaryResponse
		if err := json.Unmarshal(responseBody, &summary); err != nil {
			return nil, nil, fmt.Errorf("decode quota summary response: %w", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(responseBody, &raw); err != nil {
			return nil, nil, fmt.Errorf("decode raw quota summary response: %w", err)
		}
		// A successful quota query proves nothing about generation availability.
		// Do not alter forwarding priority or clear its unavailable markers.
		return &summary, raw, nil
	}
	return nil, nil, errors.New("no quota summary endpoint available")
}
