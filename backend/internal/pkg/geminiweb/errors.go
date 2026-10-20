package geminiweb

import (
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
