//go:build unit

package tlsfingerprint

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// 回归：直连（无代理）TLS 指纹拨号必须有建连超时，与代理 dialer 和普通 Transport 一致。
func TestNewDirectNetDialerHasBoundedTimeout(t *testing.T) {
	d := newDirectNetDialer()
	if d.Timeout != directDialTimeout || d.Timeout <= 0 {
		t.Fatalf("direct dialer timeout = %v, want %v", d.Timeout, directDialTimeout)
	}
	if d.KeepAlive != directDialKeepAlive {
		t.Fatalf("direct dialer keepalive = %v, want %v", d.KeepAlive, directDialKeepAlive)
	}
}

// DialTLSContext 传给 baseDialer 的 ctx 必须带 deadline，即使调用方 ctx 没有。
func TestDialerDialTLSContextPassesDeadlineToBaseDialer(t *testing.T) {
	var gotDeadline bool
	var remaining time.Duration
	errSentinel := errors.New("sentinel dial failure")
	d := NewDialer(&Profile{Name: "test"}, func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dl time.Time
		dl, gotDeadline = ctx.Deadline()
		remaining = time.Until(dl)
		return nil, errSentinel
	})

	_, err := d.DialTLSContext(context.Background(), "tcp", "example.com:443")
	if !errors.Is(err, errSentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if !gotDeadline {
		t.Fatal("base dialer ctx has no deadline")
	}
	if remaining <= 0 || remaining > directTLSDialTimeout {
		t.Fatalf("unexpected remaining deadline %v (limit %v)", remaining, directTLSDialTimeout)
	}
}

// 服务端只 accept、不回 ServerHello 时，握手必须在超时内失败，而不是无限挂住。
func TestDialerDialTLSContextHandshakeTimeout(t *testing.T) {
	orig := directTLSDialTimeout
	directTLSDialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { directTLSDialTimeout = orig })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, c) // hold open, never respond
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range accepted {
			_ = c.Close()
		}
		mu.Unlock()
	})

	d := NewDialer(&Profile{Name: "test"}, nil)
	start := time.Now()
	conn, err := d.DialTLSContext(context.Background(), "tcp", ln.Addr().String())
	elapsed := time.Since(start)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected handshake timeout error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("handshake did not time out promptly: %v", elapsed)
	}
}
