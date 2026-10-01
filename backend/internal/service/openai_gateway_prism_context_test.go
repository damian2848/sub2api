package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrismUpstreamCancellationPreservesOtherAccountBillingDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	managed := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{"provider_preset": PrismProviderPreset, PrismSourceAccountKey: int64(32)}}
	prismCtx, releasePrism := openaiAccountUpstreamContext(ctx, managed)
	otherCtx, releaseOther := openaiAccountUpstreamContext(ctx, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey})
	releasePrism()
	releaseOther()
	require.NoError(t, prismCtx.Err())
	cancel()
	require.ErrorIs(t, prismCtx.Err(), context.Canceled)
	require.NoError(t, otherCtx.Err())

	nilCtx, releaseNil := openaiAccountUpstreamContext(nil, managed)
	defer releaseNil()
	require.NotNil(t, nilCtx)
	require.NoError(t, nilCtx.Err())
}
