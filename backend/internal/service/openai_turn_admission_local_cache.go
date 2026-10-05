package service

import (
	"reflect"
	"slices"
	"sync"
	"time"
)

type openAITurnAdmissionLocalCacheEntry struct {
	account *Account
	parent  *Account
	stored  time.Time
}

type openAITurnAdmissionLocalCache struct {
	mu      sync.RWMutex
	entries map[int64]openAITurnAdmissionLocalCacheEntry
}

func (c *openAITurnAdmissionLocalCache) load(accountID int64, ttl time.Duration) (*Account, *Account, bool) {
	if accountID <= 0 || ttl <= 0 {
		return nil, nil, false
	}
	c.mu.RLock()
	entry, ok := c.entries[accountID]
	c.mu.RUnlock()
	if !ok || time.Since(entry.stored) >= ttl {
		return nil, nil, false
	}
	return cloneOpenAITurnAdmissionAccount(entry.account), cloneOpenAITurnAdmissionAccount(entry.parent), true
}

func (c *openAITurnAdmissionLocalCache) store(account, parent *Account) {
	if account == nil || account.ID <= 0 {
		return
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[int64]openAITurnAdmissionLocalCacheEntry)
	}
	c.entries[account.ID] = openAITurnAdmissionLocalCacheEntry{
		account: cloneOpenAITurnAdmissionAccount(account),
		parent:  cloneOpenAITurnAdmissionAccount(parent),
		stored:  time.Now(),
	}
	c.mu.Unlock()
}

func (s *OpenAIGatewayService) openAITurnAdmissionCacheTTL() time.Duration {
	if s == nil || s.cfg == nil {
		return 0
	}
	return time.Duration(s.cfg.Gateway.Scheduling.OpenAITurnAdmissionCacheTTLSeconds) * time.Second
}

func cloneOpenAITurnAdmissionAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	clone := *account
	clone.Credentials = cloneOpenAITurnAdmissionMap(account.Credentials)
	clone.Extra = cloneOpenAITurnAdmissionMap(account.Extra)
	clone.GroupIDs = slices.Clone(account.GroupIDs)
	clone.Groups = nil
	clone.AccountGroups = make([]AccountGroup, len(account.AccountGroups))
	for i := range account.AccountGroups {
		clone.AccountGroups[i] = account.AccountGroups[i]
		clone.AccountGroups[i].AllowedModels = slices.Clone(account.AccountGroups[i].AllowedModels)
		clone.AccountGroups[i].Group = nil
	}
	if account.Proxy != nil {
		proxy := *account.Proxy
		clone.Proxy = &proxy
	}
	clone.modelMappingCache = nil
	clone.modelMappingCacheReady = false
	clone.headerOverrideCache = nil
	clone.headerOverrideCacheReady = false
	return &clone
}

// Admission snapshots contain JSON-like credential/extra values. Clone maps and slices
// recursively so a request-side mutation cannot change a later cache hit (or race with it).
func cloneOpenAITurnAdmissionMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	value := cloneOpenAITurnAdmissionValue(reflect.ValueOf(input), 0)
	if !value.IsValid() {
		return nil
	}
	return value.Interface().(map[string]any)
}

func cloneOpenAITurnAdmissionValue(value reflect.Value, depth int) reflect.Value {
	if !value.IsValid() || depth > 32 {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneOpenAITurnAdmissionValue(value.Elem(), depth+1)
		result := reflect.New(value.Type()).Elem()
		if cloned.IsValid() && cloned.Type().AssignableTo(value.Type()) {
			result.Set(cloned)
		} else if cloned.IsValid() && cloned.Type().Implements(value.Type()) {
			result.Set(cloned)
		} else {
			result.Set(value)
		}
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			item := cloneOpenAITurnAdmissionValue(iter.Value(), depth+1)
			if !item.IsValid() || !item.Type().AssignableTo(value.Type().Elem()) {
				item = iter.Value()
			}
			result.SetMapIndex(iter.Key(), item)
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			item := cloneOpenAITurnAdmissionValue(value.Index(index), depth+1)
			if item.IsValid() && item.Type().AssignableTo(value.Type().Elem()) {
				result.Index(index).Set(item)
			} else {
				result.Index(index).Set(value.Index(index))
			}
		}
		return result
	case reflect.Ptr:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		item := cloneOpenAITurnAdmissionValue(value.Elem(), depth+1)
		if item.IsValid() && item.Type().AssignableTo(value.Type().Elem()) {
			result.Elem().Set(item)
		} else {
			result.Elem().Set(value.Elem())
		}
		return result
	default:
		return value
	}
}
