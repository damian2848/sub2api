// Package geminiweb is a small, independently implemented client for the
// browser-facing Gemini endpoint.  It intentionally has no dependency on the
// external geminiweb2api project or on Sub2API's service layer.
package geminiweb

import (
	"bufio"
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
)

var (
	ErrInvalidCredential = errors.New("gemini web credential is invalid")
	ErrRateLimited       = errors.New("gemini web is rate limited")
	ErrEmptyResponse     = errors.New("gemini web returned no answer")
)

type Credential struct {
	Cookie    string
	AuthUser  int
	BaseURL   string
	UserAgent string
}

type Request struct {
	Model    string
	Prompt   string
	Metadata []any
	Thinking bool
}

type Result struct {
	Text          string
	Thinking      string
	Metadata      []any
	CID           string
	RID           string
	RequestID     string
	UpdatedCookie string
}

type Doer func(*http.Request) (*http.Response, error)

type session struct {
	At, Build, Fsid, Language string
	Fetched                   time.Time
}

type Client struct {
	mu         sync.Mutex
	sessions   map[string]session
	metadata   map[string][]any
	quotaCache map[string]quotaCacheEntry
	ttl        time.Duration
	quotaTTL   time.Duration
}

func NewClient() *Client {
	return &Client{sessions: make(map[string]session), metadata: make(map[string][]any), quotaCache: make(map[string]quotaCacheEntry), ttl: 20 * time.Minute, quotaTTL: time.Minute}
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

// normalizeCookieHeader accepts both the browser's raw Cookie value and a
// copied header line ("Cookie: ...").  The admin UI intentionally stores the
// complete cookie header so users can paste it directly from DevTools.
func normalizeCookieHeader(cookie string) string {
	cookie = strings.TrimSpace(cookie)
	if strings.HasPrefix(strings.ToLower(cookie), "cookie:") {
		cookie = strings.TrimSpace(cookie[len("cookie:"):])
	}
	cookie = strings.ReplaceAll(cookie, "\r\n", "; ")
	cookie = strings.ReplaceAll(cookie, "\n", "; ")
	return cookie
}

func (c *Client) Complete(ctx context.Context, cred Credential, req Request, do Doer) (Result, error) {
	return c.complete(ctx, cred, req, do, false)
}

func (c *Client) complete(ctx context.Context, cred Credential, req Request, do Doer, retried bool) (Result, error) {
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
	model := ResolveModel(req.Model)
	meta := req.Metadata
	inner := make([]any, 81)
	inner[0] = []any{req.Prompt, 0, nil, nil, nil, nil, 0}
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
	if cred.AuthUser > 0 {
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
	defer resp.Body.Close()
	updatedCookie := mergeSetCookie(sessionCookie, resp.Header.Values("Set-Cookie"))
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
			c.Invalidate(cred.Cookie)
			return c.complete(ctx, postCred, req, do, true)
		}
		if stale {
			c.Invalidate(cred.Cookie)
		}
		if len(body) > 0 {
			return Result{UpdatedCookie: updatedCookie}, fmt.Errorf("gemini web session rejected: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return Result{UpdatedCookie: updatedCookie}, fmt.Errorf("gemini web session rejected: %s", resp.Status)
	}
	if resp.StatusCode >= 400 {
		return Result{UpdatedCookie: updatedCookie}, fmt.Errorf("gemini web upstream: %s", resp.Status)
	}
	result := Result{UpdatedCookie: updatedCookie}
	var raw bytes.Buffer
	parser := newFrameParser()
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	for {
		chunk, readErr := reader.ReadBytes(0)
		if len(chunk) > 0 {
			raw.Write(chunk)
			for _, frame := range parser.feed(string(chunk)) {
				result = applyFrame(result, frame)
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return result, readErr
			}
			break
		}
	}
	for _, frame := range parser.flush() {
		result = applyFrame(result, frame)
	}
	for _, code := range []string{"1016", "1037", "1050", "1052", "1060", "1013"} {
		if strings.Contains(raw.String(), "BardErrorInfo") && strings.Contains(raw.String(), code) {
			if code == "1016" {
				return result, ErrInvalidCredential
			}
			if code == "1037" || code == "1013" {
				return result, ErrRateLimited
			}
			return result, fmt.Errorf("gemini web BardErrorInfo %s", code)
		}
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
	ua := cred.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36"
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	r.Header.Set("Origin", base)
	r.Header.Set("Referer", base+"/")
	r.Header.Set("X-Same-Domain", "1")
	r.Header.Set("User-Agent", ua)
	r.Header.Set("Accept", "*/*")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.Header.Set("Cookie", cred.Cookie)
	if cred.AuthUser > 0 {
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
	key := cookieKey(cred.Cookie) + "\x00" + strconv.Itoa(cred.AuthUser)
	c.mu.Lock()
	cached, ok := c.sessions[key]
	c.mu.Unlock()
	if ok && time.Since(cached.Fetched) < c.ttl {
		return cached, cred.Cookie, nil
	}
	path := "/app"
	if cred.AuthUser > 0 {
		path = "/u/" + strconv.Itoa(cred.AuthUser) + "/app"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return session{}, cred.Cookie, err
	}
	ua := cred.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36"
	}
	r.Header.Set("User-Agent", ua)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.Header.Set("Cookie", cred.Cookie)
	if cred.AuthUser > 0 {
		r.Header.Set("X-Goog-AuthUser", strconv.Itoa(cred.AuthUser))
	}
	resp, err := do(r)
	if err != nil {
		return session{}, cred.Cookie, err
	}
	defer resp.Body.Close()
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
		return session{}, updated, fmt.Errorf("gemini web session upstream: %s", resp.Status)
	}
	s := session{At: findValue(string(b), `"SNlM0e"\s*:\s*"([^"]+)"`), Build: findValue(string(b), `"cfb2h"\s*:\s*"([^"]+)"`), Fsid: findValue(string(b), `"FdrFJe"\s*:\s*"([^"]+)"`), Language: findValue(string(b), `"TuX5cc"\s*:\s*"([^"]+)"`), Fetched: time.Now()}
	if s.Language == "" {
		s.Language = "en"
	}
	if s.At == "" || s.Build == "" || s.Fsid == "" {
		return session{}, updated, ErrInvalidCredential
	}
	c.mu.Lock()
	c.sessions[key] = s
	c.mu.Unlock()
	return s, updated, nil
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
func cloneAny(v []any) []any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var out []any
	_ = json.Unmarshal(b, &out)
	return out
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
		first := strings.Split(raw, ";")[0]
		kv := strings.SplitN(strings.TrimSpace(first), "=", 2)
		if len(kv) == 2 {
			k := strings.TrimSpace(kv[0])
			if _, ok := jar[k]; !ok {
				order = append(order, k)
			}
			jar[k] = strings.TrimSpace(kv[1])
		}
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, k+"="+jar[k])
	}
	return strings.Join(parts, "; ")
}

func outerURLDecode(s string) string { v, _ := url.QueryUnescape(s); return v }

// frame and payload parsing intentionally accepts both Google's length-prefixed
// stream and a plain JSON array.  The length has changed units over time, while
// the balanced JSON array remains stable.
type frame struct{ payload string }
type frameParser struct{ buf string }

func newFrameParser() *frameParser               { return &frameParser{} }
func (p *frameParser) feed(chunk string) []frame { p.buf += chunk; return p.take(false) }
func (p *frameParser) flush() []frame            { return p.take(true) }
func (p *frameParser) take(flush bool) []frame {
	p.buf = strings.TrimPrefix(strings.TrimSpace(p.buf), ")]}'")
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
