package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SessionActivityIdleWindow = 5 * time.Minute
	SessionActivityLease      = 2 * time.Minute
	SessionActivityRetention  = time.Hour
	sessionActivityHeartbeat  = 30 * time.Second
)

// SessionActivityCache is optional: unavailable activity history must prevent
// optional migrations, without preventing new assignments or fault failover.
// Implementations must atomically register requests and use a shared clock.
type SessionActivityCache interface {
	BeginSessionActivity(ctx context.Context, groupID int64, sessionKey, requestID string) (wasIdle bool, err error)
	RefreshSessionActivity(ctx context.Context, groupID int64, sessionKey, requestID string) error
	EndSessionActivity(ctx context.Context, groupID int64, sessionKey, requestID string) error
	SessionActivityAllowsMigration(ctx context.Context, groupID int64, sessionKey, requestID string) (bool, error)
}

type sessionActivityKey struct {
	groupID int64
	session string
}

type sessionActivity struct {
	cache     SessionActivityCache
	key       sessionActivityKey
	requestID string
	wasIdle   bool
}

// Track the whole HTTP request, including queues, streaming and errors. Ending
// a selector or releasing speculative account capacity is not request completion.
// Nested selectors reuse the activity in ctx. Callers without a cancellable
// context get a finite lease (no unbounded goroutine); they cannot migrate.
func trackSessionActivity(ctx context.Context, cache GatewayCache, groupID *int64, sessionKey string) context.Context {
	if sessionKey == "" {
		return ctx
	}
	key := sessionActivityKey{groupID: derefGroupID(groupID), session: sessionKey}
	if _, ok := ctx.Value(key).(*sessionActivity); ok {
		return ctx
	}
	activity := &sessionActivity{key: key, requestID: uuid.NewString()}
	trackedCtx := context.WithValue(ctx, key, activity)
	activity.cache, _ = cache.(SessionActivityCache)
	if activity.cache == nil {
		return trackedCtx
	}
	probeCtx, cancel := context.WithTimeout(ctx, priorityFailbackTimeout)
	idle, err := activity.cache.BeginSessionActivity(probeCtx, key.groupID, key.session, activity.requestID)
	cancel()
	activity.wasIdle = err == nil && idle && ctx.Done() != nil
	if ctx.Done() != nil {
		go activity.maintain(ctx)
	}
	return trackedCtx
}

func (a *sessionActivity) maintain(ctx context.Context) {
	ticker := time.NewTicker(sessionActivityHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Request cancellation must not cancel the completion timestamp write.
			endCtx, cancel := context.WithTimeout(context.Background(), priorityFailbackTimeout)
			_ = a.cache.EndSessionActivity(endCtx, a.key.groupID, a.key.session, a.requestID)
			cancel()
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, priorityFailbackTimeout)
			_ = a.cache.RefreshSessionActivity(refreshCtx, a.key.groupID, a.key.session, a.requestID)
			cancel()
		}
	}
}

func sessionActivityAllowsMigration(ctx context.Context, groupID *int64, sessionKey string) bool {
	a, _ := ctx.Value(sessionActivityKey{groupID: derefGroupID(groupID), session: sessionKey}).(*sessionActivity)
	if a == nil || !a.wasIdle || a.cache == nil || ctx.Err() != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, priorityFailbackTimeout)
	defer cancel()
	allowed, err := a.cache.SessionActivityAllowsMigration(probeCtx, a.key.groupID, a.key.session, a.requestID)
	return err == nil && allowed
}

func (s *OpenAIGatewayService) trackOpenAISessionActivity(ctx context.Context, groupID *int64, sessionHash string) context.Context {
	if strings.TrimSpace(sessionHash) == "" {
		return ctx
	}
	return trackSessionActivity(ctx, s.cache, groupID, s.openAISessionCacheKey(sessionHash))
}
