package service

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/gin-gonic/gin"
)

const accountTestVerifyModelContextKey = "account_test_verify_model"

// Connectivity alone does not prove that the selected model is available.
// Keep this stricter verdict in account tests, separate from gateway failover
// and billing policy. Missing/ambiguous declarations are not proven failures.
func (s *AccountTestService) finishAccountModelTest(c *gin.Context, sent, actual string, conflict, web bool) error {
	status, detail := accountTestModelVerification(sent, actual, conflict, web)
	s.sendEvent(c, TestEvent{Type: "model_check", Model: sent, UpstreamModel: actual, Status: status, Error: detail})
	if status != "verified" && c.GetBool(accountTestVerifyModelContextKey) {
		return s.sendErrorAndEnd(c, detail)
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func accountTestModelVerification(sent, actual string, conflict, web bool) (string, string) {
	status, detail := accountTestModelVerdict(sent, actual, conflict, web)
	// Gemini Web always declares the serving model, so anything short of a
	// confirmed match means the selected model was not served: a failure, not
	// an inconclusive warning.
	if web && status == "unverified" {
		status = "mismatch"
	}
	return status, detail
}

func accountTestModelVerdict(sent, actual string, conflict, web bool) (string, string) {
	sent, actual = strings.TrimSpace(sent), strings.TrimSpace(actual)
	if conflict {
		return "unverified", "Model identity is unverified: upstream returned conflicting model declarations"
	}
	if actual == "" {
		return "unverified", "Model identity is unverified: upstream did not declare the model used"
	}
	expected := sent
	if web {
		model, ok := geminiweb.LookupModel(sent)
		if !ok {
			return "mismatch", fmt.Sprintf("Unsupported Gemini Web model: %s", sent)
		}
		expected = model.ID
		if model.ID == "gemini-auto" {
			return "verified", ""
		}
		if model.HeaderID == "" && !strings.EqualFold(expected, actual) {
			return "unverified", fmt.Sprintf("Model identity is unverified: requested Web mode %s, upstream returned %s", sent, actual)
		}
	}
	expected, observed := normalizeAntigravityModelName(expected), normalizeAntigravityModelName(actual)
	if expected == observed {
		return "verified", ""
	}
	// Antigravity may report the base Gemini model without the requested
	// thinking-level suffix. Do not collapse different model generations,
	// Flash vs Flash-Lite, or two explicitly different thinking levels.
	if !web && strings.HasPrefix(expected, "gemini-") {
		for _, suffix := range geminiThinkingVariantSuffixes {
			if strings.HasSuffix(expected, suffix) && strings.TrimSuffix(expected, suffix) == observed {
				return "verified", ""
			}
		}
	}
	return "mismatch", fmt.Sprintf("Model mismatch: requested %s, upstream returned %s (possible downgrade)", sent, actual)
}

func observeGeminiTestResponse(body []byte) *upstreamResponseModelObserver {
	observer := &upstreamResponseModelObserver{}
	observer.ObserveGemini(body)
	forEachOpenAISSEFrame(string(body), func(_ string, payload []byte) {
		observer.ObserveGemini(payload)
	})
	return observer
}
