package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

func TestCredentialMatchesProductionIdentity(t *testing.T) {
	data := map[string]any{"gemini_web_cookie": "__Secure-1PSID=principal; __Secure-1PSIDTS=old", "auth_user": float64(2), "gemini_web_cookie_generation": "generation"}
	cred, id := credential(data)
	if cred.AuthUser != 2 || !cred.AuthUserSet {
		t.Fatal("auth_user not preserved")
	}
	payload, _ := json.Marshal([]any{"principal", 2, true, "generation"})
	digest := sha256.Sum256(payload)
	if id != hex.EncodeToString(digest[:]) {
		t.Fatal("identity differs from production policy")
	}
	data["gemini_web_cookie"] = "__Secure-1PSID=principal; __Secure-1PSIDTS=rotated"
	_, rotated := credential(data)
	if rotated != id {
		t.Fatal("timestamp rotation changed identity")
	}
	data["gemini_web_cookie_generation"] = "recovered"
	_, recovered := credential(data)
	if recovered == id {
		t.Fatal("recovery generation did not change identity")
	}
	data["gemini_web_cookie"] = "__Secure-1PSIDTS=invalid"
	_, invalid := credential(data)
	if invalid != "" {
		t.Fatal("missing principal accepted")
	}
}

func TestInputAndErrorsNeverExposeSecrets(t *testing.T) {
	for _, input := range []string{`{"DSN":"secret-password","RunID":"../secret"}`, `{"DSN":"secret-password","RunID":"safe","Workers":5}`, `{"DSN":"secret-password","RunID":"safe","unexpected":"token"}`, `{"DSN":"secret-password","RunID":"safe"} true`} {
		_, err := readInput(strings.NewReader(input))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad input result: %v", err)
		}
	}
	cfg, err := readInput(strings.NewReader(`{"DSN":"secret-password","RunID":"run_20261021"}`))
	if err != nil || cfg.Workers != 2 {
		t.Fatal("valid input rejected")
	}
	if got := safeCode(&url.Error{Op: "POST", URL: "https://private.example/?token=secret", Err: errors.New("private payload")}); got != "request_failed" {
		t.Fatalf("unexpected code %s", got)
	}
	if got := safeCode(&geminiweb.UpstreamRPCError{RPCCode: 8, BardCode: 1041}); got != "rpc_8_bard_1041" {
		t.Fatalf("unexpected RPC code %s", got)
	}
}

func TestCountsOnlyConfirmSnapshotTargets(t *testing.T) {
	res := result{Submitted: 2}
	updateCounts(&res, []string{"c_old_1", "c_old_2"}, []string{"c_old_2", "c_new"})
	if res.Deleted != 1 || res.Remaining != 1 || res.NewChats != 1 || res.Submitted != 2 {
		t.Fatalf("incorrect verification counts: %+v", res)
	}
}

// This integration test accepts only an explicitly supplied localhost test DB.
// It creates an isolated schema, never truncates an existing application table.
func TestManualClaimAndFenceIntegration(t *testing.T) {
	dsn := os.Getenv("GEMINI_HISTORY_MANUAL_TEST_DSN")
	if dsn == "" {
		t.Skip("set GEMINI_HISTORY_MANUAL_TEST_DSN to a disposable local database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || !strings.Contains(u.Path, "test") {
		t.Fatal("integration DSN must use a localhost test database")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := "gemini_manual_test_" + time.Now().UTC().Format("20060102150405000000000")
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	isolated, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { isolated.Close() })
	ddl := `CREATE TABLE accounts(id BIGINT PRIMARY KEY,type TEXT,status TEXT,credentials JSONB,proxy_id BIGINT,deleted_at TIMESTAMPTZ);
 CREATE TABLE proxies(id BIGINT PRIMARY KEY,protocol TEXT,host TEXT,port INTEGER,username TEXT,password TEXT,deleted_at TIMESTAMPTZ);`
	if _, err = isolated.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/287_gemini_web_history_cleanup.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = isolated.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = isolated.Exec(`INSERT INTO accounts VALUES(1,'gemini_web','disabled','{"gemini_web_cookie":"__Secure-1PSID=principal; __Secure-1PSIDTS=timestamp"}',NULL,NULL),(2,'gemini_web','active','{}',NULL,NOW())`); err != nil {
		t.Fatal(err)
	}
	r := &runner{db: isolated, runID: "test", out: os.Stdout}
	ctx := context.Background()
	j, a, err := r.claim(ctx, 1)
	if err != nil || j.id == 0 || a.identity == "" {
		t.Fatalf("disabled account claim failed: %v", err)
	}
	var windows, status string
	var events int
	if err = isolated.QueryRow(`SELECT windows::text,status FROM gemini_web_history_cleanup_jobs WHERE id=$1`, j.id).Scan(&windows, &status); err != nil {
		t.Fatal(err)
	}
	if err = isolated.QueryRow(`SELECT count(*) FROM gemini_web_history_cleanup_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if windows != "[]" || status != "running" || events != 0 {
		t.Fatal("manual action fabricated quota events")
	}
	if _, _, err = r.claim(ctx, 1); !errors.Is(err, errAccountBusy) {
		t.Fatal("live cleanup did not gate duplicate claim")
	}
	// The application's generation admission uses this exact account lock and
	// running-job predicate. It must observe our manual job in the same tx.
	tx, err := isolated.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, accountLock, 1); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_web_history_cleanup_jobs WHERE account_id=$1 AND status='running' AND lease_until>clock_timestamp())`, 1).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if !blocked {
		t.Fatal("manual job did not block generation admission")
	}
	if _, err = isolated.Exec(`INSERT INTO gemini_web_history_requests VALUES(1,'request',NOW()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	active, err := r.activeRequests(ctx, j)
	if err != nil || !active {
		t.Fatal("existing generation not detected")
	}
	if err = r.renew(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err = isolated.Exec(`UPDATE gemini_web_history_cleanup_jobs SET lease_until=NOW()-interval '1 second' WHERE id=$1`, j.id); err != nil {
		t.Fatal(err)
	}
	if err = r.renew(ctx, j); !errors.Is(err, errLeaseLost) {
		t.Fatal("expired lease revived")
	}
	if _, err = r.activeRequests(ctx, j); !errors.Is(err, errLeaseLost) {
		t.Fatal("expired owner checked requests")
	}
	if _, _, err = r.claim(ctx, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("soft-deleted account admitted")
	}
}
