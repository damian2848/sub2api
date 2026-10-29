package geminiweb

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// UpstreamHTTPError retains the upstream status without storing a response body
// or credentials. Callers can distinguish service failures from bad requests
// without matching error text.
type UpstreamHTTPError struct {
	StatusCode int
	Operation  string
}

func (e *UpstreamHTTPError) Error() string {
	prefix := "gemini web"
	if e.Operation != "" {
		prefix += " " + e.Operation
	}
	return fmt.Sprintf("%s upstream: %d %s", prefix, e.StatusCode, http.StatusText(e.StatusCode))
}

// UpstreamRPCError represents a failure in a wrb.fr status envelope, which
// Google can send with HTTP 200. Keep only numeric codes: the status message
// and other details may contain private upstream data.
type UpstreamRPCError struct {
	RPCCode  int
	BardCode int
}

func (e *UpstreamRPCError) Error() string {
	if e.BardCode != 0 {
		return fmt.Sprintf("gemini web upstream RPC error %d: BardErrorInfo %d", e.RPCCode, e.BardCode)
	}
	return fmt.Sprintf("gemini web upstream RPC error %d", e.RPCCode)
}

// Unwrap preserves the established credential/quota handling for known Bard
// codes. An unknown code, including 1095, does not establish either cause.
func (e *UpstreamRPCError) Unwrap() error {
	switch e.BardCode {
	case 1016:
		return ErrInvalidCredential
	case 1037, 1013:
		return ErrRateLimited
	default:
		return nil
	}
}

func parseRPCError(raw json.RawMessage) *UpstreamRPCError {
	var status []json.RawMessage
	if json.Unmarshal(raw, &status) != nil || len(status) == 0 {
		return nil
	}
	var code int
	if json.Unmarshal(status[0], &code) != nil || code <= 0 {
		return nil
	}
	err := &UpstreamRPCError{RPCCode: code}
	if len(status) < 3 {
		return err
	}
	var details [][]json.RawMessage
	if json.Unmarshal(status[2], &details) != nil {
		return err
	}
	for _, detail := range details {
		if len(detail) < 2 {
			continue
		}
		var typeURL string
		if json.Unmarshal(detail[0], &typeURL) != nil {
			continue
		}
		switch typeURL {
		case "type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",
			"type.googleapis.com/google.internal.bard.v1.BardErrorInfo":
		default:
			continue
		}
		var info []json.RawMessage
		if json.Unmarshal(detail[1], &info) == nil && len(info) > 0 {
			var bardCode int
			if json.Unmarshal(info[0], &bardCode) == nil && bardCode > 0 {
				err.BardCode = bardCode
				break
			}
		}
	}
	return err
}
