package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/stretchr/testify/require"
)

type historyWorkerRepo struct {
	AccountRepository
	GeminiWebHistoryCleanupRepository
	account                                        *Account
	mu                                             sync.Mutex
	renews, failRenewAt, activeChecks, activeUntil int
	status, code                                   string
	snapshot                                       []string
	clock                                          *time.Time
}

func (r *historyWorkerRepo) GetByID(context.Context, int64) (*Account, error) {
	a := *r.account
	a.Credentials = shallowCopyMap(a.Credentials)
	return &a, nil
}
func (r *historyWorkerRepo) RenewGeminiWebHistoryCleanup(context.Context, *GeminiWebHistoryCleanupJob, time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renews++
	if r.failRenewAt > 0 && r.renews >= r.failRenewAt {
		return ErrGeminiWebHistoryLeaseLost
	}
	return nil
}
func (r *historyWorkerRepo) GeminiWebHistoryHasActiveRequests(context.Context, *GeminiWebHistoryCleanupJob) (bool, error) {
	r.activeChecks++
	if r.clock != nil && r.activeChecks <= r.activeUntil {
		*r.clock = r.clock.Add(2 * time.Minute)
	}
	return r.activeChecks <= r.activeUntil, nil
}
func (r *historyWorkerRepo) SaveGeminiWebHistorySnapshot(_ context.Context, j *GeminiWebHistoryCleanupJob, ids []string) error {
	r.snapshot = append([]string{}, ids...)
	j.Snapshot = append([]string{}, ids...)
	j.SnapshotReady = true
	return nil
}
func (r *historyWorkerRepo) FinishGeminiWebHistoryCleanup(context.Context, *GeminiWebHistoryCleanupJob) error {
	r.status = "completed"
	return nil
}
func (r *historyWorkerRepo) CancelGeminiWebHistoryCleanup(_ context.Context, _ *GeminiWebHistoryCleanupJob, code string) error {
	r.status = "cancelled"
	r.code = code
	return nil
}
func (r *historyWorkerRepo) RetryGeminiWebHistoryCleanup(_ context.Context, _ *GeminiWebHistoryCleanupJob, code string, _ time.Time) error {
	r.status = "pending"
	r.code = code
	return nil
}

type historyWorkerClient struct {
	ids, deleted       []string
	listCalls          int
	listErr, deleteErr error
	keepDeleted        bool
	newAfterDelete     bool
	blockList          bool
}

func (c *historyWorkerClient) CurrentCookie(geminiweb.Credential) string { return "" }
func (c *historyWorkerClient) ListConversations(ctx context.Context, _ geminiweb.Credential, _ geminiweb.Doer) (geminiweb.HistoryResult, error) {
	c.listCalls++
	if c.blockList {
		<-ctx.Done()
		return geminiweb.HistoryResult{}, ctx.Err()
	}
	return geminiweb.HistoryResult{ConversationIDs: append([]string{}, c.ids...)}, c.listErr
}
func (c *historyWorkerClient) DeleteConversation(_ context.Context, _ geminiweb.Credential, id string, _ geminiweb.Doer) (string, error) {
	c.deleted = append(c.deleted, id)
	if c.deleteErr != nil {
		return "", c.deleteErr
	}
	if !c.keepDeleted {
		for i, v := range c.ids {
			if v == id {
				c.ids = append(c.ids[:i], c.ids[i+1:]...)
				break
			}
		}
	}
	if c.newAfterDelete {
		c.ids = append(c.ids, "c_new")
		c.newAfterDelete = false
	}
	return "", nil
}

