//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func usageLogAPISuccessColumnIndex(t *testing.T) int {
	t.Helper()
	for i, column := range strings.Split(usageLogSelectColumns, ",") {
		if strings.TrimSpace(column) == "api_success" {
			return i - 1 // INSERT omits SELECT's leading id column
		}
	}
	t.Fatal("api_success missing from usage log SELECT columns")
	return -1
}

func TestUsageLogAPISuccessTriStateInsertPaths(t *testing.T) {
	truth, falsity := true, false
	for _, tc := range []struct {
		name  string
		value *bool
		want  sql.NullBool
	}{
		{"success", &truth, sql.NullBool{Bool: true, Valid: true}},
		{"failure", &falsity, sql.NullBool{Bool: false, Valid: true}},
		{"unknown_legacy", nil, sql.NullBool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &service.UsageLog{
				UserID: 1, APIKeyID: 2, AccountID: 3, Model: "model", RequestID: "tri-state-" + tc.name,
				APISuccess: tc.value, CreatedAt: time.Now().UTC(),
			}
			prepared := prepareUsageLogInsert(log)
			index := usageLogAPISuccessColumnIndex(t)
			require.Len(t, prepared.args, len(usageLogInsertArgTypes))
			require.Equal(t, "boolean", usageLogInsertArgTypes[index])
			require.Equal(t, tc.want, prepared.args[index])

			var captured []string
			db, mock := newSQLCapturingMock(t, &captured)
			repo := &usageLogRepository{sql: db}
			mock.ExpectQuery("INSERT INTO usage_logs").
				WithArgs(anySliceToDriverValues(prepared.args)...).
				WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(1, log.CreatedAt))
			inserted, err := repo.createSingle(context.Background(), db, log)
			require.NoError(t, err)
			require.True(t, inserted)
			mock.ExpectExec("INSERT INTO usage_logs").WithArgs(anySliceToDriverValues(prepared.args)...).
				WillReturnResult(sqlmock.NewResult(0, 1))
			require.NoError(t, execUsageLogInsertNoResult(context.Background(), db, prepared))
			require.NoError(t, mock.ExpectationsWereMet())
			for _, query := range captured {
				requireStaticInsertMatchesArgTypes(t, query)
				require.Contains(t, query, "api_success,")
			}
		})
	}
}

func TestUsageLogAPISuccessBatchTriStateColumnAndArgumentAlignment(t *testing.T) {
	truth, falsity := true, false
	preparedList := make([]usageLogInsertPrepared, 0, 3)
	keys := make([]string, 0, 3)
	byKey := map[string]usageLogInsertPrepared{}
	for i, value := range []*bool{&truth, &falsity, nil} {
		log := &service.UsageLog{RequestID: fmt.Sprintf("req-%d", i), APIKeyID: 2, Model: "model", APISuccess: value}
		prepared := prepareUsageLogInsert(log)
		preparedList = append(preparedList, prepared)
		key := usageLogBatchKey(log.RequestID, log.APIKeyID)
		keys = append(keys, key)
		byKey[key] = prepared
	}
	index := usageLogAPISuccessColumnIndex(t)
	for _, withInputIndex := range []bool{true, false} {
		var query string
		var args []any
		if withInputIndex {
			query, args = buildUsageLogBatchInsertQuery(keys, byKey)
		} else {
			query, args = buildUsageLogBestEffortInsertQuery(preparedList)
		}
		stride := len(usageLogInsertArgTypes)
		offset := 0
		if withInputIndex {
			stride++
			offset++
		}
		require.Len(t, args, 3*stride)
		for i, prepared := range preparedList {
			argPosition := i*stride + offset + index
			require.Equal(t, prepared.args[index], args[argPosition])
			require.Contains(t, query, fmt.Sprintf("$%d::boolean", argPosition+1))
		}
		// Each CTE input, INSERT and SELECT carries exactly one marker column.
		require.Equal(t, 3, strings.Count(query, "api_success,"))
		placeholders := regexp.MustCompile(`\$\d+`).FindAllString(query, -1)
		require.Len(t, placeholders, len(args))
		for i, placeholder := range placeholders {
			require.Equal(t, fmt.Sprintf("$%d", i+1), placeholder)
		}
	}
}

type apiSuccessUsageLogScanner struct{ success sql.NullBool }

func (s apiSuccessUsageLogScanner) Scan(dest ...any) error {
	for i, column := range strings.Split(usageLogSelectColumns, ",") {
		if strings.TrimSpace(column) == "api_success" {
			value, ok := dest[i].(*sql.NullBool)
			if !ok {
				return fmt.Errorf("api_success scanner destination is %T", dest[i])
			}
			*value = s.success
			return nil
		}
	}
	return fmt.Errorf("missing api_success column")
}

func TestUsageLogAPISuccessTriStateRead(t *testing.T) {
	for _, value := range []sql.NullBool{{Bool: true, Valid: true}, {Bool: false, Valid: true}, {}} {
		log, err := scanUsageLog(apiSuccessUsageLogScanner{success: value})
		require.NoError(t, err)
		if value.Valid {
			require.NotNil(t, log.APISuccess)
			require.Equal(t, value.Bool, *log.APISuccess)
		} else {
			require.Nil(t, log.APISuccess, "NULL legacy outcomes must not default to success or failure")
		}
	}
}
