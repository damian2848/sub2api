package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type encryptedAffinityTestCache struct {
	GatewayCache
	mu                sync.Mutex
	values            map[string]int64
	readErr, writeErr error
	reads, claims     int
}

func (c *encryptedAffinityTestCache) GetSessionAccountID(_ context.Context, groupID int64, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.readErr != nil {
		return 0, c.readErr
	}
	owner, found := c.values[fmt.Sprintf("%d:%s", groupID, key)]
	if !found {
		return 0, ErrStickySessionNotFound
	}
	return owner, nil
}

func (c *encryptedAffinityTestCache) ClaimSessionAccountID(_ context.Context, groupID int64, key string, owner int64, _ time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims++
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.values == nil {
		c.values = make(map[string]int64)
	}
	key = fmt.Sprintf("%d:%s", groupID, key)
	if existing, found := c.values[key]; found {
		return existing, nil
	}
	c.values[key] = owner
	return owner, nil
}

func (c *encryptedAffinityTestCache) SetSessionAccountID(_ context.Context, groupID int64, key string, owner int64, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]int64)
	}
	c.values[fmt.Sprintf("%d:%s", groupID, key)] = owner
	return nil
}
func (c *encryptedAffinityTestCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}
func (c *encryptedAffinityTestCache) DeleteSessionAccountID(_ context.Context, groupID int64, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, fmt.Sprintf("%d:%s", groupID, key))
	return nil
}

func encryptedAffinityProducerItem(t *testing.T, message, metadata string) []byte {
	t.Helper()
	args, err := json.Marshal(map[string]string{"message": message, "task_name": "worker"})
	require.NoError(t, err)
	item := map[string]any{"type": "function_call", "namespace": "collaboration", "name": "spawn_agent", "arguments": string(args)}
	if metadata != "missing" {
		var value any
		require.NoError(t, json.Unmarshal([]byte(metadata), &value))
		item["encrypted_function_args"] = value
	}
	raw, err := json.Marshal(map[string]any{"type": "response.output_item.done", "item": item})
	require.NoError(t, err)
	return raw
}

func encryptedAffinityInput(t *testing.T, messages ...string) []byte {
	t.Helper()
	items := make([]any, 0, len(messages))
	for _, message := range messages {
		items = append(items, map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "encrypted_content", "encrypted_content": message}}})
	}
	raw, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	return raw
}

func requireEncryptedAffinityCode(t *testing.T, err error, code string) {
	t.Helper()
	var affinityErr *OpenAIEncryptedContentAffinityError
	require.ErrorAs(t, err, &affinityErr)
	require.Equal(t, code, affinityErr.Code)
}

func TestOpenAIEncryptedContentAffinityMatchesCodexMessageBytes(t *testing.T) {
	const ciphertext = "opaque\\cipher\"text\n\t中文  "
	want := openAIEncryptedContentDigest(ciphertext)
	for _, metadata := range []string{`["message"]`, `null`, `missing`, `["another_field"]`} {
		t.Run(metadata, func(t *testing.T) {
			output, err := openAIEncryptedOutputDigests(encryptedAffinityProducerItem(t, ciphertext, metadata))
			require.NoError(t, err)
			require.Equal(t, []string{want}, output)
			input, err := openAIEncryptedInputDigests(encryptedAffinityInput(t, ciphertext))
			require.NoError(t, err)
			require.Equal(t, output, input)
		})
	}
	digests, err := openAIEncryptedOutputDigests(encryptedAffinityProducerItem(t, ciphertext, `[]`))
	require.NoError(t, err)
	require.Empty(t, digests, "an explicit empty list is Codex's plaintext marker")
}

