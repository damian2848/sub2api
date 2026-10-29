// gemini-history-cleanup is an explicitly invoked, one-time operator action.
// It never manufactures quota observations or changes account credentials.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	_ "github.com/lib/pq"
)

const accountLock = `SELECT pg_advisory_xact_lock(hashtextextended('gemini-web-history:' || $1::text, 0))`
const jobFence = ` WHERE id=$1 AND account_id=$2 AND lease_token=$3 AND status='running' AND lease_until>clock_timestamp()`
const accountSQL = `SELECT a.credentials,a.proxy_id,p.id,COALESCE(p.protocol,''),COALESCE(p.host,''),COALESCE(p.port,0),COALESCE(p.username,''),COALESCE(p.password,'')
 FROM accounts a LEFT JOIN proxies p ON p.id=a.proxy_id AND p.deleted_at IS NULL
 WHERE a.id=$1 AND a.type='gemini_web' AND a.deleted_at IS NULL`

var errLeaseLost = errors.New("lease_lost")
var errAccountChanged = errors.New("account_changed")
var errAccountBusy = errors.New("account_busy")
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type input struct {
	DSN     string
	RunID   string
	Workers int
}

type account struct {
	id       int64
	identity string
	cred     geminiweb.Credential
	proxyURL string
}

type job struct {
	id, accountID int64
	token         string
}

// Deliberately contains no names, conversation IDs, upstream text or credentials.
type result struct {
	Event     string `json:"event"`
	RunID     string `json:"run_id"`
	AccountID int64  `json:"account_id,omitempty"`
	JobID     int64  `json:"job_id,omitempty"`
	Status    string `json:"status,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Code      string `json:"error_code,omitempty"`
	Targets   int    `json:"targets"`
	Deleted   int    `json:"deleted"`
	Remaining int    `json:"remaining"`
	NewChats  int    `json:"new_chats"`
	Submitted int    `json:"submitted"`
}

type runner struct {
	db    *sql.DB
	runID string
	mu    sync.Mutex
	out   io.Writer
}

func (r *runner) emit(v any) { r.mu.Lock(); defer r.mu.Unlock(); _ = json.NewEncoder(r.out).Encode(v) }

func readInput(in io.Reader) (input, error) {
	var cfg input
	dec := json.NewDecoder(io.LimitReader(in, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&cfg) != nil || strings.TrimSpace(cfg.DSN) == "" || !runIDPattern.MatchString(cfg.RunID) {
		return input{}, errors.New("invalid_input")
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return input{}, errors.New("invalid_input")
	}
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	if cfg.Workers < 1 || cfg.Workers > 4 {
		return input{}, errors.New("invalid_input")
	}
	return cfg, nil
}

func main() { os.Exit(run()) }

func run() int {
	cfg, err := readInput(os.Stdin)
	if err != nil {
		fmt.Println(`{"event":"fatal","error_code":"invalid_input"}`)
		return 1
	}
	r := &runner{runID: cfg.RunID, out: os.Stdout}
	fatal := func(code string) int {
		r.emit(map[string]any{"event": "fatal", "run_id": cfg.RunID, "error_code": code})
		return 1
	}
	db, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		return fatal("database_open_failed")
	}
	r.db = db
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(8)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	pingCancel()
	if err != nil {
		return fatal("database_unavailable")
	}
	// Keep a connection solely for this session lock. Closing it releases the
	// lock even if the process is killed. This protects concurrent invocations.
	lockConn, err := db.Conn(ctx)
	if err != nil {
		return fatal("run_lock_failed")
	}
	defer func() { _ = lockConn.Close() }()
	var locked bool
	lockCtx, lockCancel := context.WithTimeout(ctx, 5*time.Second)
	err = lockConn.QueryRowContext(lockCtx, `SELECT pg_try_advisory_lock(hashtextextended('gemini-web-manual-run:' || $1,0))`, cfg.RunID).Scan(&locked)
	lockCancel()
	if err != nil || !locked {
		return fatal("run_already_active")
	}
	listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
	rows, err := db.QueryContext(listCtx, `SELECT id FROM accounts WHERE type='gemini_web' AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		listCancel()
		return fatal("account_list_failed")
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	listCancel()
	if err != nil {
		return fatal("account_list_failed")
	}
	r.emit(map[string]any{"event": "started", "run_id": cfg.RunID, "accounts": len(ids), "workers": cfg.Workers, "scope": "all_nondeleted_gemini_web_including_disabled"})
	queue := make(chan int64)
	results := make(chan result, len(ids))
	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range queue {
				res := r.cleanAccount(ctx, id)
				results <- res
				r.emit(res)
			}
		}()
	}
	for _, id := range ids {
		queue <- id
	}
	close(queue)
	wg.Wait()
	close(results)
	completed, failed, deleted := 0, 0, 0
	for res := range results {
		if res.Status == "completed" {
			completed++
		} else {
			failed++
		}
		deleted += res.Deleted
	}
	r.emit(map[string]any{"event": "summary", "run_id": cfg.RunID, "accounts": len(ids), "completed": completed, "failed": failed, "deleted": deleted})
	if failed > 0 {
		return 2
	}
	return 0
}

