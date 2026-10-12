//go:build unit

package service

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type channelModelsAccountRepo struct {
	AccountRepository
	accounts []Account
	err      error
	calls    int
}

func (r *channelModelsAccountRepo) ListModelAvailabilityCandidates(_ context.Context, _ *int64, _ []string, _ bool) ([]Account, error) {
	r.calls++
	return r.accounts, r.err
}
func TestFilterSupportedChannelModels_GroupCapabilities(t *testing.T) {
	future := time.Now().Add(time.Hour)
	repo := &channelModelsAccountRepo{accounts: []Account{{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, RateLimitResetAt: &future,
		Credentials:   map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5", "gpt-4o": "gpt-4o"}},
		AccountGroups: []AccountGroup{{GroupID: 1, AllowedModels: []string{"gpt-5"}}, {GroupID: 2, AllowedModels: []string{"gpt-4o"}}},
	}}}
	svc := &GatewayService{accountRepo: repo}
	models := []SupportedModel{{Name: "gpt-5", Platform: PlatformOpenAI}, {Name: "gpt-4o", Platform: PlatformOpenAI}, {Name: "gpt-3.5-turbo", Platform: PlatformOpenAI}, {Name: "gpt-*", Platform: PlatformOpenAI}, {Name: "claude-sonnet", Platform: PlatformAnthropic}}
	first, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Equal(t, []SupportedModel{models[0]}, first)
	second, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 2}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Equal(t, []SupportedModel{models[1]}, second)
	require.Equal(t, 2, repo.calls)
	blocked, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-4o"}}}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Empty(t, blocked)
}
func TestFilterSupportedChannelModels_EmptyPoolAndFailure(t *testing.T) {
	repo := &channelModelsAccountRepo{}
	svc := &GatewayService{accountRepo: repo}
	models := []SupportedModel{{Name: "gpt-5", Platform: PlatformOpenAI}}
	got, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Empty(t, got)
	repo.err = errors.New("database unavailable")
	got, err = svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1}, PlatformOpenAI, models)
	require.ErrorIs(t, err, repo.err)
	require.Nil(t, got)
}

func TestFilterSupportedChannelModels_ChannelMapping(t *testing.T) {
	channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{1}, ModelMapping: map[string]map[string]string{PlatformOpenAI: {"public-alias": "gpt-5"}}}
	channels := &ChannelService{}
	channels.cache.Store(populateChannelCache([]Channel{channel}, map[int64]string{1: PlatformOpenAI}))
	repo := &channelModelsAccountRepo{accounts: []Account{{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5"}}}}}
	svc := &GatewayService{accountRepo: repo, channelService: channels}
	models := []SupportedModel{{Name: "public-alias", Platform: PlatformOpenAI}, {Name: "pricing-only", Platform: PlatformOpenAI}}
	got, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Equal(t, models[:1], got)
}
func TestFilterSupportedChannelModels_OpenAIOAuthDoesNotAdvertiseEntireCatalog(t *testing.T) {
	repo := &channelModelsAccountRepo{accounts: []Account{{Platform: PlatformOpenAI, Type: AccountTypeOAuth}}}
	svc := &GatewayService{accountRepo: repo}
	models := []SupportedModel{{Name: "gpt-5.4", Platform: PlatformOpenAI}, {Name: "gpt-3.5-turbo", Platform: PlatformOpenAI}, {Name: "gpt-audio", Platform: PlatformOpenAI}}
	got, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Equal(t, models[:1], got)
}

func TestFilterSupportedChannelModels_OAuthSyncedCatalog(t *testing.T) {
	account := Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{"gpt-new": {ID: "gpt-new"}}})
	repo := &channelModelsAccountRepo{accounts: []Account{account}}
	svc := &GatewayService{accountRepo: repo}
	models := []SupportedModel{{Name: "gpt-new", Platform: PlatformOpenAI}, {Name: "gpt-5.4", Platform: PlatformOpenAI}}
	got, err := svc.FilterSupportedChannelModels(context.Background(), &Group{ID: 1}, PlatformOpenAI, models)
	require.NoError(t, err)
	require.Equal(t, models[:1], got)
}
