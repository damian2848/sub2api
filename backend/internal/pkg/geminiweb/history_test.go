package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func historyRPCFixture(rpc string, payload any) string {
	encoded, _ := json.Marshal(payload)
	frame, _ := json.Marshal([]any{[]any{"wrb.fr", rpc, string(encoded), nil, nil, nil, "generic"}})
	return ")]}'\n\n" + fmt.Sprint(len(frame)) + "\n" + string(frame) + "\n25\n[[\"e\",4,null,null,139]]\n"
}

func historyPageFixture(next any, ids ...string) []any {
	chats := make([]any, 0, len(ids))
	for _, id := range ids {
		chats = append(chats, []any{id, "PRIVATE_TITLE_DO_NOT_LOG", false, nil, nil, []any{1700000000, 0}})
	}
	return []any{nil, next, chats}
}

func TestHistoryInventoryAndDeletionUseAccountSession(t *testing.T) {
	bootstraps, posts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Goog-AuthUser") != "0" || req.Header.Get("User-Agent") != "history-fixture-agent" {
			t.Error("request lost explicit account or user agent")
		}
		if req.Method == http.MethodGet {
			bootstraps++
			if req.URL.Path != "/u/0/app" {
				t.Error("wrong account bootstrap path")
			}
			_, _ = io.WriteString(w, `"SNlM0e":"fixture-token" "cfb2h":"fixture-build" "FdrFJe":"fixture-session" "TuX5cc":"en"`)
			return
		}
		posts++
		if req.Method != http.MethodPost || req.URL.Path != "/u/0/_/BardChatUi/data/batchexecute" || req.URL.Query().Get("source-path") != "/u/0/app" {
			t.Error("wrong history RPC route")
		}
		if req.URL.Query().Get("bl") != "fixture-build" || req.URL.Query().Get("f.sid") != "fixture-session" {
			t.Error("lost bootstrap parameters")
		}
		if err := req.ParseForm(); err != nil {
			t.Error(err)
			http.Error(w, "bad form", 400)
			return
		}
		if req.PostForm.Get("at") != "fixture-token" {
			t.Error("lost session token")
		}
		var envelope [][][]json.RawMessage
		if json.Unmarshal([]byte(req.PostForm.Get("f.req")), &envelope) != nil || len(envelope) != 1 || len(envelope[0]) != 1 || len(envelope[0][0]) != 4 {
			t.Error("invalid batch envelope")
			http.Error(w, "bad envelope", 400)
			return
		}
		var rpc, argString string
		_ = json.Unmarshal(envelope[0][0][0], &rpc)
		_ = json.Unmarshal(envelope[0][0][1], &argString)
		if rpc != req.URL.Query().Get("rpcids") {
			t.Error("RPC query/envelope mismatch")
		}
		var args []any
		if json.Unmarshal([]byte(argString), &args) != nil {
			t.Error("invalid arguments")
			return
		}
		var modelHeader []any
		if json.Unmarshal([]byte(req.Header.Get("x-goog-ext-525001261-jspb")), &modelHeader) != nil || len(modelHeader) != 17 || modelHeader[4] != nil || modelHeader[11] != nil {
			t.Error("history RPC should use a neutral model header")
		}
		switch posts {
		case 1:
			if !reflect.DeepEqual(args, []any{float64(100), nil, []any{float64(0), nil, float64(1)}}) {
				t.Errorf("unexpected first page args: %#v", args)
			}
			w.Header().Add("Set-Cookie", "__Secure-1PSIDTS=rotated-on-list; Path=/; Secure")
			_, _ = io.WriteString(w, historyRPCFixture(rpc, historyPageFixture("opaque+cursor/=", "c_web", "c_api")))
		case 2:
			if args[1] != "opaque+cursor/=" {
				t.Error("opaque pagination cursor was not preserved")
			}
			if !strings.Contains(req.Header.Get("Cookie"), "__Secure-1PSIDTS=rotated-on-list") {
				t.Error("second page lost rotated cookie")
			}
			_, _ = io.WriteString(w, historyRPCFixture(rpc, historyPageFixture(nil, "c_api", "c_older")))
		case 3:
			if !reflect.DeepEqual(args, []any{float64(100), nil, []any{float64(1), nil, float64(1)}}) {
				t.Error("pinned filter did not start with a fresh cursor")
			}
			_, _ = io.WriteString(w, historyRPCFixture(rpc, historyPageFixture("", "c_pinned", "c_web")))
		case 4:
			if rpc != geminiWebDeleteConversationRPC || !reflect.DeepEqual(args, []any{"c_web"}) {
				t.Error("wrong deletion arguments")
			}
			w.Header().Add("Set-Cookie", "__Secure-1PSIDTS=rotated-on-delete; Path=/; Secure")
			_, _ = io.WriteString(w, historyRPCFixture(rpc, []any{}))
		default:
			t.Error("unexpected extra RPC")
		}
	}))
	defer server.Close()
	client := NewClient()
	cred := Credential{Cookie: "Cookie: __Secure-1PSID=fixture; __Secure-1PSIDTS=original", AuthUser: 0, AuthUserSet: true, UserAgent: "history-fixture-agent", BaseURL: server.URL, CacheGeneration: "recovered"}
	got, err := client.ListConversations(context.Background(), cred, server.Client().Do)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ConversationIDs, []string{"c_web", "c_api", "c_older", "c_pinned"}) {
		t.Fatalf("unexpected deduplicated inventory: %#v", got.ConversationIDs)
	}
	if !strings.Contains(got.UpdatedCookie, "rotated-on-list") {
		t.Fatal("inventory did not return rotated cookie")
	}
	// Deliberately pass the original credential: the client must reuse its latest session cookie.
	cookie, err := client.DeleteConversation(context.Background(), cred, "c_web", server.Client().Do)
	if err != nil || !strings.Contains(cookie, "rotated-on-delete") {
		t.Fatalf("delete result: error=%v", err)
	}
	if bootstraps != 1 || posts != 4 {
		t.Fatalf("calls: bootstrap=%d, RPC=%d", bootstraps, posts)
	}
	if client.CurrentCookie(cred) != cookie {
		t.Fatal("client lost latest cookie")
	}
}

