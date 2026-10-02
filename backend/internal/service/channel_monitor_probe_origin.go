package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	ChannelMonitorProbeOriginHeader = "X-Sub2api-Monitor-Probe"
	RequestSourceBusiness           = "business"
	RequestSourceProbe              = "probe"
	monitorProbeOriginKind          = "channel-monitor-probe/v1"
	monitorProbeOriginMaxBody       = 1 << 20
)

type monitorProbeSignerKey struct{}
type monitorProbeOriginKey struct{}
type monitorProbeSigner struct{ encryptor SecretEncryptor }

type monitorProbeOriginToken struct {
	Kind       string `json:"kind"`
	ExpiresAt  int64  `json:"expires_at"`
	Method     string `json:"method"`
	Host       string `json:"host"`
	Path       string `json:"path"`
	AuthDigest string `json:"auth_digest"`
	BodyDigest string `json:"body_digest"`
}

func WithChannelMonitorProbeSigner(ctx context.Context, encryptor SecretEncryptor) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, monitorProbeSignerKey{}, monitorProbeSigner{encryptor})
}

// SignChannelMonitorProbeRequest runs after adapters finish the request headers/body.
func SignChannelMonitorProbeRequest(ctx context.Context, req *http.Request) error {
	if req == nil || req.URL == nil {
		return errors.New("monitor probe request is unavailable")
	}
	req.Header.Del(ChannelMonitorProbeOriginHeader)
	if ctx == nil {
		return nil
	}
	signer, present := ctx.Value(monitorProbeSignerKey{}).(monitorProbeSigner)
	if !present {
		return nil
	}
	if signer.encryptor == nil {
		return errors.New("monitor probe request cannot be signed")
	}
	body, err := monitorProbeRequestBody(req)
	if err != nil {
		return errors.New("monitor probe request cannot be signed")
	}
	token := monitorProbeOriginToken{
		Kind: monitorProbeOriginKind, ExpiresAt: time.Now().Add(2 * time.Minute).Unix(),
		Method: req.Method, Host: monitorProbeHost(req), Path: req.URL.RequestURI(),
		AuthDigest: monitorProbeAuthDigest(req), BodyDigest: monitorProbeDigest(body),
	}
	payload, err := json.Marshal(token)
	if err != nil {
		return errors.New("monitor probe request cannot be signed")
	}
	sealed, err := signer.encryptor.Encrypt(string(payload))
	if err != nil || sealed == "" {
		return errors.New("monitor probe request cannot be signed")
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set(ChannelMonitorProbeOriginHeader, sealed)
	return nil
}

func (s *ChannelMonitorService) ProbeOriginMiddleware() gin.HandlerFunc {
	var mu sync.Mutex
	used := make(map[string]time.Time)
	return func(c *gin.Context) {
		sealed := c.GetHeader(ChannelMonitorProbeOriginHeader)
		c.Request.Header.Del(ChannelMonitorProbeOriginHeader)
		if s != nil && s.encryptor != nil && len(sealed) > 0 && len(sealed) <= 8192 {
			now := time.Now()
			if s.validProbeOrigin(c.Request, sealed, now) {
				mu.Lock()
				for key, until := range used {
					if !until.After(now) {
						delete(used, key)
					}
				}
				key := monitorProbeDigest([]byte(sealed))
				_, replayed := used[key]
				allowed := !replayed && len(used) < 8192
				if allowed {
					used[key] = now.Add(2 * time.Minute)
				}
				mu.Unlock()
				if allowed {
					c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), monitorProbeOriginKey{}, RequestSourceProbe))
				}
			}
		}
		c.Next()
	}
}

func (s *ChannelMonitorService) validProbeOrigin(req *http.Request, sealed string, now time.Time) bool {
	plain, err := s.encryptor.Decrypt(sealed)
	if err != nil {
		return false
	}
	var token monitorProbeOriginToken
	if json.Unmarshal([]byte(plain), &token) != nil || token.Kind != monitorProbeOriginKind ||
		token.ExpiresAt < now.Unix() || token.ExpiresAt > now.Add(2*time.Minute+10*time.Second).Unix() ||
		token.Method != req.Method || token.Host != monitorProbeHost(req) || token.Path != req.URL.RequestURI() ||
		token.AuthDigest != monitorProbeAuthDigest(req) {
		return false
	}
	body, err := monitorProbeRequestBody(req)
	return err == nil && token.BodyDigest == monitorProbeDigest(body)
}

func ChannelMonitorRequestSource(ctx context.Context) string {
	if ctx != nil && ctx.Value(monitorProbeOriginKey{}) == RequestSourceProbe {
		return RequestSourceProbe
	}
	return RequestSourceBusiness
}

func CopyChannelMonitorRequestSource(parent, base context.Context) context.Context {
	if base == nil {
		base = context.Background()
	}
	if ChannelMonitorRequestSource(parent) == RequestSourceProbe {
		return context.WithValue(base, monitorProbeOriginKey{}, RequestSourceProbe)
	}
	return base
}

func monitorProbeRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, monitorProbeOriginMaxBody+1))
	req.Body = &monitorProbeReplayBody{Reader: io.MultiReader(bytes.NewReader(body), req.Body), Closer: req.Body}
	if err != nil || len(body) > monitorProbeOriginMaxBody {
		return nil, errors.New("monitor probe request body is unavailable")
	}
	return body, nil
}

type monitorProbeReplayBody struct {
	io.Reader
	io.Closer
}

func monitorProbeHost(req *http.Request) string {
	if req.Host != "" {
		return strings.ToLower(req.Host)
	}
	return strings.ToLower(req.URL.Host)
}

func monitorProbeAuthDigest(req *http.Request) string {
	return monitorProbeDigest([]byte(strings.Join([]string{
		strings.TrimSpace(req.Header.Get("Authorization")), strings.TrimSpace(req.Header.Get("X-Api-Key")),
		strings.TrimSpace(req.Header.Get("X-Goog-Api-Key")), req.URL.Query().Get("key"),
	}, "\n")))
}

func monitorProbeDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
