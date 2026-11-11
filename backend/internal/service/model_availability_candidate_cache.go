package service

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"golang.org/x/sync/singleflight"
)

// modelAvailabilityCandidateLoadTimeout 限制 singleflight 共享查询的最长时间。
// 共享查询脱离发起者的取消信号，避免发起者断开时连带让等待者回退。
const modelAvailabilityCandidateLoadTimeout = 5 * time.Second

// resolveModelAvailabilityDiagnosisCacheTTL 返回诊断候选账号缓存的 TTL；0 表示关闭缓存。
// 默认值（1500ms）由配置加载时的 viper 默认值提供；没有配置对象时关闭缓存。
func resolveModelAvailabilityDiagnosisCacheTTL(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.Gateway.ModelAvailabilityDiagnosisCacheTTLMillis <= 0 {
		return 0
	}
	return time.Duration(cfg.Gateway.ModelAvailabilityDiagnosisCacheTTLMillis) * time.Millisecond
}

// modelAvailabilityCandidateCache 为选号失败时的模型可用性诊断缓存候选账号列表。
//
// 诊断在每个最终返回 404/429/503 的请求上运行，原先每次都对分组发一次全量仓储查询，
// 429/503 风暴时客户端越重试、查询越多。这里按查询范围（分组、平台、是否含已分组账号）
// 短 TTL 缓存仓储结果，并发未命中经 singleflight 合并为一次查询。
//
// 缓存的是仓储返回的账号母本，发布前调用 PrepareSharedSnapshot，发布后只读；
// 每次读取都复制出调用方独占的账号副本，诊断的懒缓存只写副本。诊断本身用调用时的
// 当前时间重新计算，已过期的限流时间点会被自然过滤。查询出错不写缓存。
//
// 零值可用。
type modelAvailabilityCandidateCache struct {
	mu      sync.Mutex
	entries map[string]modelAvailabilityCandidateEntry
	flight  singleflight.Group
}

type modelAvailabilityCandidateEntry struct {
	accounts  []Account
	expiresAt time.Time
}

func modelAvailabilityCandidateCacheKey(groupID *int64, platforms []string, includeGrouped bool) string {
	sorted := slices.Clone(platforms)
	slices.Sort(sorted)
	var b strings.Builder
	if groupID == nil {
		b.WriteString("all")
	} else {
		b.WriteString(strconv.FormatInt(*groupID, 10))
	}
	b.WriteByte('|')
	b.WriteString(strings.Join(sorted, ","))
	b.WriteByte('|')
	b.WriteString(strconv.FormatBool(includeGrouped))
	return b.String()
}

// list 返回候选账号的独占副本。ttl<=0 时直接查询仓储。
func (c *modelAvailabilityCandidateCache) list(
	ctx context.Context,
	repo AccountRepository,
	ttl time.Duration,
	groupID *int64,
	platforms []string,
	includeGrouped bool,
) ([]Account, error) {
	if c == nil || ttl <= 0 {
		return repo.ListModelAvailabilityCandidates(ctx, groupID, platforms, includeGrouped)
	}
	key := modelAvailabilityCandidateCacheKey(groupID, platforms, includeGrouped)
	if masters, ok := c.lookup(key, time.Now()); ok {
		return cloneModelAvailabilityCandidates(masters), nil
	}

	ch := c.flight.DoChan(key, func() (any, error) {
		// 前一个共享查询可能刚写入缓存。
		if masters, ok := c.lookup(key, time.Now()); ok {
			return masters, nil
		}
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelAvailabilityCandidateLoadTimeout)
		defer cancel()
		accounts, err := repo.ListModelAvailabilityCandidates(loadCtx, groupID, platforms, includeGrouped)
		if err != nil {
			return nil, err
		}
		for i := range accounts {
			accounts[i].PrepareSharedSnapshot()
		}
		c.store(key, accounts, time.Now().Add(ttl))
		return accounts, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		masters, _ := res.Val.([]Account)
		return cloneModelAvailabilityCandidates(masters), nil
	}
}

func (c *modelAvailabilityCandidateCache) lookup(key string, now time.Time) ([]Account, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return nil, false
	}
	return entry.accounts, true
}

func (c *modelAvailabilityCandidateCache) store(key string, accounts []Account, expiresAt time.Time) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]modelAvailabilityCandidateEntry)
	}
	// 键数量只随分组×平台组合增长，写入时顺手清掉过期条目即可。
	for k, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = modelAvailabilityCandidateEntry{accounts: accounts, expiresAt: expiresAt}
}

func cloneModelAvailabilityCandidates(masters []Account) []Account {
	if masters == nil {
		return nil
	}
	out := make([]Account, len(masters))
	copy(out, masters)
	return out
}
