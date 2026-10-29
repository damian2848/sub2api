package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const geminiWebQuotaRPC = "jSf9Qc"

type Quota struct {
	Plan       string
	PlanCode   string // raw upstream plan enum, kept so unmapped tiers can be identified
	AICredits  *float64
	Windows    []QuotaWindow
	ObservedAt time.Time
}
type QuotaWindow struct {
	ID               string
	Label            string
	UsedPercent      float64
	RemainingPercent float64
	HasPercent       bool
	ResetAt          time.Time
	HasReset         bool
}
type QuotaResult struct {
	Quota         Quota
	UpdatedCookie string
}
type quotaCacheEntry struct {
	result   QuotaResult
	observed time.Time
}

// FetchQuota reads the account-wide usage windows exposed by Gemini Web's read-only usage RPC.
func (c *Client) FetchQuota(ctx context.Context, cred Credential, do Doer) (QuotaResult, error) {
	return c.fetchQuota(ctx, cred, do, false)
}

// RefreshQuota bypasses the usage cache while preserving the browser session
// and its latest rotated cookies.
func (c *Client) RefreshQuota(ctx context.Context, cred Credential, do Doer) (QuotaResult, error) {
	return c.fetchQuota(ctx, cred, do, true)
}

func (c *Client) fetchQuota(ctx context.Context, cred Credential, do Doer, force bool) (QuotaResult, error) {
	cred.Cookie = normalizeCookieHeader(cred.Cookie)
	if strings.TrimSpace(cred.Cookie) == "" || cookieKey(cred.Cookie) == "" {
		return QuotaResult{}, ErrInvalidCredential
	}
	if do == nil {
		return QuotaResult{}, errors.New("gemini web transport is nil")
	}
	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if base == "" {
		base = "https://gemini.google.com"
	}
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration)
	c.mu.Lock()
	if c.quotaCache == nil {
		c.quotaCache = make(map[string]quotaCacheEntry)
	}
	if cached, ok := c.quotaCache[key]; !force && ok && time.Since(cached.observed) < c.quotaTTL {
		c.mu.Unlock()
		return cached.result, nil
	}
	c.mu.Unlock()
	sess, sessionCookie, err := c.getSession(ctx, cred, base, do)
	if err != nil {
		return QuotaResult{UpdatedCookie: sessionCookie}, err
	}
	postCred := cred
	if sessionCookie != "" {
		postCred.Cookie = sessionCookie
	}
	accountPath := ""
	if cred.hasAuthUser() {
		accountPath = "/u/" + strconv.Itoa(cred.AuthUser)
	}
	query := url.Values{"rpcids": {geminiWebQuotaRPC}, "source-path": {accountPath + "/usage"}, "hl": {defaultString(sess.Language, "en")}, "_reqid": {strconv.FormatInt(time.Now().UnixNano()%1000000, 10)}, "rt": {"c"}, "bl": {sess.Build}, "f.sid": {sess.Fsid}}
	framed, _ := json.Marshal([]any{[]any{[]any{geminiWebQuotaRPC, "[]", nil, "generic"}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+accountPath+"/_/BardChatUi/data/batchexecute?"+query.Encode(), strings.NewReader("at="+url.QueryEscape(sess.At)+"&f.req="+url.QueryEscape(string(framed))))
	if err != nil {
		return QuotaResult{UpdatedCookie: sessionCookie}, err
	}
	applyHeaders(req, postCred, base, sess, Model{}, uuid())
	req.Header.Set("Referer", base+accountPath+"/usage")
	resp, err := do(req)
	if err != nil {
		return QuotaResult{UpdatedCookie: sessionCookie}, err
	}
	if resp == nil || resp.Body == nil {
		return QuotaResult{UpdatedCookie: sessionCookie}, errors.New("gemini web quota returned no response body")
	}
	defer func() { _ = resp.Body.Close() }()
	updatedCookie := mergeSetCookie(postCred.Cookie, resp.Header.Values("Set-Cookie"))
	c.updateSessionCookie(postCred, updatedCookie)
	if live := c.currentSessionCookie(postCred); live != "" {
		updatedCookie = live
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return QuotaResult{UpdatedCookie: updatedCookie}, ErrInvalidCredential
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return QuotaResult{UpdatedCookie: updatedCookie}, ErrRateLimited
	}
	if resp.StatusCode >= 400 {
		return QuotaResult{UpdatedCookie: updatedCookie}, fmt.Errorf("gemini web quota upstream: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return QuotaResult{UpdatedCookie: updatedCookie}, err
	}
	quota, err := decodeQuota(body, time.Now())
	if err != nil {
		return QuotaResult{UpdatedCookie: updatedCookie}, err
	}
	result := QuotaResult{Quota: quota, UpdatedCookie: updatedCookie}
	c.mu.Lock()
	c.quotaCache[key] = quotaCacheEntry{result: result, observed: time.Now()}
	c.mu.Unlock()
	return result, nil
}
func decodeQuota(body []byte, observed time.Time) (Quota, error) {
	payload, err := decodeBatchRPC(body, geminiWebQuotaRPC)
	if err != nil {
		return Quota{}, err
	}
	var fields []any
	d := json.NewDecoder(strings.NewReader(string(payload)))
	d.UseNumber()
	if d.Decode(&fields) == nil {
		if q, ok := parseQuotaPayload(fields, observed); ok {
			return q, nil
		}
	}
	return Quota{}, errors.New("invalid or unavailable Gemini Web usage data")
}
func parseQuotaPayload(fields []any, observed time.Time) (Quota, bool) {
	bv, ok := arrayValue(fields, 1)
	if !ok {
		return Quota{}, false
	}
	buckets, ok := bv.([]any)
	if !ok {
		return Quota{}, false
	}
	q := Quota{ObservedAt: observed, Windows: make([]QuotaWindow, 0, 2)}
	if v, ok := arrayValue(fields, 0); ok {
		if plan, valid := integerValue(v); valid {
			q.PlanCode = strconv.Itoa(plan)
			switch plan {
			case 1:
				// Inferred: the free tier is the only consumer plan below Pro (2).
				q.Plan = "FREE"
			case 2:
				q.Plan = "PRO"
			case 3, 6:
				q.Plan = "ULTRA"
			case 4:
				q.Plan = "PLUS"
			}
		}
	}
	seen := map[int]bool{}
	for _, raw := range buckets {
		bucket, ok := raw.([]any)
		if !ok {
			continue
		}
		kv, _ := arrayValue(bucket, 2)
		kind, _ := integerValue(kv)
		if seen[kind] {
			continue
		}
		seen[kind] = true
		switch kind {
		case 1, 2:
			w := QuotaWindow{}
			if kind == 1 {
				w.ID, w.Label = "five_hour", "Current usage"
			} else {
				w.ID, w.Label = "weekly", "Weekly limit"
			}
			fv, _ := arrayValue(bucket, 1)
			if fraction, valid := numberValue(fv); valid && fraction >= 0 && fraction <= 1 {
				w.UsedPercent = fraction * 100
				w.RemainingPercent = math.Max(0, 100-w.UsedPercent)
				w.HasPercent = true
			}
			if ts, valid := bucketResetValue(bucket, observed); valid {
				w.ResetAt, w.HasReset = ts, true
			}
			q.Windows = append(q.Windows, w)
		case 3:
			cv, _ := arrayValue(bucket, 0)
			if credits, valid := numberValue(cv); valid && credits >= 0 {
				q.AICredits = &credits
			}
		}
	}
	return q, true
}

// bucketResetValue reads a window's reset, normally a [seconds, nanos]
// timestamp at index 3. Later fields are also checked, accepting only a time
// inside the next week, so a shifted field does not hide the countdown.
func bucketResetValue(bucket []any, observed time.Time) (time.Time, bool) {
	for i := 3; i < len(bucket); i++ {
		ts, ok := resetValue(bucket[i])
		if !ok {
			continue
		}
		if i == 3 || (ts.After(observed.Add(-time.Hour)) && ts.Before(observed.Add(8*24*time.Hour))) {
			return ts, true
		}
	}
	return time.Time{}, false
}
func resetValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case []any:
		if ts, ok := timestampValue(v); ok {
			return ts, true
		}
		if len(v) == 1 {
			if inner, ok := v[0].([]any); ok {
				return timestampValue(inner)
			}
		}
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}
func arrayValue(values []any, index int) (any, bool) {
	if index < 0 || index >= len(values) {
		return nil, false
	}
	return values[index], true
}
func integerValue(value any) (int, bool) {
	n, ok := numberValue(value)
	return int(n), ok && math.Trunc(n) == n
}
func numberValue(value any) (float64, bool) {
	var text string
	switch v := value.(type) {
	case json.Number:
		text = string(v)
	case string:
		text = v
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
	n, err := strconv.ParseFloat(text, 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func timestampValue(value []any) (time.Time, bool) {
	sv, _ := arrayValue(value, 0)
	seconds, ok := numberValue(sv)
	if !ok || seconds <= 0 || seconds > 253402300799 || math.Trunc(seconds) != seconds {
		return time.Time{}, false
	}
	nanos := float64(0)
	if nv, exists := arrayValue(value, 1); exists && nv != nil {
		var valid bool
		nanos, valid = numberValue(nv)
		if !valid || nanos < 0 || nanos >= 1e9 || math.Trunc(nanos) != nanos {
			return time.Time{}, false
		}
	}
	return time.Unix(int64(seconds), int64(nanos)).UTC(), true
}