func TestHistoryPaginationFailureReturnsNoPartialInventory(t *testing.T) {
	for _, scenario := range []string{"repeat", "cap", "second_filter_error", "malformed_page"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(200, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				posts++
				next := any(nil)
				switch scenario {
				case "repeat":
					next = "same-cursor"
				case "cap":
					next = fmt.Sprint(posts)
				case "second_filter_error":
					if posts == 2 {
						return response(200, strings.Replace(bard1095Frame, `"wrb.fr",null`, `"wrb.fr","MaZiqc"`, 1), nil), nil
					}
				case "malformed_page":
					if posts == 2 {
						return response(200, historyRPCFixture(geminiWebListConversationsRPC, []any{nil, nil, nil}), nil), nil
					}
					next = "next"
				}
				return response(200, historyRPCFixture(geminiWebListConversationsRPC, historyPageFixture(next, "c_first")), http.Header{"Set-Cookie": {"__Secure-1PSIDTS=latest; Path=/"}}), nil
			}
			got, err := NewClient().ListConversations(context.Background(), Credential{Cookie: "__Secure-1PSID=fixture", BaseURL: "https://gemini.test"}, do)
			if err == nil || got.ConversationIDs != nil {
				t.Fatalf("incomplete scan must fail without a partial inventory, err=%v ids=%v", err, got.ConversationIDs)
			}
			if !strings.Contains(got.UpdatedCookie, "latest") {
				t.Fatal("failure lost rotated cookie")
			}
			if scenario == "repeat" && posts != 2 {
				t.Fatalf("repeated cursor not bounded: %d", posts)
			}
			if scenario == "cap" && posts != historyMaxPagesPerFilter {
				t.Fatalf("page cap failed: %d", posts)
			}
		})
	}
}

func TestDecodeBatchRPCFailsClosed(t *testing.T) {
	good := historyRPCFixture(geminiWebListConversationsRPC, historyPageFixture(nil))
	tests := map[string]string{
		"empty": "", "html": "<html>PRIVATE</html>", "wrong RPC": historyRPCFixture("unknown", historyPageFixture(nil)),
		"missing RPC": `[["di",1],["e",4,null,null,139]]`, "truncated": good + `[["wrb.fr"`,
		"invalid outer": `[["wrb.fr","MaZiqc",]]`, "garbage tail": good + "PRIVATE_TRAILING_DATA",
		"invalid inner": `[["wrb.fr","MaZiqc","not json"]]`, "unknown row": `[["unexpected",1]]` + good,
		"nonstring payload": `[["wrb.fr","MaZiqc",[null,null,[]]]]`, "malformed status": `[["wrb.fr","MaZiqc","[null,null,[]]",null,null,["bad"]]]`,
		"null status code": `[["wrb.fr","MaZiqc","[null,null,[]]",null,null,[null]]]`,
		"duplicate RPC":    good + strings.TrimPrefix(good, ")]}'\n\n"), "trailing length": good + "99",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBatchRPC([]byte(body), geminiWebListConversationsRPC); err == nil {
				t.Fatal("accepted ambiguous response")
			}
		})
	}
	for _, code := range []int{7, 8, 16} {
		frame := fmt.Sprintf(`[["wrb.fr","MaZiqc",null,null,null,[%d,"PRIVATE_STATUS",[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1095]]]]]]`, code)
		_, err := decodeBatchRPC([]byte(frame), geminiWebListConversationsRPC)
		var rpc *UpstreamRPCError
		if !errors.As(err, &rpc) || rpc.RPCCode != code || rpc.BardCode != 1095 {
			t.Fatalf("lost structured error: %v", err)
		}
		if strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("error leaked upstream data")
		}
	}
	if _, err := decodeBatchRPC([]byte(good), geminiWebListConversationsRPC); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeHistoryPageRejectsAmbiguousLists(t *testing.T) {
	tests := []any{nil, []any{nil}, []any{nil, nil, nil}, []any{nil, 123, []any{}}, []any{nil, nil, []any{"bad"}}, []any{nil, nil, []any{[]any{"not_a_conversation", "title"}}}, []any{nil, nil, []any{[]any{"c_valid"}}}, []any{nil, strings.Repeat("x", historyMaxCursorBytes+1), []any{}}}
	tooMany := make([]any, historyPageSize+1)
	for i := range tooMany {
		tooMany[i] = []any{fmt.Sprint("c_", i), "title"}
	}
	tests = append(tests, []any{nil, nil, tooMany})
	for i, payload := range tests {
		encoded, _ := json.Marshal(payload)
		if _, _, err := decodeHistoryPage(encoded); err == nil {
			t.Fatalf("accepted ambiguous list case %d", i)
		}
	}
	encoded, _ := json.Marshal(historyPageFixture(nil))
	ids, next, err := decodeHistoryPage(encoded)
	if err != nil || ids == nil || len(ids) != 0 || next != "" {
		t.Fatalf("explicit empty list: %v %v %v", ids, next, err)
	}
	ids, next, err = decodeHistoryPage(json.RawMessage("[]"))
	if err != nil || ids == nil || len(ids) != 0 || next != "" {
		t.Fatalf("live empty JSPB list: %v %v %v", ids, next, err)
	}
}