func credentialString(data map[string]any, key string) string {
	switch v := data[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

func credential(data map[string]any) (geminiweb.Credential, string) {
	cred := geminiweb.Credential{Cookie: geminiweb.NormalizeCookieHeader(credentialString(data, "gemini_web_cookie")), CacheGeneration: credentialString(data, "gemini_web_cookie_generation"), UserAgent: credentialString(data, "user_agent"), BaseURL: "https://gemini.google.com"}
	if n, err := strconv.Atoi(strings.TrimSpace(credentialString(data, "auth_user"))); err == nil && n >= 0 {
		cred.AuthUser = n
		cred.AuthUserSet = true
	}
	var psid string
	for _, part := range strings.Split(cred.Cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == "__Secure-1PSID" {
			psid = strings.TrimSpace(kv[1])
		}
	}
	if psid == "" {
		return cred, ""
	}
	payload, _ := json.Marshal([]any{psid, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration})
	digest := sha256.Sum256(payload)
	return cred, hex.EncodeToString(digest[:])
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadAccount(ctx context.Context, q querier, id int64) (account, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var a account
	a.id = id
	var raw []byte
	var wantedProxy, proxyID sql.NullInt64
	var scheme, host, user, password string
	var port int
	if err := q.QueryRowContext(ctx, accountSQL, id).Scan(&raw, &wantedProxy, &proxyID, &scheme, &host, &port, &user, &password); err != nil {
		return a, err
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return a, geminiweb.ErrInvalidCredential
	}
	a.cred, a.identity = credential(data)
	if a.identity == "" {
		return a, geminiweb.ErrInvalidCredential
	}
	if wantedProxy.Valid {
		if !proxyID.Valid || host == "" || port < 1 || port > 65535 {
			return a, errors.New("proxy_unavailable")
		}
		switch scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return a, errors.New("proxy_unavailable")
		}
		u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port))}
		if user != "" && password != "" {
			u.User = url.UserPassword(user, password)
		}
		a.proxyURL = u.String()
	}
	return a, nil
}

