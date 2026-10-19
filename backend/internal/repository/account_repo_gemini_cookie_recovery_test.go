package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"strings"
	"testing"
	"time"
)

func TestRecoveryGenerationCASIncludesAtomicOutboxAndExactGuards(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	expected := &service.Account{ID: 12, Name: "test", Credentials: map[string]any{"gemini_web_cookie": "old"}}
	applied, err := repo.ApplyGeminiWebCookieRecovery(context.Background(), expected, "new", "generation", time.Now().Add(time.Minute).UnixMilli())
	if err != nil || applied {
		t.Fatal("failed CAS was applied")
	}
	if len(exec.execQueries) != 1 {
		t.Fatal("write and outbox must be atomic")
	}
	query := exec.execQueries[0]
	for _, guard := range []string{"WITH updated AS", "credentials = $2::jsonb", "status = 'active'", "schedulable = TRUE", "temp_unschedulable_reason = $5", "clock_timestamp()", "NOT EXISTS", "INSERT INTO scheduler_outbox"} {
		if !strings.Contains(query, guard) {
			t.Fatalf("missing guard %s", guard)
		}
	}
}

func TestRecoveryGenerationRedisCleanupPreservesLaterFailure(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	cache := NewTempUnschedCache(client).(*tempUnschedCache)
	ctx := context.Background()
	old := &service.TempUnschedState{UntilUnix: time.Now().Add(time.Minute).Unix(), ErrorMessage: service.GeminiWebCookieRotationRejected}
	if err := cache.SetTempUnsched(ctx, 12, old); err != nil {
		t.Fatal(err)
	}
	later := &service.TempUnschedState{UntilUnix: time.Now().Add(2 * time.Minute).Unix(), ErrorMessage: "unrelated failure"}
	if err := cache.SetTempUnsched(ctx, 12, later); err != nil {
		t.Fatal(err)
	}
	if err := cache.DeleteTempUnschedIfMatches(ctx, 12, old); err != nil {
		t.Fatal(err)
	}
	got, err := cache.GetTempUnsched(ctx, 12)
	if err != nil || got == nil || got.ErrorMessage != later.ErrorMessage {
		t.Fatal("later failure removed")
	}
	if err := cache.SetTempUnsched(ctx, 13, old); err != nil {
		t.Fatal(err)
	}
	if err := cache.DeleteTempUnschedIfMatches(ctx, 13, old); err != nil {
		t.Fatal(err)
	}
	got, err = cache.GetTempUnsched(ctx, 13)
	if err != nil || got != nil {
		t.Fatal("original rejection not removed")
	}
}
