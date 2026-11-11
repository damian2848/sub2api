//go:build unit

package service

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func captureSlog(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

const stickyLogMetadata = `{"device_id":"device-secret-0123456789","account_uuid":"acc-uuid","session_id":"sess-abcdef1234567890"}`

func TestGenerateSessionHashStickyLogsStayOffInfo(t *testing.T) {
	buf := captureSlog(t, slog.LevelInfo)
	svc := &GatewayService{}

	require.Equal(t, "sess-abcdef1234567890", svc.GenerateSessionHash(&ParsedRequest{MetadataUserID: stickyLogMetadata}))
	svc.GenerateSessionHash(&ParsedRequest{MetadataUserID: "raw-metadata-secret"})

	require.NotContains(t, buf.String(), "sticky.")
}

func TestGenerateSessionHashStickyDebugLogsDropClientIdentifiers(t *testing.T) {
	buf := captureSlog(t, slog.LevelDebug)
	svc := &GatewayService{}

	svc.GenerateSessionHash(&ParsedRequest{MetadataUserID: stickyLogMetadata})
	svc.GenerateSessionHash(&ParsedRequest{MetadataUserID: "raw-metadata-secret"})

	out := buf.String()
	require.Contains(t, out, "sticky.hash_source")
	require.Contains(t, out, "sticky.hash_metadata_parse_failed")
	require.Contains(t, out, "session_id=sess-abc")
	require.NotContains(t, out, "sess-abcdef1234567890")
	require.NotContains(t, out, "device-secret-0123456789")
	require.NotContains(t, out, "raw-metadata-secret")
}

func TestShortSessionHash(t *testing.T) {
	require.Equal(t, "", ShortSessionHash(""))
	require.Equal(t, "abc", ShortSessionHash("abc"))
	require.Equal(t, "01234567", ShortSessionHash("0123456789abcdef"))
}