func newHistoryWorkerTest(t *testing.T) (*GeminiWebHistoryCleanupService, *historyWorkerRepo, *historyWorkerClient, *GeminiWebHistoryCleanupJob, *time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := &historyWorkerRepo{account: &Account{ID: 42, Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=test"}}}
	c := &historyWorkerClient{ids: []string{"c_old", "c_manual"}}
	s := NewGeminiWebHistoryCleanupService(r, &GeminiMessagesCompatService{})
	s.client = c
	s.now = func() time.Time { return now }
	s.heartbeatInterval = time.Hour
	s.drainInterval = time.Nanosecond
	s.cookiePersist = nil
	w := GeminiWebCleanupWindow{ID: "five_hour", ResetAt: now.Add(time.Hour)}
	s.quotaRead = func(context.Context, *Account) (geminiweb.Quota, error) {
		return geminiweb.Quota{ObservedAt: now, Windows: []geminiweb.QuotaWindow{{ID: w.ID, HasPercent: true, UsedPercent: 100, RemainingPercent: 0, HasReset: true, ResetAt: w.ResetAt}}}, nil
	}
	j := &GeminiWebHistoryCleanupJob{ID: 1, AccountID: 42, Identity: geminiWebHistoryIdentity(r.account), Windows: []GeminiWebCleanupWindow{w}, LeaseToken: "lease", Attempts: 1}
	t.Cleanup(s.Stop)
	return s, r, c, j, &now
}

func TestGeminiWebHistoryWorkerDeletesSnapshotIncludingManualChats(t *testing.T) {
	s, r, c, j, _ := newHistoryWorkerTest(t)
	c.newAfterDelete = true
	s.runJob(context.Background(), j)
	require.Equal(t, "completed", r.status)
	require.Equal(t, []string{"c_old", "c_manual"}, r.snapshot)
	require.Equal(t, r.snapshot, c.deleted)
	require.Equal(t, []string{"c_new"}, c.ids)
	require.Equal(t, 2, c.listCalls)
}
func TestGeminiWebHistoryWorkerRetryNeverBroadensSnapshot(t *testing.T) {
	s, r, c, j, _ := newHistoryWorkerTest(t)
	j.SnapshotReady = true
	j.Snapshot = []string{"c_old", "c_already_gone"}
	c.ids = []string{"c_old", "c_new"}
	s.runJob(context.Background(), j)
	require.Equal(t, "completed", r.status)
	require.Nil(t, r.snapshot)
	require.Equal(t, []string{"c_old"}, c.deleted)
	require.Equal(t, []string{"c_new"}, c.ids)
}
func TestGeminiWebHistoryWorkerEmptySnapshotDoesNotRecapture(t *testing.T) {
	s, r, c, j, _ := newHistoryWorkerTest(t)
	j.SnapshotReady = true
	j.Snapshot = []string{}
	s.runJob(context.Background(), j)
	require.Equal(t, "completed", r.status)
	require.Empty(t, c.deleted)
	require.Nil(t, r.snapshot)
}
func TestGeminiWebHistoryWorkerUncertainQuotaNeverDeletes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		unknown bool
	}{
		{"1095", &geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1095}, false},
		{"503", errors.New("upstream 503"), false},
		{"rate limited", geminiweb.ErrRateLimited, false},
		{"missing metadata", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, c, j, _ := newHistoryWorkerTest(t)
			s.quotaRead = func(context.Context, *Account) (geminiweb.Quota, error) { return geminiweb.Quota{}, tc.err }
			s.runJob(context.Background(), j)
			require.Equal(t, "pending", r.status)
			require.Equal(t, "quota_unavailable", r.code)
			require.Empty(t, c.deleted)
			require.Zero(t, c.listCalls)
		})
	}
}
func TestGeminiWebHistoryWorkerCancelsRecoveredOrChangedAccount(t *testing.T) {
	for _, tc := range []string{"recovered", "identity", "disabled", "expired window"} {
		t.Run(tc, func(t *testing.T) {
			s, r, c, j, now := newHistoryWorkerTest(t)
			switch tc {
			case "identity":
				r.account.Credentials["gemini_web_cookie"] = "__Secure-1PSID=another"
			case "disabled":
				r.account.Schedulable = false
			case "expired window":
				*now = now.Add(2 * time.Hour)
			case "recovered":
				s.quotaRead = func(context.Context, *Account) (geminiweb.Quota, error) {
					return geminiweb.Quota{ObservedAt: *now, Windows: []geminiweb.QuotaWindow{{ID: "five_hour", HasPercent: true, UsedPercent: 20, RemainingPercent: 80}}}, nil
				}
			}
			s.runJob(context.Background(), j)
			require.Equal(t, "cancelled", r.status)
			require.Empty(t, c.deleted)
			require.Zero(t, c.listCalls)
		})
	}
}

func TestGeminiWebHistoryWorkerIncompleteSecondWindowKeepsAuthorizationPending(t *testing.T) {
	s, r, c, j, now := newHistoryWorkerTest(t)
	j.Windows = append(j.Windows, GeminiWebCleanupWindow{ID: "weekly", ResetAt: now.Add(24 * time.Hour)})
	s.quotaRead = func(context.Context, *Account) (geminiweb.Quota, error) {
		return geminiweb.Quota{ObservedAt: *now, Windows: []geminiweb.QuotaWindow{{ID: "five_hour", HasPercent: true, UsedPercent: 20, RemainingPercent: 80}}}, nil
	}
	s.runJob(context.Background(), j)
	require.Equal(t, "pending", r.status)
	require.Equal(t, "quota_unavailable", r.code)
	require.Empty(t, c.deleted)
	require.Zero(t, c.listCalls)
}
func TestGeminiWebHistoryWorkerWaitsForExistingSixMinuteRequestLease(t *testing.T) {
	s, r, c, j, now := newHistoryWorkerTest(t)
	r.activeUntil = 3
	r.clock = now
	before := *now
	read := s.quotaRead
	s.quotaRead = func(ctx context.Context, a *Account) (geminiweb.Quota, error) {
		require.GreaterOrEqual(t, now.Sub(before), 6*time.Minute)
		require.Equal(t, 4, r.activeChecks)
		return read(ctx, a)
	}
	s.runJob(context.Background(), j)
	require.Equal(t, "completed", r.status)
	require.Len(t, c.deleted, 2)
}
func TestGeminiWebHistoryWorkerRequiresLeaseBeforeDelete(t *testing.T) {
	s, r, c, j, _ := newHistoryWorkerTest(t)
	r.failRenewAt = 3
	s.runJob(context.Background(), j)
	require.Empty(t, r.status)
	require.Empty(t, c.deleted)
	require.True(t, j.SnapshotReady)
}
func TestGeminiWebHistoryWorkerHeartbeatCancelsInflightList(t *testing.T) {
	s, r, c, j, _ := newHistoryWorkerTest(t)
	r.failRenewAt = 2
	c.blockList = true
	s.heartbeatInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.runJob(ctx, j)
	require.NoError(t, ctx.Err())
	require.Empty(t, r.status)
	require.Empty(t, c.deleted)
}
func TestGeminiWebHistoryWorkerRetriesListDeleteAndVerificationFailures(t *testing.T) {
	for _, tc := range []string{"list", "delete", "verify"} {
		t.Run(tc, func(t *testing.T) {
			s, r, c, j, _ := newHistoryWorkerTest(t)
			switch tc {
			case "list":
				c.listErr = errors.New("malformed list")
			case "delete":
				c.deleteErr = context.DeadlineExceeded
			case "verify":
				c.keepDeleted = true
			}
			s.runJob(context.Background(), j)
			require.Equal(t, "pending", r.status)
			if tc == "list" {
				require.Empty(t, c.deleted)
				require.False(t, j.SnapshotReady)
			} else {
				require.Equal(t, []string{"c_old", "c_manual"}, j.Snapshot)
				require.True(t, j.SnapshotReady)
			}
		})
	}
}
