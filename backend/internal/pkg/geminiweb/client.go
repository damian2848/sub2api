// Package geminiweb is a small, independently implemented client for the
// browser-facing Gemini endpoint.  It intentionally has no dependency on the
// external geminiweb2api project or on Sub2API's service layer.
package geminiweb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidCredential = errors.New("gemini web credential is invalid")
	ErrRateLimited       = errors.New("gemini web is rate limited")
	ErrEmptyResponse     = errors.New("gemini web returned no answer")
	// ErrCookieRotationUnavailable means Google accepted the rotation request,
	// but did not issue a new timestamp cookie.  This is a transient condition;
	// callers may continue using a still-valid session and retry later.
	ErrCookieRotationUnavailable = errors.New("gemini web cookie rotation returned no new timestamp cookie")
)

type Credential struct {
	Cookie string
	// CacheGeneration isolates an externally recovered session, even when PSID is unchanged.
	CacheGeneration string
	AuthUser        int
	AuthUserSet     bool
	BaseURL         string
	UserAgent       string
}

type Request struct {
	Model    string
	Prompt   string
	Metadata []any
	Thinking bool
	// Attachments are uploaded before the prompt is sent and attached to it,
	// as files picked with the browser's upload button.
	Attachments []Attachment
	// OnResult receives cumulative snapshots while the browser response is
	// being decoded. It is used by the native streaming adapter to forward
	// Gemini Web frames as soon as they arrive. A nil callback preserves the
	// original buffered behavior.
	OnResult func(Result) error

	// files is the uploaded attachment list, filled in once by Complete so a
	// stale-session retry does not upload again.
	files []any
}

type Result struct {
	Text     string
	Thinking string
	// ResponseModel is declared by the upstream protocol, independently of the
	// requested model. Empty means Google did not identify the model used.
	ResponseModel         string
	ResponseModelConflict bool
	// GenerationFailed is set when Google flags the reply as a failed
	// generation (payload[31] == true). Gemini Web then returns a canned
	// failure message, such as "I seem to be encountering an error...", as
	// answer text with HTTP 200; real answers and policy refusals do not set
	// the flag.
	GenerationFailed bool
	Metadata         []any
	CID              string
	RID              string
	RequestID        string
	UpdatedCookie    string
	// Timing is filled in by Complete, on success and on error, so callers can
	// tell where a slow or failed request spent its time.
	Timing Timing
	// Raw is the StreamGenerate response body as received, kept so callers can
	// save suspicious replies for analysis. It holds no request data.
	Raw []byte
}

// Timing records the stages of one Complete call. Each duration is measured
// from the start of Complete; zero means the stage was never reached.
type Timing struct {
	PromptChars int
	Attachments int
	// Status is the HTTP status of the StreamGenerate response, 0 if none.
	Status int
	// Upload is when the attachments finished uploading.
	Upload        time.Duration
	Session       time.Duration
	Headers       time.Duration
	FirstFrame    time.Duration
	FirstThinking time.Duration
	FirstText     time.Duration
	Total         time.Duration
}

type stageClock struct {
	start  time.Time
	timing *Timing
}

// mark sets *stage to the time elapsed so far. Unless overwrite is set it keeps
// an earlier mark, so first-arrival stages survive later frames.
func (c *stageClock) mark(stage *time.Duration, overwrite bool) {
	if *stage != 0 && !overwrite {
		return
	}
	// Never record zero: zero means "not reached".
	*stage = max(time.Since(c.start), time.Nanosecond)
}

func (c *stageClock) observe(r Result) {
	c.mark(&c.timing.FirstFrame, false)
	if r.Thinking != "" {
		c.mark(&c.timing.FirstThinking, false)
	}
	if r.Text != "" {
		c.mark(&c.timing.FirstText, false)
	}
}

type Doer func(*http.Request) (*http.Response, error)

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36"

type session struct {
	At, Build, Fsid, Language string
	// Cookie is the latest merged cookie header observed by this session. It
	// prevents an older cached Cookie from being sent after Google rotates the
	// timestamp cookie in a response.
	Cookie string
	// CookieRefreshed records the last successful RotateCookies call (or the
	// bootstrap that created the session). Gemini Web cookies are kept alive at
	// most once per ten minutes, matching cliproxyapi's browser-cookie worker.
	CookieRefreshed time.Time
	Fetched         time.Time
	// ModelIDs are the Web model IDs the startup page's model picker lists.
	ModelIDs []string
}