func TestHistoryHTTPAndAcknowledgmentValidation(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", 401, "PRIVATE", ErrInvalidCredential}, {"forbidden", 403, "PRIVATE", ErrInvalidCredential}, {"rate limited", 429, "PRIVATE", ErrRateLimited},
		{"redirect", 302, "PRIVATE", nil}, {"service", 503, "PRIVATE", nil}, {"oversize", 200, strings.Repeat(" ", historyMaxResponseBytes+1), nil},
		{"wrong ack", 200, historyRPCFixture(geminiWebDeleteConversationRPC, []any{true}), nil},
		{"RPC error", 200, strings.Replace(bard1095Frame, `"wrb.fr",null`, `"wrb.fr","GzXR5e"`, 1), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(200, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				return response(tc.status, tc.body, http.Header{"Set-Cookie": {"__Secure-1PSIDTS=updated; Path=/"}}), nil
			}
			cookie, err := NewClient().DeleteConversation(context.Background(), Credential{Cookie: "__Secure-1PSID=fixture", BaseURL: "https://gemini.test"}, "c_valid", do)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(cookie, "updated") || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("cookie propagation or error redaction failed")
			}
		})
	}
	for _, payload := range []any{nil, []any{}, []any{nil}} {
		t.Run(fmt.Sprintf("empty ack %v", payload), func(t *testing.T) {
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(200, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				return response(200, historyRPCFixture(geminiWebDeleteConversationRPC, payload), nil), nil
			}
			if _, err := NewClient().DeleteConversation(context.Background(), Credential{Cookie: "__Secure-1PSID=fixture", BaseURL: "https://gemini.test"}, "c_valid", do); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoryDoesNotSendInvalidOrCanceledRequests(t *testing.T) {
	called := false
	do := func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("PRIVATE_TRANSPORT")
	}
	for _, id := range []string{"", "c_", "other", "c_x/other", "c_x\n", strings.Repeat("c", 131)} {
		if _, err := NewClient().DeleteConversation(context.Background(), Credential{Cookie: "__Secure-1PSID=fixture"}, id, do); err == nil {
			t.Fatal("accepted invalid ID")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewClient().ListConversations(ctx, Credential{Cookie: "__Secure-1PSID=fixture"}, do); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if called {
		t.Fatal("invalid/canceled request reached transport")
	}
	_, err := NewClient().ListConversations(context.Background(), Credential{Cookie: "__Secure-1PSID=fixture"}, do)
	if err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("transport error was not redacted")
	}
}

func TestQuotaDecoderRequiresMatchingCompleteRPC(t *testing.T) {
	payload := []any{2, []any{[]any{nil, 1, 1, nil}}}
	good := historyRPCFixture(geminiWebQuotaRPC, payload)
	if q, err := decodeQuota([]byte(good), time.Now()); err != nil || len(q.Windows) != 1 || q.Windows[0].UsedPercent != 100 {
		t.Fatalf("valid quota: %#v %v", q, err)
	}
	for name, body := range map[string]string{
		"wrong RPC":           historyRPCFixture(geminiWebListConversationsRPC, payload),
		"truncated tail":      good + `[["wrb.fr"`,
		"RPC error":           strings.Replace(bard1095Frame, `"wrb.fr",null`, `"wrb.fr","jSf9Qc"`, 1),
		"error after success": good + strings.TrimPrefix(strings.Replace(bard1095Frame, `"wrb.fr",null`, `"wrb.fr","jSf9Qc"`, 1), ")]}'"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeQuota([]byte(body), time.Now()); err == nil {
				t.Fatal("ambiguous quota could authorize cleanup")
			}
		})
	}
}
