//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var antigravityProArgonQuotaTestPlans = []struct {
	name     string
	value    any
	separate bool
}{
	{name: "Pro", value: "Pro", separate: true},
	{name: "normalized Pro", value: " pro ", separate: true},
	{name: "Pro tier ID", value: "g1-pro-tier", separate: true},
	{name: "Ultra", value: "Ultra"},
	{name: "Ultra tier ID", value: "g1-ultra-tier"},
	{name: "Ultra lite tier ID", value: "g1-ultra-lite-tier"},
	{name: "free", value: "Free"},
	{name: "unknown", value: "enterprise"},
	{name: "missing"},
	{name: "malformed", value: 42},
}

var (
	antigravityProArgonQuotaTestArgonModels = []string{"gemini-4-argon-high", "gemini-4-argon-low", "gemini-4-argon-medium", "client-argon"}
	antigravityProArgonQuotaTestOtherModels = []string{"gemini-3.8-flash", "gemini-2.5-pro"}
)

func newAntigravityProArgonQuotaTestAccount(planValue any) *Account {
	return &Account{
		ID: 789, Platform: PlatformAntigravity, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"plan_type": planValue,
			"model_mapping": map[string]any{
				"gemini-4-argon-high":   "gemini-4-argon-high",
				"gemini-4-argon-low":    "gemini-4-argon-low",
				"gemini-4-argon-medium": "gemini-4-argon-medium",
				"client-argon":          "gemini-4-argon-high",
				"gemini-3.8-flash":      "gemini-3.8-flash",
				"gemini-2.5-pro":        "gemini-2.5-pro",
				"gemini-3-pro-image":    "gemini-3-pro-image",
				"claude-sonnet-4-6":     "claude-sonnet-4-6",
			},
		},
	}
}

func antigravityProArgonQuotaSnapshot(t *testing.T, account *Account, limits map[string]any) Account {
	t.Helper()
	// Read a fresh account through JSON, as the scheduler does instead of
	// using the forwarding account object.
	account.Extra = map[string]any{modelRateLimitsKey: limits}
	data, err := json.Marshal(account)
	require.NoError(t, err)
	var snapshot Account
	require.NoError(t, json.Unmarshal(data, &snapshot))
	return snapshot
}

// fork 的写入路径只冷却最终模型本身（不推断家族 scope），所以无论套餐，
// Argon 耗尽都不会波及其他 Gemini 模型，其他 Gemini 耗尽也不会波及 Argon。
func TestAntigravityModelRateLimits_ProArgonQuotaIsolation(t *testing.T) {
	pools := []struct {
		name      string
		exhausted string
		same      []string
		other     []string
	}{
		{
			name:      "Argon exhausted",
			exhausted: "gemini-4-argon-high",
			same:      []string{"gemini-4-argon-high", "client-argon"},
			other:     []string{"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-3.8-flash", "gemini-2.5-pro"},
		},
		{
			name:      "other Gemini exhausted",
			exhausted: "gemini-3.8-flash",
			same:      []string{"gemini-3.8-flash"},
			other:     []string{"gemini-2.5-pro", "gemini-4-argon-high", "gemini-4-argon-low", "gemini-4-argon-medium", "client-argon"},
		},
	}
	for _, plan := range antigravityProArgonQuotaTestPlans {
		for _, pool := range pools {
			t.Run(plan.name+"/"+pool.name, func(t *testing.T) {
				account := newAntigravityProArgonQuotaTestAccount(plan.value)
				repo := &stubAntigravityAccountRepo{}
				svc := &AntigravityGatewayService{}
				resetAt := time.Now().Add(time.Hour)
				require.True(t, svc.setAntigravityModelRateLimits(context.Background(), repo, account,
					pool.exhausted, "[test]", http.StatusTooManyRequests, resetAt, false))

				limits := make(map[string]any)
				for _, call := range repo.modelRateLimitCalls {
					require.Equal(t, account.ID, call.accountID)
					limits[call.modelKey] = map[string]any{"rate_limit_reset_at": call.resetAt.Format(time.RFC3339)}
				}
				require.Equal(t, []string{pool.exhausted}, func() []string {
					keys := make([]string, 0, len(limits))
					for key := range limits {
						keys = append(keys, key)
					}
					return keys
				}(), "only the exhausted model is cooled")
				snapshot := antigravityProArgonQuotaSnapshot(t, account, limits)

				for _, model := range pool.same {
					require.False(t, snapshot.IsSchedulableForModel(model), "exhausted model: %s", model)
					require.Positive(t, snapshot.GetRateLimitRemainingTime(model), "exhausted model: %s", model)
				}
				for _, model := range pool.other {
					require.True(t, snapshot.IsSchedulableForModel(model), "other model: %s", model)
					require.Zero(t, snapshot.GetRateLimitRemainingTime(model), "other model: %s", model)
				}
				for _, model := range []string{"gemini-3-pro-image", "claude-sonnet-4-6"} {
					require.True(t, snapshot.IsSchedulableForModel(model), "unrelated model: %s", model)
					require.Zero(t, snapshot.GetRateLimitRemainingTime(model))
				}
			})
		}
	}
}

// 显式家族 scope 仍可读取：Pro 的 Argon 变体读 antigravity:gemini:argon（独立配额池），
// 不受共享 antigravity:gemini 影响；Ultra 与未知套餐的 Argon 仍读共享 scope。
func TestAntigravityModelRateLimits_ProArgonExplicitFamilyScopes(t *testing.T) {
	for _, plan := range antigravityProArgonQuotaTestPlans {
		for _, scope := range []string{antigravityArgonModelRateLimitKey, antigravityGeminiModelRateLimitKey} {
			t.Run(plan.name+"/"+scope, func(t *testing.T) {
				account := newAntigravityProArgonQuotaTestAccount(plan.value)
				snapshot := antigravityProArgonQuotaSnapshot(t, account, map[string]any{
					scope: map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
				})

				argonBlocked := plan.separate == (scope == antigravityArgonModelRateLimitKey)
				otherBlocked := scope == antigravityGeminiModelRateLimitKey
				for _, model := range antigravityProArgonQuotaTestArgonModels {
					require.Equal(t, !argonBlocked, snapshot.IsSchedulableForModel(model), "argon model: %s", model)
					require.Equal(t, argonBlocked, snapshot.GetRateLimitRemainingTime(model) > 0, "argon model: %s", model)
				}
				for _, model := range antigravityProArgonQuotaTestOtherModels {
					require.Equal(t, !otherBlocked, snapshot.IsSchedulableForModel(model), "other model: %s", model)
					require.Equal(t, otherBlocked, snapshot.GetRateLimitRemainingTime(model) > 0, "other model: %s", model)
				}
				for _, model := range []string{"gemini-3-pro-image", "claude-sonnet-4-6"} {
					require.True(t, snapshot.IsSchedulableForModel(model), "unrelated model: %s", model)
					require.Zero(t, snapshot.GetRateLimitRemainingTime(model))
				}
			})
		}
	}
}
