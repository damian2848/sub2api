package service

import "context"

type deferredGatewayStickyBindingKey struct{}

// WithDeferredGatewayStickyBinding keeps speculative selection from changing
// the session binding. Callers retain session eligibility checks and bind only
// the account whose slot is acquired and passes final admission.
func WithDeferredGatewayStickyBinding(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferredGatewayStickyBindingKey{}, true)
}

func gatewayStickyBindingDeferred(ctx context.Context) bool {
	deferred, _ := ctx.Value(deferredGatewayStickyBindingKey{}).(bool)
	return deferred
}
