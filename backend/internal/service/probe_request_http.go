package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// This transport decorates only explicitly scoped generation tests. HEAD, GET,
// quota discovery and ordinary gateway requests never acquire a probe fact.
type probeRequestHTTPUpstream struct{ HTTPUpstream }

func observeProbeHTTPUpstream(upstream HTTPUpstream) HTTPUpstream {
	if upstream == nil {
		return nil
	}
	if _, decorated := upstream.(*probeRequestHTTPUpstream); decorated {
		return upstream
	}
	return &probeRequestHTTPUpstream{HTTPUpstream: upstream}
}

func (p *probeRequestHTTPUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return observeProbeHTTPRequest(req, func() (*http.Response, error) { return p.HTTPUpstream.Do(req, proxy, accountID, concurrency) })
}
func (p *probeRequestHTTPUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return observeProbeHTTPRequest(req, func() (*http.Response, error) {
		return p.HTTPUpstream.DoWithTLS(req, proxy, accountID, concurrency, profile)
	})
}

func observeProbeHTTPRequest(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
	if req == nil || req.Method != http.MethodPost {
		return send()
	}
	collector := pelicanUsageFromContext(req.Context())
	if collector == nil {
		return send()
	}
	// A scoped test can pass through the AccountTest and gateway decorators.
	// Nested observation of the very same network request is not another call.
	if collector.activeHTTPRequests[req] {
		return send()
	}
	if collector.activeHTTPRequests == nil {
		collector.activeHTTPRequests = make(map[*http.Request]bool)
	}
	collector.activeHTTPRequests[req] = true
	defer delete(collector.activeHTTPRequests, req)
	path := strings.ToLower(req.URL.Path)
	if strings.HasSuffix(path, "/input_tokens") || strings.HasSuffix(path, "/count_tokens") {
		return send()
	}
	protocol := "openai"
	switch {
	case strings.Contains(path, "chat/completions"):
		protocol = "chat"
	case strings.Contains(path, "messages") || strings.HasSuffix(path, ":streamrawpredict") || strings.HasSuffix(path, "/invoke"):
		protocol = "anthropic"
	case strings.Contains(path, "generatecontent"):
		protocol = "gemini"
	case strings.Contains(path, "responses"):
	default:
		return send() // Token refresh/media/quota calls are not text generation.
	}
	u := &pelicanTestUsage{protocol: protocol, model: collector.model, attemptID: uuid.NewString(), startedAt: time.Now(), networkAttempt: true}
	collector.requests = append(collector.requests, u)
	streamRequested := false
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr == nil {
			data, _ := io.ReadAll(io.LimitReader(body, 1<<20))
			_ = body.Close()
			if model := gjson.GetBytes(data, "model").String(); model != "" {
				u.model = model
			}
			streamRequested = gjson.GetBytes(data, "stream").Bool()
		}
	}
	resp, err := send()
	if err != nil {
		kind := probeRequestErrorKind(err, "transport")
		u.errorKind = &kind
		u.finishedAt = time.Now()
		return resp, err
	}
	if resp == nil {
		kind := "transport"
		u.errorKind = &kind
		u.finishedAt = time.Now()
		return resp, nil
	}
	status := resp.StatusCode
	u.httpStatus = &status
	if status < 200 || status >= 300 {
		kind := "http"
		u.errorKind = &kind
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	u.canTTFT = strings.Contains(contentType, "text/event-stream")
	if !u.canTTFT && !strings.Contains(contentType, "application/json") {
		u.canTTFT = streamRequested || protocol == "gemini" && strings.Contains(path, "streamgeneratecontent")
	}
	if resp.Body == nil {
		kind := "stream"
		if u.errorKind == nil {
			u.errorKind = &kind
		}
		u.finishedAt = time.Now()
	} else {
		resp.Body = &probeRequestBody{ReadCloser: resp.Body, usage: u, stream: u.canTTFT}
	}
	return resp, nil
}

type probeRequestBody struct {
	io.ReadCloser
	usage   *pelicanTestUsage
	stream  bool
	pending []byte
}

func (b *probeRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		// Bound parser memory; giant/invalid frames cannot fill the ledger reader.
		if len(b.pending)+n <= 1<<20 {
			b.pending = append(b.pending, p[:n]...)
		} else {
			b.pending = nil
			b.usage.apiFailed = true
		}
		if b.stream {
			for {
				i := bytes.IndexByte(b.pending, '\n')
				if i < 0 {
					break
				}
				b.readLine(b.pending[:i])
				b.pending = b.pending[i+1:]
			}
		}
	}
	if err != nil {
		if len(b.pending) > 0 {
			if b.stream {
				b.readLine(b.pending)
			} else {
				b.usage.read(string(b.pending))
			}
			b.pending = nil
		}
		if err != io.EOF {
			kind := probeRequestErrorKind(err, "stream")
			if b.usage.errorKind == nil {
				b.usage.errorKind = &kind
			}
		}
		b.finish()
	}
	return n, err
}
func (b *probeRequestBody) readLine(line []byte) {
	text := strings.TrimSpace(string(line))
	if data, ok := strings.CutPrefix(text, "data:"); ok {
		b.usage.read(strings.TrimSpace(data))
	}
}
func (b *probeRequestBody) finish() {
	if b.usage.finishedAt.IsZero() {
		b.usage.finishedAt = time.Now()
	}
}
func (b *probeRequestBody) Close() error {
	b.finish()
	err := b.ReadCloser.Close()
	if err != nil && b.usage.errorKind == nil {
		kind := "stream"
		b.usage.errorKind = &kind
	}
	return err
}

func probeRequestErrorKind(err error, fallback string) string {
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return fallback
}