type Client struct {
	mu          sync.Mutex
	sessions    map[string]session
	liveCookies map[string]string
	metadata    map[string][]any
	quotaCache  map[string]quotaCacheEntry
	ttl         time.Duration
	quotaTTL    time.Duration
}

const geminiWebCookieRefreshInterval = 10 * time.Minute

// cookieRefreshLocks is shared by all clients in this process. The keepalive
// worker and a request-triggered lazy refresh can otherwise race the same
// account and persist an older Set-Cookie value after the newer rotation.
var cookieRefreshLocks sync.Map // map[string]*sync.Mutex

func cookieRefreshLock(key string) *sync.Mutex {
	actual, _ := cookieRefreshLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		panic("gemini web cookie refresh lock has unexpected type")
	}
	return lock
}

func (cred Credential) hasAuthUser() bool {
	return cred.AuthUserSet || cred.AuthUser > 0
}

func credentialKey(cookie string, authUser int, authUserSet bool, generation ...string) string {
	key := cookieKey(cookie) + "\x00" + strconv.Itoa(authUser) + "\x00" + strconv.FormatBool(authUserSet)
	if len(generation) > 0 && generation[0] != "" {
		key += "\x00" + generation[0]
	}
	return key
}

func NewClient() *Client {
	return &Client{sessions: make(map[string]session), liveCookies: make(map[string]string), metadata: make(map[string][]any), quotaCache: make(map[string]quotaCacheEntry), ttl: 20 * time.Minute, quotaTTL: time.Minute}
}

func (c *Client) Invalidate(cookie string) {
	c.mu.Lock()
	prefix := cookieKey(cookie) + "\x00"
	for key := range c.sessions {
		if key == cookieKey(cookie) || strings.HasPrefix(key, prefix) {
			delete(c.sessions, key)
		}
	}
	for key := range c.quotaCache {
		if key == cookieKey(cookie) || strings.HasPrefix(key, prefix) {
			delete(c.quotaCache, key)
		}
	}
	for key := range c.liveCookies {
		if strings.HasPrefix(key, prefix) {
			delete(c.liveCookies, key)
		}
	}
	c.mu.Unlock()
}

func cookieKey(cookie string) string {
	cookie = normalizeCookieHeader(cookie)
	for _, piece := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(piece), "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) == "__Secure-1PSID" {
			return strings.TrimSpace(kv[1])
		}
	}
	return strings.TrimSpace(cookie)
}

// NormalizeCookieHeader converts a raw Cookie header or a browser-exported
// cookie JSON array to the header representation used by Gemini Web.
// Browser exports include expiry and domain metadata, but HTTP requests only
// send name/value pairs; the metadata is used here to avoid sending expired or
// out-of-scope cookies.
func NormalizeCookieHeader(cookie string) string {
	return normalizeCookieHeader(cookie)
}

// normalizeCookieHeader accepts both the browser's raw Cookie value and a
// copied header line ("Cookie: ..."). The admin UI stores the complete value
// so users can paste it directly from DevTools or a browser cookie exporter.
func normalizeCookieHeader(cookie string) string {
	cookie = strings.TrimSpace(cookie)
	if converted := browserCookieJSONHeader(cookie); converted != "" {
		cookie = converted
	}
	if strings.HasPrefix(strings.ToLower(cookie), "cookie:") {
		cookie = strings.TrimSpace(cookie[len("cookie:"):])
	}
	cookie = strings.ReplaceAll(cookie, "\r\n", "; ")
	cookie = strings.ReplaceAll(cookie, "\n", "; ")
	return cookie
}

type browserCookieExport struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	HostOnly       bool    `json:"hostOnly"`
	ExpirationDate float64 `json:"expirationDate"`
	Session        bool    `json:"session"`
}