func TestOpenAIEncryptedContentAffinityOnlyParsesProtocolLocations(t *testing.T) {
	body := []byte(`{"type":"response.create","response":{"input":[
	 {"type":"reasoning","encrypted_content":"reasoning-private"},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"{\"type\":\"encrypted_content\",\"encrypted_content\":\"untrusted-json\"}"}]},
	 {"type":"agent_message","content":[{"type":"encrypted_content","data":"one"}]},
	 {"type":"function_call_output","output":[{"type":"encrypted_content","encrypted_content":"two"}]},
	 {"type":"custom_tool_call_output","output":[{"type":"encrypted_content","encrypted_content":"one"}]}
	]}}`)
	digests, err := openAIEncryptedInputDigests(body)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{openAIEncryptedContentDigest("one"), openAIEncryptedContentDigest("two")}, digests)
	added := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"incomplete\"}"}}`)
	digests, err = openAIEncryptedOutputDigests(added)
	require.NoError(t, err)
	require.Empty(t, digests, "an added item is not a finalized message")
}

func TestOpenAIEncryptedContentObserverDurablyBindsBeforeDeliveryAndDeduplicates(t *testing.T) {
	cache := &encryptedAffinityTestCache{}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(51)
	observer := svc.NewOpenAIEncryptedContentObserver(context.Background(), &groupID, 91, 17)
	item := encryptedAffinityProducerItem(t, "cipher-A", `["message"]`)
	require.NoError(t, observer.Observe(item))
	require.Equal(t, 1, cache.claims)
	// A different process has no local map and must still resolve the producer.
	reader := NewOpenAIWSStateStore(cache)
	owner, err := reader.GetEncryptedContentAccount(context.Background(), groupID, 91, openAIEncryptedContentDigest("cipher-A"))
	require.NoError(t, err)
	require.Equal(t, int64(17), owner)
	require.NoError(t, observer.Observe(item))
	require.Equal(t, 1, cache.claims)
	for _, scope := range [][2]int64{{52, 91}, {51, 92}} {
		owner, err := reader.GetEncryptedContentAccount(context.Background(), scope[0], scope[1], openAIEncryptedContentDigest("cipher-A"))
		require.NoError(t, err)
		require.Zero(t, owner)
	}
	for key := range cache.values {
		require.NotContains(t, key, "cipher-A")
	}
	before := cache.reads
	require.NoError(t, observer.Observe([]byte(`{"type":"response.output_text.delta","delta":"ordinary output"}`)))
	require.Equal(t, before, cache.reads, "ordinary output adds no cache calls")
}

func TestOpenAIEncryptedContentObserverDoesNotPublishOnCacheFailure(t *testing.T) {
	cache := &encryptedAffinityTestCache{writeErr: errors.New("redis unavailable")}
	svc := &OpenAIGatewayService{cache: cache}
	groupID := int64(1)
	observer := svc.NewOpenAIEncryptedContentObserver(context.Background(), &groupID, 2, 3)
	item := encryptedAffinityProducerItem(t, "private-task-cipher", "missing")
	err := observer.Observe(item)
	requireEncryptedAffinityCode(t, err, "encrypted_content_affinity_unavailable")
	require.NotContains(t, err.Error(), "private-task-cipher")
	owner, err := svc.getOpenAIWSStateStore().GetEncryptedContentAccount(context.Background(), groupID, 2, openAIEncryptedContentDigest("private-task-cipher"))
	require.NoError(t, err)
	require.Zero(t, owner, "failed shared writes must not leave a locally successful binding")
	cache.writeErr = nil
	require.NoError(t, observer.Observe(item))
	require.Equal(t, 2, cache.claims, "the failed item was not deduplicated as successful")
	cache.readErr = errors.New("redis read unavailable")
	reader := &OpenAIGatewayService{cache: cache}
	err = reader.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 3, encryptedAffinityInput(t, "private-task-cipher"))
	requireEncryptedAffinityCode(t, err, "encrypted_content_affinity_unavailable")
}

func TestOpenAIEncryptedContentConcurrentClaimsPreserveFirstOwner(t *testing.T) {
	cache := &encryptedAffinityTestCache{}
	digest := openAIEncryptedContentDigest("same-cipher")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, owner := range []int64{11, 22} {
		wg.Add(1)
		go func(owner int64) {
			defer wg.Done()
			errs <- NewOpenAIWSStateStore(cache).BindEncryptedContentAccount(context.Background(), 1, 2, digest, owner, time.Hour)
		}(owner)
	}
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			requireEncryptedAffinityCode(t, err, "encrypted_content_account_conflict")
			conflicts++
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
}

func TestOpenAIEncryptedContentExistingConnectionAndConflictProtection(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(1)
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindEncryptedContentAccount(context.Background(), groupID, 2, openAIEncryptedContentDigest("first"), 11, time.Hour))
	require.NoError(t, store.BindEncryptedContentAccount(context.Background(), groupID, 2, openAIEncryptedContentDigest("second"), 22, time.Hour))
	require.NoError(t, svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 11, encryptedAffinityInput(t, "first", "unknown")))
	err := svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 22, encryptedAffinityInput(t, "first"))
	requireEncryptedAffinityCode(t, err, "encrypted_content_account_mismatch")
	err = svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 11, encryptedAffinityInput(t, "first", "second"))
	requireEncryptedAffinityCode(t, err, "encrypted_content_account_conflict")
	var affinityErr *OpenAIEncryptedContentAffinityError
	require.ErrorAs(t, err, &affinityErr)
	require.Equal(t, http.StatusBadRequest, affinityErr.HTTPStatus())
}

func TestOpenAIEncryptedContentObserverLongConnectionRemainsBounded(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(1)
	observer := svc.NewOpenAIEncryptedContentObserver(context.Background(), &groupID, 2, 3)
	for i := 0; i < openAIEncryptedContentMaxDigests+5; i++ {
		require.NoError(t, observer.Observe(encryptedAffinityProducerItem(t, fmt.Sprintf("cipher-%d", i), "missing")))
	}
	require.LessOrEqual(t, len(observer.seen), openAIEncryptedContentMaxDigests)
	require.NoError(t, observer.Observe([]byte(`{"type":"response.created","response":{"id":"next"}}`)))
	require.Empty(t, observer.seen)
}

func encryptedAffinitySchedulerService(t *testing.T, mode string, mutate func(*Account)) (*OpenAIGatewayService, int64) {
	t.Helper()
	groupID := int64(3001)
	owner := Account{ID: 401, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 20, GroupIDs: []int64{groupID}, Credentials: map[string]any{"plan_type": "team"}}
	other := Account{ID: 402, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, Credentials: map[string]any{"plan_type": "team"}}
	if mutate != nil {
		mutate(&owner)
	}
	cache := &encryptedAffinityTestCache{}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{owner, other}}},
		cache:       cache, cfg: &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(mode, "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{401: true, 402: true}}),
	}
	require.NoError(t, svc.getOpenAIWSStateStore().BindEncryptedContentAccount(context.Background(), groupID, 12, openAIEncryptedContentDigest("task"), 401, time.Hour))
	return svc, groupID
}

func TestOpenAIEncryptedContentSchedulerPinsProducerAcrossModes(t *testing.T) {
	for _, mode := range []string{"false", "true"} {
		t.Run(mode, func(t *testing.T) {
			svc, groupID := encryptedAffinitySchedulerService(t, mode, nil)
			ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
			selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "new-child-thread", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(401), selection.Account.ID)
			require.Equal(t, openAIAccountScheduleLayerEncryptedContent, decision.Layer)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestOpenAIEncryptedContentSingleObjectInputPinsProducer(t *testing.T) {
	input := `{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"task"}]}`
	for _, body := range []string{
		`{"input":` + input + `}`,
		`{"type":"response.create","response":{"input":` + input + `}}`,
		`{"type":"response.inject","input":` + input + `}`,
	} {
		for _, mode := range []string{"false", "true"} {
			svc, groupID := encryptedAffinitySchedulerService(t, mode, nil)
			ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, []byte(body))
			selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "object-input-child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(401), selection.Account.ID)
			require.Equal(t, openAIAccountScheduleLayerEncryptedContent, decision.Layer)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			err = svc.ValidateOpenAIEncryptedContentAccount(ctx, &groupID, 12, 402, []byte(body))
			requireEncryptedAffinityCode(t, err, "encrypted_content_account_mismatch")
		}
	}
}

func TestOpenAIEncryptedContentSchedulerNeverEscapesUnavailableProducer(t *testing.T) {
	for name, mutate := range map[string]func(*Account){
		"disabled":           func(a *Account) { a.Schedulable = false },
		"different group":    func(a *Account) { a.GroupIDs = []int64{999} },
		"different platform": func(a *Account) { a.Platform = PlatformGrok },
		"model unsupported":  func(a *Account) { a.Credentials["model_mapping"] = map[string]any{"another-model": "gpt-4o"} },
	} {
		t.Run(name, func(t *testing.T) {
			svc, groupID := encryptedAffinitySchedulerService(t, "true", mutate)
			ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selection)
			requireEncryptedAffinityCode(t, err, "encrypted_content_account_unavailable")
		})
	}
	svc, groupID := encryptedAffinitySchedulerService(t, "true", nil)
	ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child", "gpt-5.6-sol", map[int64]struct{}{401: {}}, OpenAIUpstreamTransportAny, false)
	require.Nil(t, selection)
	requireEncryptedAffinityCode(t, err, "encrypted_content_account_unavailable")
}

func encryptedAffinityModelCooldown(reason string) func(*Account) {
	return func(a *Account) {
		a.Extra = map[string]any{modelRateLimitsKey: map[string]any{"gpt-5.6-sol": map[string]any{
			"rate_limit_reset_at": time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339),
			"reason":              reason,
		}}}
	}
}

// A transient model-not-found on the producer must not fail the conversation
// for the whole cooldown: the producer is the only account that can read it.
func TestOpenAIEncryptedContentSchedulerProbesProducerDuringModelNotFoundCooldown(t *testing.T) {
	for _, reason := range []string{upstreamModelNotFoundReason, upstreamModelNotFound401Reason} {
		for _, mode := range []string{"false", "true"} {
			t.Run(reason+"/"+mode, func(t *testing.T) {
				svc, groupID := encryptedAffinitySchedulerService(t, mode, encryptedAffinityModelCooldown(reason))
				ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
				selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, int64(401), selection.Account.ID)
				require.Equal(t, openAIAccountScheduleLayerEncryptedContent, decision.Layer)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}

				// Requests without the producer's ciphertext still avoid it.
				plain, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "fresh", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, plain)
				require.Equal(t, int64(402), plain.Account.ID)
				if plain.ReleaseFunc != nil {
					plain.ReleaseFunc()
				}
			})
		}
	}
}

func TestOpenAIEncryptedContentSchedulerHonorsProducerQuotaCooldown(t *testing.T) {
	for _, reason := range []string{"", upstreamCodexPlanGatedModelReason} {
		svc, groupID := encryptedAffinitySchedulerService(t, "true", encryptedAffinityModelCooldown(reason))
		ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
		selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
		require.Nil(t, selection)
		requireEncryptedAffinityCode(t, err, "encrypted_content_account_unavailable")
	}
}

func TestOpenAIEncryptedContentSchedulerWaitsForProducerCapacity(t *testing.T) {
	svc, groupID := encryptedAffinitySchedulerService(t, "true", nil)
	svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{401: false, 402: true}})
	ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(401), selection.Account.ID)
	require.NotNil(t, selection.WaitPlan)
}

func TestOpenAIEncryptedContentSchedulerHonorsPrivacyAndPreviousResponseOwner(t *testing.T) {
	t.Run("privacy restriction", func(t *testing.T) {
		svc, groupID := encryptedAffinitySchedulerService(t, "true", nil)
		svc.schedulerSnapshot = &SchedulerSnapshotService{
			accountRepo: svc.accountRepo,
			groupRepo:   guardianAffinityGroupRepo{group: &Group{ID: groupID, RequirePrivacySet: true}},
		}
		ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
		selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
		require.Nil(t, selection)
		requireEncryptedAffinityCode(t, err, "encrypted_content_account_unavailable")
	})
	t.Run("conflicting previous response", func(t *testing.T) {
		svc, groupID := encryptedAffinitySchedulerService(t, "true", nil)
		require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(context.Background(), groupID, "resp_other", 402, time.Hour))
		ctx := WithOpenAIEncryptedContentAffinity(context.Background(), 12, encryptedAffinityInput(t, "task"))
		selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "resp_other", "child", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
		require.Nil(t, selection)
		requireEncryptedAffinityCode(t, err, "encrypted_content_account_conflict")
	})
}

func TestOpenAIEncryptedContentBindingRefreshesActiveHistory(t *testing.T) {
	cache := &encryptedAffinityTestCache{}
	svc := &OpenAIGatewayService{cache: cache}
	store, ok := svc.getOpenAIWSStateStore().(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	groupID := int64(1)
	digest := openAIEncryptedContentDigest("task")
	require.NoError(t, store.BindEncryptedContentAccount(context.Background(), groupID, 2, digest, 3, time.Hour))
	require.Equal(t, 1, cache.claims)
	mapKey := openAIWSResponseAccountMapKey(groupID, openAIEncryptedContentBindingID(2, digest))
	store.encryptedToAccountMu.Lock()
	binding := store.encryptedToAccount[mapKey]
	binding.expiresAt = time.Now().Add(20 * time.Minute)
	store.encryptedToAccount[mapKey] = binding
	store.encryptedToAccountMu.Unlock()
	require.NoError(t, svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 3, encryptedAffinityInput(t, "task")))
	require.Equal(t, 2, cache.claims)
	require.NoError(t, svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 3, encryptedAffinityInput(t, "task")))
	require.Equal(t, 2, cache.claims, "repeated active turns do not write until refresh is due")
}

func TestOpenAIEncryptedContentObserverReadsSSEEventFieldWithoutChangingJSON(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(1)
	observer := svc.NewOpenAIEncryptedContentObserver(context.Background(), &groupID, 2, 3)
	var item map[string]any
	require.NoError(t, json.Unmarshal(encryptedAffinityProducerItem(t, "event-only-task", "missing"), &item))
	delete(item, "type")
	payload, err := json.Marshal(item)
	require.NoError(t, err)
	original := append([]byte(nil), payload...)
	require.NoError(t, observer.ObserveEvent("response.output_item.done", payload))
	require.Equal(t, original, payload)
	require.NoError(t, svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 3, encryptedAffinityInput(t, "event-only-task")))
	err = svc.ValidateOpenAIEncryptedContentAccount(context.Background(), &groupID, 2, 4, encryptedAffinityInput(t, "event-only-task"))
	requireEncryptedAffinityCode(t, err, "encrypted_content_account_mismatch")
}

func TestOpenAIEncryptedContentBindingsCannotBeReachedThroughResponseIDs(t *testing.T) {
	cache := &encryptedAffinityTestCache{}
	store := NewOpenAIWSStateStore(cache)
	ctx := context.Background()
	digest := openAIEncryptedContentDigest("private-task")
	id := openAIEncryptedContentBindingID(2, digest)
	require.NoError(t, store.BindEncryptedContentAccount(ctx, 1, 2, digest, 3, time.Hour))
	owner, err := store.GetResponseAccount(ctx, 1, id)
	require.NoError(t, err)
	require.Zero(t, owner, "client-provided previous_response_id must not discover a ciphertext owner")
	require.NoError(t, store.BindResponseAccount(ctx, 1, id, 99, time.Hour))
	for _, reader := range []OpenAIWSStateStore{store, NewOpenAIWSStateStore(cache)} {
		owner, err := reader.GetEncryptedContentAccount(ctx, 1, 2, digest)
		require.NoError(t, err)
		require.Equal(t, int64(3), owner)
	}
	require.NoError(t, store.DeleteResponseAccount(ctx, 1, id))
	for _, reader := range []OpenAIWSStateStore{store, NewOpenAIWSStateStore(cache)} {
		owner, err := reader.GetEncryptedContentAccount(ctx, 1, 2, digest)
		require.NoError(t, err)
		require.Equal(t, int64(3), owner)
	}
}
