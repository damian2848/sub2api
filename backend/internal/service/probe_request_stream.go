package service

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// WebSocket generation attempts have the same non-billing facts as HTTP. Begin
// this only at a real response.create write, not admission, queueing or ping.
func beginProbeStreamAttempt(ctx context.Context, model string) *pelicanTestUsage {
	collector := pelicanUsageFromContext(ctx)
	if collector == nil {
		return nil
	}
	status := 101
	u := &pelicanTestUsage{protocol: "openai_ws", model: model, attemptID: uuid.NewString(), startedAt: time.Now(), networkAttempt: true, canTTFT: true, httpStatus: &status}
	collector.requests = append(collector.requests, u)
	return u
}
func finishProbeStreamAttempt(u *pelicanTestUsage, err error, fallback string) {
	if u == nil {
		return
	}
	if err != nil {
		kind := probeRequestErrorKind(err, fallback)
		u.errorKind = &kind
	}
	if u.finishedAt.IsZero() {
		u.finishedAt = time.Now()
	}
}