func browserCookieJSONHeader(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || (trimmed[0] != '[' && trimmed[0] != '{') {
		return ""
	}
	var cookies []browserCookieExport
	if trimmed[0] == '[' {
		if err := json.Unmarshal([]byte(trimmed), &cookies); err != nil {
			return ""
		}
	} else {
		var wrapper struct {
			Cookies []browserCookieExport `json:"cookies"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err != nil {
			return ""
		}
		cookies = wrapper.Cookies
	}
	if len(cookies) == 0 {
		return ""
	}

	now := float64(time.Now().Unix())
	values := make(map[string]string, len(cookies))
	order := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		name := strings.TrimSpace(cookie.Name)
		if name == "" || cookie.Value == "" {
			continue
		}
		if cookie.ExpirationDate > 0 && !cookie.Session && cookie.ExpirationDate <= now {
			continue
		}
		domain := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(cookie.Domain), "."))
		if domain != "" {
			// Keep the shared Google-domain authentication cookies. A flat
			// header cannot preserve host/path scope, and sending a Gemini-only
			// cookie to accounts.google.com during RotateCookies is incorrect.
			if cookie.HostOnly || domain != "google.com" {
				continue
			}
		}
		if _, exists := values[name]; !exists {
			order = append(order, name)
		}
		values[name] = cookie.Value
	}
	if len(order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
}

func (c *Client) Complete(ctx context.Context, cred Credential, req Request, do Doer) (Result, error) {
	clock := &stageClock{start: time.Now(), timing: &Timing{PromptChars: utf8.RuneCountInString(req.Prompt), Attachments: len(req.Attachments)}}
	var result Result
	var err error
	if len(req.Attachments) > 0 {
		if do == nil {
			err = errors.New("gemini web transport is nil")
		} else if req.files, err = c.uploadAttachments(ctx, cred, req.Attachments, do); err == nil {
			clock.mark(&clock.timing.Upload, true)
		}
	}
	if err == nil {
		result, err = c.complete(ctx, cred, req, do, false, clock)
	}
	clock.mark(&clock.timing.Total, true)
	result.Timing = *clock.timing
	return result, err
}

// RefreshCookies asks Google's browser session endpoint to rotate the
// timestamp cookie (__Secure-1PSIDTS).  Gemini Web cookies are bearer-like
// browser credentials: unlike OAuth tokens they do not have an expiry field,
// and Google periodically invalidates an old timestamp cookie.  cliproxyapi
// keeps these sessions alive by calling RotateCookies every ten minutes; the
// Sub2API client does the same lazily before reusing an idle session.
//
// The returned header is the merged Cookie value and should be persisted by
// the account service.  No secret is included in returned errors.
func (c *Client) RefreshCookies(ctx context.Context, cred Credential, do Doer) (string, error) {
	cred.Cookie = normalizeCookieHeader(cred.Cookie)
	if strings.TrimSpace(cred.Cookie) == "" || cookieKey(cred.Cookie) == "" || cookieValue(cred.Cookie, "__Secure-1PSID") == "" {
		return cred.Cookie, ErrInvalidCredential
	}
	if do == nil {
		return cred.Cookie, errors.New("gemini web transport is nil")
	}
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration)
	c.mu.Lock()
	if live := c.liveCookies[key]; live != "" {
		cred.Cookie = live
	}
	c.mu.Unlock()
	// Rotation changes account state globally. Serialize calls across all
	// Client instances so the request path and background keepalive worker
	// cannot overwrite a fresh timestamp with an older Set-Cookie response.
	lock := cookieRefreshLock(credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet))
	lock.Lock()
	defer lock.Unlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://accounts.google.com/RotateCookies", strings.NewReader(`[000,"-0000000000000000000"]`))
	if err != nil {
		return cred.Cookie, err
	}
	ua := defaultString(cred.UserAgent, defaultUserAgent)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://accounts.google.com")
	referer := "https://gemini.google.com/app"
	if cred.hasAuthUser() {
		referer = "https://gemini.google.com/u/" + strconv.Itoa(cred.AuthUser) + "/app"
	}
	request.Header.Set("Referer", referer)
	request.Header.Set("X-Same-Domain", "1")
	request.Header.Set("User-Agent", ua)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cookie", cred.Cookie)
	if cred.hasAuthUser() {
		request.Header.Set("X-Goog-AuthUser", strconv.Itoa(cred.AuthUser))
	}
	resp, err := do(request)
	if err != nil {
		return cred.Cookie, err
	}
	if resp == nil {
		return cred.Cookie, errors.New("gemini web cookie rotation returned no response")
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	updated := mergeSetCookie(cred.Cookie, googleSharedCookieValues(resp.Header.Values("Set-Cookie")))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return updated, ErrInvalidCredential
	case http.StatusTooManyRequests:
		return updated, ErrRateLimited
	}
	if resp.StatusCode >= 400 {
		return updated, fmt.Errorf("gemini web cookie rotation upstream: %s", resp.Status)
	}
	if !hasChangedCookie(cred.Cookie, updated, "__Secure-1PSIDTS") {
		return updated, ErrCookieRotationUnavailable
	}
	c.updateSessionCookie(cred, updated)
	return updated, nil
}

// googleSharedCookieValues keeps the rotation response limited to cookies
// whose declared scope can be sent to Gemini. A host-only cookie from
// accounts.google.com must stay on that host; the account credential stores a
// flat Cookie header and cannot represent host-only scope, so it is discarded.
func googleSharedCookieValues(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, raw := range values {
		domain := ""
		for _, attr := range strings.Split(raw, ";")[1:] {
			parts := strings.SplitN(strings.TrimSpace(attr), "=", 2)
			if strings.EqualFold(strings.TrimSpace(parts[0]), "domain") && len(parts) == 2 {
				domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(parts[1]), "."))
				break
			}
		}
		// Only the parent Google domain is shared with gemini.google.com.
		// A cookie scoped to accounts.google.com (or another Google subdomain)
		// must remain host-specific; the flat account Cookie field cannot model
		// that scope safely.
		if domain == "google.com" {
			filtered = append(filtered, raw)
		}
	}
	return filtered
}

func hasChangedCookie(before, after, name string) bool {
	lookup := func(header string) string {
		for _, piece := range strings.Split(normalizeCookieHeader(header), ";") {
			kv := strings.SplitN(strings.TrimSpace(piece), "=", 2)
			if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), name) {
				return strings.TrimSpace(kv[1])
			}
		}
		return ""
	}
	oldValue, newValue := lookup(before), lookup(after)
	return newValue != "" && newValue != oldValue
}

func (c *Client) complete(ctx context.Context, cred Credential, req Request, do Doer, retried bool, clock *stageClock) (Result, error) {
	cred.Cookie = normalizeCookieHeader(cred.Cookie)
	if strings.TrimSpace(cred.Cookie) == "" || cookieKey(cred.Cookie) == "" {
		return Result{}, ErrInvalidCredential
	}
	if do == nil {
		return Result{}, errors.New("gemini web transport is nil")
	}
	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if base == "" {
		base = "https://gemini.google.com"
	}
	sess, sessionCookie, err := c.getSession(ctx, cred, base, do)
	if err != nil {
		return Result{UpdatedCookie: sessionCookie}, err
	}
	clock.mark(&clock.timing.Session, true)
	model := ResolveModel(req.Model)
	meta := req.Metadata
	inner := make([]any, 81)
	var files any
	if len(req.files) > 0 {
		files = req.files
	}
	inner[0] = []any{req.Prompt, 0, nil, files, nil, nil, 0}
	inner[1] = []any{defaultString(sess.Language, "en")}
	if meta == nil {
		meta = []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	}
	inner[2] = meta
	inner[6], inner[7], inner[10], inner[27], inner[30], inner[41], inner[45], inner[61], inner[68], inner[80] = []any{1}, 1, 10, 1, []any{4}, []any{1}, 1, []any{}, 1, 1
	think := 4
	if req.Thinking || model.Think == 0 {
		think = model.Think
	}
	inner[17] = []any{[]any{think}}
	requestUUID := uuid()
	inner[59] = requestUUID
	inner[79] = model.Mode
	encodedInner, _ := json.Marshal(inner)
	outer, _ := json.Marshal([]any{nil, string(encodedInner)})
	path := "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	if cred.hasAuthUser() {
		path = "/u/" + strconv.Itoa(cred.AuthUser) + path
	}
	endpoint := base + path + "?bl=" + url.QueryEscape(sess.Build) + "&hl=" + url.QueryEscape(defaultString(sess.Language, "en")) + "&_reqid=" + strconv.FormatInt(time.Now().UnixNano()%1000000, 10) + "&rt=c&f.sid=" + url.QueryEscape(sess.Fsid)
	body := "at=" + url.QueryEscape(sess.At) + "&f.req=" + url.QueryEscape(string(outer))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	postCred := cred
	if sessionCookie != "" {
		postCred.Cookie = sessionCookie
	}
	applyHeaders(httpReq, postCred, base, sess, model, requestUUID)
	resp, err := do(httpReq)
	if err != nil {
		return Result{}, err
	}
	if resp == nil || resp.Body == nil {
		return Result{}, errors.New("gemini web generation returned no response body")
	}
	// A stale-session retry overwrites these with the retried request's values.
	clock.mark(&clock.timing.Headers, true)
	clock.timing.Status = resp.StatusCode
	defer func() { _ = resp.Body.Close() }()
	updatedCookie := mergeSetCookie(sessionCookie, resp.Header.Values("Set-Cookie"))
	c.updateSessionCookie(postCred, updatedCookie)
	if live := c.currentSessionCookie(postCred); live != "" {
		updatedCookie = live
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Result{UpdatedCookie: updatedCookie}, ErrInvalidCredential
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Result{UpdatedCookie: updatedCookie}, ErrRateLimited
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		stale := resp.StatusCode == http.StatusMethodNotAllowed || len(bytes.TrimSpace(body)) == 0
		if stale && !retried {
			c.InvalidateCredential(cred)
			return c.complete(ctx, postCred, req, do, true, clock)
		}
		if stale {
			c.InvalidateCredential(cred)
		}
		if len(body) > 0 {
			return Result{UpdatedCookie: updatedCookie}, fmt.Errorf("gemini web session rejected: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return Result{UpdatedCookie: updatedCookie}, fmt.Errorf("gemini web session rejected: %s", resp.Status)
	}
	if resp.StatusCode >= 400 {
		return Result{UpdatedCookie: updatedCookie}, &UpstreamHTTPError{StatusCode: resp.StatusCode}
	}
	result := Result{UpdatedCookie: updatedCookie}
	var upstreamErr *UpstreamRPCError
	var raw bytes.Buffer
	parser := newFrameParser()
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			_, _ = raw.Write(chunk)
			for _, frame := range parser.feed(string(chunk)) {
				if frame.err != nil && upstreamErr == nil {
					upstreamErr = frame.err
				}
				if upstreamErr != nil {
					continue
				}
				result = applyFrame(result, frame)
				clock.observe(result)
				if req.OnResult != nil {
					if callbackErr := req.OnResult(result); callbackErr != nil {
						return result, callbackErr
					}
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				result.Raw = raw.Bytes()
				return result, readErr
			}
			break
		}
	}
	for _, frame := range parser.flush() {
		if frame.err != nil && upstreamErr == nil {
			upstreamErr = frame.err
		}
		if upstreamErr != nil {
			continue
		}
		result = applyFrame(result, frame)
		clock.observe(result)
		if req.OnResult != nil {
			if callbackErr := req.OnResult(result); callbackErr != nil {
				return result, callbackErr
			}
		}
	}
	result.Raw = raw.Bytes()
	if upstreamErr != nil {
		return result, upstreamErr
	}
	if strings.TrimSpace(result.Text) == "" && strings.TrimSpace(result.Thinking) == "" {
		return result, ErrEmptyResponse
	}
	// Metadata is only reused when the caller explicitly supplies it.  The
	// provider serves many users through one Cookie account; implicitly carrying
	// a previous browser conversation would cross user boundaries.
	return result, nil
}

func applyHeaders(r *http.Request, cred Credential, base string, sess session, model Model, requestUUID string) {
	ua := defaultString(cred.UserAgent, defaultUserAgent)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	r.Header.Set("Origin", base)
	r.Header.Set("Referer", base+"/")
	r.Header.Set("X-Same-Domain", "1")
	r.Header.Set("User-Agent", ua)
	r.Header.Set("Accept", "*/*")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.Header.Set("Cookie", cred.Cookie)
	if cred.hasAuthUser() {
		r.Header.Set("X-Goog-AuthUser", strconv.Itoa(cred.AuthUser))
	}
	r.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	r.Header.Set("x-goog-ext-73010990-jspb", "[0,0,0]")
	r.Header.Set("x-goog-ext-525005358-jspb", "[\""+requestUUID+"\",1]")
	if model.HeaderID != "" {
		h := []any{1, nil, nil, nil, model.HeaderID, nil, nil, 0, []int{4, 5, 6, 8}, nil, nil, model.Capacity, nil, nil, model.Mode, 1, requestUUID}
		b, _ := json.Marshal(h)
		r.Header.Set("x-goog-ext-525001261-jspb", string(b))
	}
}

func (c *Client) getSession(ctx context.Context, cred Credential, base string, do Doer) (session, string, error) {
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration)
	c.mu.Lock()
	cached, ok := c.sessions[key]
	liveCookie := c.liveCookies[key]
	c.mu.Unlock()
	if liveCookie != "" {
		// The background worker may have rotated an account before this client
		// bootstrapped a browser session. Prefer that live generation over the
		// older cookie row returned by the account repository.
		cred.Cookie = liveCookie
	}
	if ok && time.Since(cached.Fetched) < c.ttl {
		cookie := defaultString(liveCookie, defaultString(cached.Cookie, cred.Cookie))
		if cached.CookieRefreshed.IsZero() || time.Since(cached.CookieRefreshed) >= geminiWebCookieRefreshInterval {
			rotated, rotateErr := c.RefreshCookies(ctx, Credential{
				Cookie: cookie, AuthUser: cred.AuthUser, AuthUserSet: cred.AuthUserSet, BaseURL: base, UserAgent: cred.UserAgent, CacheGeneration: cred.CacheGeneration,
			}, do)
			if rotateErr == nil && rotated != "" {
				// RefreshCookies already merged the response into the cache. Read
				// the entry again so a concurrent model/quota response cannot be
				// overwritten by this older local snapshot.
				c.mu.Lock()
				live := c.sessions[key]
				c.mu.Unlock()
				if live.Cookie == "" {
					live = cached
					live.Cookie = rotated
					live.CookieRefreshed = time.Now()
					c.mu.Lock()
					c.sessions[key] = live
					c.mu.Unlock()
				}
				return live, defaultString(live.Cookie, rotated), nil
			}
			if errors.Is(rotateErr, ErrInvalidCredential) || errors.Is(rotateErr, ErrRateLimited) {
				return cached, rotated, rotateErr
			}
			// A transport failure or a response without a new timestamp cookie
			// should not take down a still-valid generation. Mark the attempt so
			// repeated requests do not hammer accounts.google.com; retry after
			// the normal ten-minute interval.
			c.mu.Lock()
			live := c.sessions[key]
			if live.Cookie == "" {
				live = cached
			}
			live.CookieRefreshed = time.Now()
			c.sessions[key] = live
			c.mu.Unlock()
			cached = live
			cookie = defaultString(live.Cookie, cookie)
		}
		return cached, cookie, nil
	}
	path := "/app"
	if cred.hasAuthUser() {
		path = "/u/" + strconv.Itoa(cred.AuthUser) + "/app"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return session{}, cred.Cookie, err
	}
	ua := defaultString(cred.UserAgent, defaultUserAgent)
	r.Header.Set("User-Agent", ua)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.Header.Set("Cookie", cred.Cookie)
	if cred.hasAuthUser() {
		r.Header.Set("X-Goog-AuthUser", strconv.Itoa(cred.AuthUser))
	}
	resp, err := do(r)
	if err != nil {
		return session{}, cred.Cookie, err
	}
	if resp == nil || resp.Body == nil {
		return session{}, cred.Cookie, errors.New("gemini web session returned no response body")
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return session{}, cred.Cookie, err
	}
	updated := mergeSetCookie(cred.Cookie, resp.Header.Values("Set-Cookie"))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return session{}, updated, ErrInvalidCredential
	}
	if resp.StatusCode == 429 {
		return session{}, updated, ErrRateLimited
	}
	if resp.StatusCode >= 400 {
		return session{}, updated, &UpstreamHTTPError{StatusCode: resp.StatusCode, Operation: "session"}
	}
	now := time.Now()
	s := session{At: findValue(string(b), `"SNlM0e"\s*:\s*"([^"]+)"`), Build: findValue(string(b), `"cfb2h"\s*:\s*"([^"]+)"`), Fsid: findValue(string(b), `"FdrFJe"\s*:\s*"([^"]+)"`), Language: findValue(string(b), `"TuX5cc"\s*:\s*"([^"]+)"`), Cookie: updated, CookieRefreshed: now, Fetched: now}
	s.ModelIDs = parseModelPicker(string(b))
	if s.Language == "" {
		s.Language = "en"
	}
	if s.At == "" || s.Build == "" || s.Fsid == "" {
		return session{}, updated, ErrInvalidCredential
	}
	c.mu.Lock()
	if live := c.liveCookies[key]; live != "" {
		// Keep any live cookie that arrived while bootstrap was in flight,
		// including companion cookies with the same timestamp.
		s.Cookie = live
	}
	c.sessions[key] = s
	if c.liveCookies == nil {
		c.liveCookies = make(map[string]string)
	}
	if c.liveCookies[key] == "" {
		c.liveCookies[key] = updated
	}
	c.mu.Unlock()
	return s, s.Cookie, nil
}

// updateSessionCookie keeps a live session aligned with cookies returned by a
// generation or usage response. The stable __Secure-1PSID key remains the
// session identity, so the next request can use the rotated timestamp without
// doing an unnecessary bootstrap.
func (c *Client) updateSessionCookie(cred Credential, cookie string) {
	if strings.TrimSpace(cookie) == "" {
		return
	}
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration)
	c.mu.Lock()
	defer c.mu.Unlock()
	previousCookie := c.liveCookies[key]
	if previousCookie == "" {
		if cached, ok := c.sessions[key]; ok {
			previousCookie = cached.Cookie
		}
	}
	previousTimestamp := cookieValue(previousCookie, "__Secure-1PSIDTS")
	updatedTimestamp := cookieValue(cookie, "__Secure-1PSIDTS")
	credentialTimestamp := cookieValue(cred.Cookie, "__Secure-1PSIDTS")
	// An in-flight request may still carry the old credential after another
	// request rotated the shared live session. Never let that old response
	// roll the cached timestamp back.
	if previousTimestamp != "" && credentialTimestamp != "" && previousTimestamp != credentialTimestamp {
		return
	}
	if previousTimestamp != "" && updatedTimestamp == "" {
		return
	}
	if c.liveCookies == nil {
		c.liveCookies = make(map[string]string)
	}
	c.liveCookies[key] = cookie
	if cached, ok := c.sessions[key]; ok {
		cached.Cookie = cookie
		if updatedTimestamp != "" && updatedTimestamp != previousTimestamp {
			cached.CookieRefreshed = time.Now()
		}
		c.sessions[key] = cached
	}
}

func (c *Client) currentSessionCookie(cred Credential) string {
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration)
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.liveCookies[key]; live != "" {
		return live
	}
	if cached, ok := c.sessions[key]; ok {
		return cached.Cookie
	}
	return ""
}

// CurrentCookie returns the newest cookie header held by the live session.
// Callers that persist rotated credentials should read it after a network
// operation so an overlapping request cannot write an older generation.
func (c *Client) CurrentCookie(cred Credential) string {
	return c.currentSessionCookie(cred)
}

func cookieValue(header, name string) string {
	for _, piece := range strings.Split(normalizeCookieHeader(header), ";") {
		kv := strings.SplitN(strings.TrimSpace(piece), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), name) {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

func findValue(body, expression string) string {
	re := regexp.MustCompile(expression)
	m := re.FindStringSubmatch(body)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}
func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
func uuid() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032X", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.ToUpper(fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16])))
}

func mergeSetCookie(current string, values []string) string {
	jar := map[string]string{}
	order := []string{}
	for _, raw := range strings.Split(current, ";") {
		kv := strings.SplitN(strings.TrimSpace(raw), "=", 2)
		if len(kv) == 2 {
			k := strings.TrimSpace(kv[0])
			if _, ok := jar[k]; !ok {
				order = append(order, k)
			}
			jar[k] = strings.TrimSpace(kv[1])
		}
	}
	for _, raw := range values {
		parts := strings.Split(raw, ";")
		first := parts[0]
		kv := strings.SplitN(strings.TrimSpace(first), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		deleteCookie := v == ""
		for _, attr := range parts[1:] {
			nameValue := strings.SplitN(strings.TrimSpace(attr), "=", 2)
			name := strings.ToLower(strings.TrimSpace(nameValue[0]))
			switch name {
			case "max-age":
				if len(nameValue) == 2 {
					if maxAge, err := strconv.Atoi(strings.TrimSpace(nameValue[1])); err == nil && maxAge <= 0 {
						deleteCookie = true
					}
				}
			case "expires":
				if len(nameValue) == 2 {
					if expires, err := http.ParseTime(strings.TrimSpace(nameValue[1])); err == nil && !expires.After(time.Now()) {
						deleteCookie = true
					}
				}
			}
		}
		if deleteCookie {
			delete(jar, k)
			continue
		}
		if _, ok := jar[k]; !ok {
			order = append(order, k)
		}
		jar[k] = v
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if value, ok := jar[k]; ok {
			parts = append(parts, k+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

// frame and payload parsing intentionally accepts both Google's length-prefixed
// stream and a plain JSON array.  The length has changed units over time, while
// the balanced JSON array remains stable.
type frame struct {
	payload string
	err     *UpstreamRPCError
}
type frameParser struct{ buf string }

func newFrameParser() *frameParser               { return &frameParser{} }
func (p *frameParser) feed(chunk string) []frame { p.buf += chunk; return p.take(false) }
func (p *frameParser) flush() []frame            { return p.take(true) }
func (p *frameParser) take(flush bool) []frame {
	// Only trim the front. The tail may be the middle of an unfinished frame,
	// and trimming it would delete a space inside a JSON string whenever a
	// network chunk ends right after that space.
	p.buf = strings.TrimPrefix(strings.TrimLeft(p.buf, " \t\r\n"), ")]}'")
	var out []frame
	for {
		start := strings.IndexByte(p.buf, '[')
		if start < 0 {
			if !flush && len(p.buf) > 32 {
				p.buf = p.buf[len(p.buf)-32:]
			}
			break
		}
		end := balanced(p.buf[start:])
		if end < 0 {
			if start > 0 {
				p.buf = p.buf[start:]
			}
			break
		}
		raw := p.buf[start : start+end]
		p.buf = p.buf[start+end:]
		var top []json.RawMessage
		if json.Unmarshal([]byte(raw), &top) != nil {
			continue
		}
		for _, entry := range top {
			var parts []json.RawMessage
			if json.Unmarshal(entry, &parts) != nil || len(parts) < 3 {
				continue
			}
			var kind string
			_ = json.Unmarshal(parts[0], &kind)
			if kind != "wrb.fr" {
				continue
			}
			// The RPC status belongs to the envelope, not the generated
			// payload. Searching raw text would also match quoted examples
			// of errors in otherwise successful answers.
			if len(parts) > 5 {
				if err := parseRPCError(parts[5]); err != nil {
					out = append(out, frame{err: err})
					continue
				}
			}
			var payload string
			_ = json.Unmarshal(parts[2], &payload)
			if payload != "" {
				out = append(out, frame{payload: payload})
			}
		}
	}
	return out
}
func balanced(s string) int {
	depth := 0
	in := false
	esc := false
	for i, c := range []byte(s) {
		if in {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				in = false
			}
			continue
		}
		switch c {
		case '"':
			in = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
func applyFrame(r Result, f frame) Result {
	var top []any
	if json.Unmarshal([]byte(f.payload), &top) != nil {
		return r
	}
	// StreamGenerate declares the model ID at payload[39]. Do not infer it
	// from generated text or the request: Google can silently downgrade a
	// requested model. Like native Gemini modelVersion, the latest declaration
	// wins, with conflicts retained for the shared response-model billing gate.
	if id, ok := topAt(top, 39).(string); ok {
		label, _ := topAt(top, 42).(string)
		if model := responseModel(id, label); model != "" {
			if r.ResponseModel != "" && !strings.EqualFold(r.ResponseModel, model) {
				r.ResponseModelConflict = true
			}
			r.ResponseModel = model
		}
	}
	if failed, ok := topAt(top, 31).(bool); ok && failed {
		r.GenerationFailed = true
	}
	if m, ok := topAt(top, 1).([]any); ok {
		r.Metadata = m
		if len(m) > 0 {
			r.CID, _ = m[0].(string)
		}
		if len(m) > 1 {
			r.RID, _ = m[1].(string)
		}
	}
	candidates, _ := topAt(top, 4).([]any)
	for _, raw := range candidates {
		c, ok := raw.([]any)
		if !ok {
			continue
		}
		if v, ok := nested(c, 1, 0).(string); ok {
			r.Text = v
		}
		if v, ok := nested(c, 37, 0, 0).(string); ok {
			r.Thinking = v
		}
	}
	return r
}
func topAt(v []any, i int) any {
	if i >= 0 && i < len(v) {
		return v[i]
	}
	return nil
}
func nested(v []any, path ...int) any {
	var cur any = v
	for _, i := range path {
		a, ok := cur.([]any)
		if !ok || i < 0 || i >= len(a) {
			return nil
		}
		cur = a[i]
	}
	return cur
}

// Keep bytes imported by older Go toolchains that optimize io.ReadAll paths.
var _ = bytes.MinRead