func (r *runner) claim(ctx context.Context, id int64) (job, account, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var j job
	var a account
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return j, a, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, accountLock, id); err != nil {
		return j, a, err
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_web_history_cleanup_jobs WHERE account_id=$1 AND status='running' AND lease_until>clock_timestamp())`, id).Scan(&active); err != nil {
		return j, a, err
	}
	if active {
		return j, a, errAccountBusy
	}
	a, err = loadAccount(ctx, tx, id)
	if err != nil {
		return j, a, err
	}
	var entropy [24]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return j, a, err
	}
	j.accountID = id
	j.token = hex.EncodeToString(entropy[:])
	err = tx.QueryRowContext(ctx, `INSERT INTO gemini_web_history_cleanup_jobs(account_id,identity,windows,status,attempts,lease_token,lease_until,last_error_code) VALUES($1,$2,'[]'::jsonb,'running',1,$3,clock_timestamp()+interval '90 seconds','manual_cleanup') RETURNING id`, id, a.identity, j.token).Scan(&j.id)
	if err != nil {
		return j, a, err
	}
	err = tx.Commit()
	return j, a, err
}

func (r *runner) write(ctx context.Context, j job, query string, extra ...any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := append([]any{j.id, j.accountID, j.token}, extra...)
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errLeaseLost
	}
	return nil
}

func (r *runner) renew(ctx context.Context, j job) error {
	return r.write(ctx, j, `UPDATE gemini_web_history_cleanup_jobs SET lease_until=clock_timestamp()+interval '90 seconds',updated_at=clock_timestamp()`+jobFence)
}

func (r *runner) activeRequests(ctx context.Context, j job) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var active bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_web_history_requests WHERE account_id=$2 AND expires_at>clock_timestamp()) FROM gemini_web_history_cleanup_jobs`+jobFence, j.id, j.accountID, j.token).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		err = errLeaseLost
	}
	return active, err
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func safeCode(err error) string {
	var rpc *geminiweb.UpstreamRPCError
	var status *geminiweb.UpstreamHTTPError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errLeaseLost):
		return "lease_lost"
	case errors.Is(err, errAccountChanged):
		return "account_changed"
	case errors.Is(err, errAccountBusy):
		return "account_busy"
	case errors.Is(err, sql.ErrNoRows):
		return "account_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &rpc):
		return fmt.Sprintf("rpc_%d_bard_%d", rpc.RPCCode, rpc.BardCode)
	case errors.As(err, &status):
		return fmt.Sprintf("http_%d", status.StatusCode)
	case errors.Is(err, geminiweb.ErrInvalidCredential):
		return "invalid_credential"
	case errors.Is(err, geminiweb.ErrRateLimited):
		return "rate_limited"
	default:
		return "request_failed"
	}
}

