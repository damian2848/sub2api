package repository

import (
	"context"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"strings"
	"testing"
	"time"
)

func TestAntigravityPlanlensTransactionCommitAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			repo := newAccountRepositoryWithSQL(client, db, nil)
			mock.ExpectBegin()
			mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("LOCK TABLE accounts, account_groups, groups, proxies IN SHARE ROW EXCLUSIVE MODE").WillReturnResult(sqlmock.NewResult(0, 0))
			if fail {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			err = repo.WithAntigravityImportTransaction(context.Background(), func(accounts service.AccountRepository, groups service.GroupRepository, proxies service.ProxyRepository) (int64, error) {
				transactional, ok := accounts.(*accountRepository)
				if !ok {
					t.Fatal("unexpected transactional account repository")
				}
				if transactional.client == repo.client {
					t.Fatal("writes are not transactional")
				}
				if fail {
					return 0, errors.New("stale_revision")
				}
				return 12, nil
			})
			if (err != nil) != fail {
				t.Fatalf("unexpected error %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAntigravityPlanlensCredentialsCASPreservesState(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	err := repo.ApplyAntigravityImportCredentials(context.Background(), &service.Account{ID: 12, Credentials: map[string]any{"access_token": "old"}}, map[string]any{"access_token": "new"}, time.Now().Add(time.Minute).UnixMilli())
	if err == nil || err.Error() != "stale_revision" {
		t.Fatal("CAS miss accepted")
	}
	if len(exec.execQueries) != 1 {
		t.Fatal("outbox is not atomic")
	}
	query := exec.execQueries[0]
	for _, required := range []string{"credentials=credentials ||", "credentials=$2::jsonb", "clock_timestamp()", "INSERT INTO scheduler_outbox"} {
		if !strings.Contains(query, required) {
			t.Fatal("missing guard", required)
		}
	}
	mutation := strings.Split(query, "WHERE")[0]
	for _, forbidden := range []string{"schedulable=", "proxy_id=", "extra=", "temp_unschedulable_until="} {
		if strings.Contains(mutation, forbidden) {
			t.Fatal("manual state mutation", forbidden)
		}
	}
	if !strings.Contains(mutation, "status=CASE WHEN $6") || !strings.Contains(mutation, "error_message=CASE WHEN $6") {
		t.Fatal("error clearing must be conditional")
	}
}
func TestAntigravityPlanlensBackgroundGenerationCAS(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	applied, err := repo.UpdateAntigravityCredentialsIfGeneration(context.Background(), 12, "old", map[string]any{"access_token": "late", "model_mapping": "must-not-change"})
	if err != nil || applied {
		t.Fatal("late refresh applied")
	}
	if !strings.Contains(exec.execQueries[0], "coalesce(credentials->>'planlens_oauth_generation','')=$2") {
		t.Fatal("missing generation CAS")
	}
}