func (r *runner) cleanAccount(parent context.Context, id int64) (res result) {
	res = result{Event: "account_finished", RunID: r.runID, AccountID: id, Status: "failed", Stage: "claim"}
	ctx, cancel := context.WithTimeout(parent, 20*time.Minute)
	defer cancel()
	var j job
	var a account
	var err error
	claimDeadline := time.Now().Add(7 * time.Minute)
	for {
		j, a, err = r.claim(ctx, id)
		if !errors.Is(err, errAccountBusy) || time.Now().After(claimDeadline) {
			break
		}
		if !sleep(ctx, 5*time.Second) {
			err = ctx.Err()
			break
		}
	}
	if err != nil {
		res.Code = safeCode(err)
		return
	}
	res.JobID = j.id
	r.emit(result{Event: "account_started", RunID: r.runID, AccountID: id, JobID: j.id, Status: "running"})
	ctx, abort := context.WithCancelCause(ctx)
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r.renew(ctx, j) != nil {
					abort(errLeaseLost)
					return
				}
			}
		}
	}()
	defer func() {
		// Use a separate bounded context for completion/cancellation even after
		// SIGTERM. The fence prohibits reviving or overwriting a lost lease.
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finishCancel()
		var finishErr error
		if res.Status == "completed" {
			finishErr = r.write(finishCtx, j, `UPDATE gemini_web_history_cleanup_jobs SET status='completed',snapshot_ids=NULL,lease_token=NULL,lease_until=NULL,last_error_code='',completed_at=clock_timestamp(),updated_at=clock_timestamp()`+jobFence+` AND snapshot_ids IS NOT NULL`)
		} else {
			finishErr = r.write(finishCtx, j, `UPDATE gemini_web_history_cleanup_jobs SET status='cancelled',snapshot_ids=NULL,lease_token=NULL,lease_until=NULL,last_error_code=$4,completed_at=clock_timestamp(),updated_at=clock_timestamp()`+jobFence, "manual_"+res.Code)
		}
		abort(nil)
		heartbeat.Wait()
		if finishErr != nil {
			res.Status = "failed"
			res.Code = "job_finalize_failed"
		}
	}()
	check := func() error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		latest, err := loadAccount(ctx, r.db, id)
		if err != nil {
			return err
		}
		if latest.identity != a.identity || latest.proxyURL != a.proxyURL {
			return errAccountChanged
		}
		return r.renew(ctx, j)
	}
	res.Stage = "drain"
	for {
		if err = check(); err != nil {
			res.Code = safeCode(err)
			return
		}
		var active bool
		active, err = r.activeRequests(ctx, j)
		if err != nil {
			res.Code = safeCode(err)
			return
		}
		if !active {
			break
		}
		if !sleep(ctx, 2*time.Second) {
			res.Code = safeCode(context.Cause(ctx))
			return
		}
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		res.Code = "request_failed"
		return
	}
	transport := defaultTransport.Clone()
	transport.Proxy = nil
	if a.proxyURL != "" {
		proxy, _ := url.Parse(a.proxyURL)
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	client := geminiweb.NewClient()
	list := func() ([]string, error) {
		if err := check(); err != nil {
			return nil, err
		}
		got, err := client.ListConversations(ctx, a.cred, httpClient.Do)
		if got.UpdatedCookie != "" {
			a.cred.Cookie = got.UpdatedCookie
		}
		return got.ConversationIDs, err
	}
	var snapshot []string
	snapshotReady := false
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !sleep(ctx, []time.Duration{0, 10 * time.Second, 30 * time.Second}[attempt]) {
			res.Code = safeCode(context.Cause(ctx))
			return
		}
		res.Stage = "list"
		var current []string
		current, err = list()
		if err != nil {
			res.Code = safeCode(err)
			continue
		}
		if !snapshotReady {
			snapshot = append([]string{}, current...)
			payload, _ := json.Marshal(snapshot)
			res.Targets = len(snapshot)
			if err = r.write(ctx, j, `UPDATE gemini_web_history_cleanup_jobs SET snapshot_ids=$4::jsonb,conversation_count=jsonb_array_length($4::jsonb),updated_at=clock_timestamp()`+jobFence+` AND snapshot_ids IS NULL`, string(payload)); err != nil {
				res.Code = safeCode(err)
				return
			}
			snapshotReady = true
		}
		present := stringSet(current)
		updateCounts(&res, snapshot, current)
		res.Stage = "delete"
		for _, cid := range snapshot {
			if !present[cid] {
				continue
			}
			if err = check(); err != nil {
				res.Code = safeCode(err)
				return
			}
			var cookie string
			cookie, err = client.DeleteConversation(ctx, a.cred, cid, httpClient.Do)
			res.Submitted++
			if cookie != "" {
				a.cred.Cookie = cookie
			}
			if err != nil {
				deleteErr := err
				current, err = list()
				if err != nil {
					res.Code = safeCode(deleteErr)
					break
				}
				present = stringSet(current)
				updateCounts(&res, snapshot, current)
				if present[cid] {
					err = deleteErr
					res.Code = safeCode(err)
					break
				}
			}
		}
		if err != nil {
			continue
		}
		res.Stage = "verify"
		current, err = list()
		if err != nil {
			res.Code = safeCode(err)
			continue
		}
		updateCounts(&res, snapshot, current)
		if res.Remaining == 0 {
			res.Status = "completed"
			res.Stage = "verified"
			res.Code = ""
			return
		}
		res.Code = "history_not_empty"
	}
	if res.Code == "" {
		res.Code = "request_failed"
	}
	return
}

func stringSet(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

func updateCounts(res *result, snapshot, current []string) {
	present, targets := stringSet(current), stringSet(snapshot)
	res.Deleted, res.Remaining, res.NewChats = 0, 0, 0
	for id := range targets {
		if present[id] {
			res.Remaining++
		} else {
			res.Deleted++
		}
	}
	for id := range present {
		if !targets[id] {
			res.NewChats++
		}
	}
}
