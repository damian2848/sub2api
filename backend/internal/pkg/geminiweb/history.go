package geminiweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	geminiWebListConversationsRPC  = "MaZiqc"
	geminiWebDeleteConversationRPC = "GzXR5e"
	historyPageSize                = 100
	historyMaxPagesPerFilter       = 100
	historyMaxResponseBytes        = 4 << 20
	historyMaxCursorBytes          = 64 << 10
)

var errHistoryProtocol = errors.New("gemini web history response has an unsupported or malformed protocol")

// HistoryResult contains the complete, deduplicated pinned and recent chat
// inventory, including chats created in the website. On failure no partial
// inventory is returned. Temporary chats are not exposed by Google's list RPC.
type HistoryResult struct {
	ConversationIDs []string
	UpdatedCookie   string
}

// ListConversations fetches fresh pages for both sidebar filters. A pagination
// cap or an invalid response is an error, never an apparently complete list.
func (c *Client) ListConversations(ctx context.Context, cred Credential, do Doer) (HistoryResult, error) {
	result := HistoryResult{}
	ids := make([]string, 0)
	seenIDs := make(map[string]bool)
	for _, filter := range []int{0, 1} {
		var cursor any
		seenCursors := make(map[string]bool)
		for page := 0; ; page++ {
			if page >= historyMaxPagesPerFilter {
				return result, errors.New("gemini web history pagination limit exceeded")
			}
			payload, cookie, err := c.historyRPC(ctx, cred, geminiWebListConversationsRPC, []any{historyPageSize, cursor, []any{filter, nil, 1}}, do)
			if cookie != "" {
				result.UpdatedCookie, cred.Cookie = cookie, cookie
			}
			if err != nil {
				return result, err
			}
			pageIDs, next, err := decodeHistoryPage(payload)
			if err != nil {
				return result, err
			}
			for _, id := range pageIDs {
				if !seenIDs[id] {
					seenIDs[id] = true
					ids = append(ids, id)
				}
			}
			if next == "" {
				break
			}
			if seenCursors[next] {
				return result, errors.New("gemini web history pagination repeated a cursor")
			}
			seenCursors[next] = true
			cursor = next
		}
	}
	result.ConversationIDs = ids
	return result, nil
}

// DeleteConversation submits one deletion. An accepted RPC is not proof that
// Google's history has converged; callers must verify with ListConversations.
func (c *Client) DeleteConversation(ctx context.Context, cred Credential, conversationID string, do Doer) (string, error) {
	if !validHistoryConversationID(conversationID) {
		return "", errors.New("gemini web history conversation ID is invalid")
	}
	payload, cookie, err := c.historyRPC(ctx, cred, geminiWebDeleteConversationRPC, []any{conversationID}, do)
	if err != nil {
		return cookie, err
	}
	// The empty RPC response may be encoded as null or an empty JSPB message.
	// Unknown result shapes must not be reported as accepted deletions.
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return cookie, nil
	}
	var ack []json.RawMessage
	if json.Unmarshal(payload, &ack) != nil || len(ack) > 1 || (len(ack) == 1 && !isJSONNull(ack[0])) {
		return cookie, errHistoryProtocol
	}
	return cookie, nil
}

func validHistoryConversationID(id string) bool {
	if !strings.HasPrefix(id, "c_") || len(id) < 3 || len(id) > 130 {
		return false
	}
	for _, ch := range id[2:] {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= 'A' && ch <= 'Z') && !(ch >= '0' && ch <= '9') && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

// historyRequestError preserves cancellation/type classification without
// exposing transport URLs, response contents, or session tokens in logs.
type historyRequestError struct{ cause error }

func (e *historyRequestError) Error() string { return "gemini web history request failed" }
func (e *historyRequestError) Unwrap() error { return e.cause }

func (c *Client) historyRPC(ctx context.Context, cred Credential, rpcID string, args []any, do Doer) (json.RawMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	cred.Cookie = normalizeCookieHeader(cred.Cookie)
	if cred.Cookie == "" || cookieKey(cred.Cookie) == "" {
		return nil, "", ErrInvalidCredential
	}
	if do == nil {
		return nil, "", errors.New("gemini web transport is nil")
	}
	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if base == "" {
		base = "https://gemini.google.com"
	}
	sess, cookie, err := c.getSession(ctx, cred, base, do)
	if err != nil {
		return nil, cookie, &historyRequestError{cause: err}
	}
	postCred := cred
	if cookie != "" {
		postCred.Cookie = cookie
	}
	accountPath := ""
	if cred.hasAuthUser() {
		accountPath = "/u/" + strconv.Itoa(cred.AuthUser)
	}
	query := url.Values{
		"rpcids": {rpcID}, "source-path": {accountPath + "/app"},
		"hl": {defaultString(sess.Language, "en")}, "rt": {"c"},
		"_reqid": {strconv.FormatInt(time.Now().UnixNano()%1000000, 10)},
		"bl":     {sess.Build}, "f.sid": {sess.Fsid},
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, cookie, errHistoryProtocol
	}
	envelope, err := json.Marshal([]any{[]any{[]any{rpcID, string(argsJSON), nil, "generic"}}})
	if err != nil {
		return nil, cookie, errHistoryProtocol
	}
	form := url.Values{"at": {sess.At}, "f.req": {string(envelope)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+accountPath+"/_/BardChatUi/data/batchexecute?"+query.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, cookie, &historyRequestError{cause: err}
	}
	requestID := uuid()
	applyHeaders(req, postCred, base, sess, Model{}, requestID)
	req.Header.Set("Referer", base+accountPath+"/app")
	batchHeader := make([]any, 17)
	batchHeader[0], batchHeader[8], batchHeader[16] = 1, []int{4, 5, 6, 8}, requestID
	encodedHeader, _ := json.Marshal(batchHeader)
	req.Header.Set("x-goog-ext-525001261-jspb", string(encodedHeader))
	resp, err := do(req)
	if err != nil {
		return nil, cookie, &historyRequestError{cause: err}
	}
	if resp == nil || resp.Body == nil {
		return nil, cookie, errors.New("gemini web history returned no response body")
	}
	defer func() { _ = resp.Body.Close() }()
	cookie = mergeSetCookie(postCred.Cookie, resp.Header.Values("Set-Cookie"))
	c.updateSessionCookie(postCred, cookie)
	if live := c.currentSessionCookie(postCred); live != "" {
		cookie = live
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, cookie, ErrInvalidCredential
	case http.StatusTooManyRequests:
		return nil, cookie, ErrRateLimited
	default:
		return nil, cookie, &UpstreamHTTPError{StatusCode: resp.StatusCode, Operation: "history"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, historyMaxResponseBytes+1))
	if err != nil {
		return nil, cookie, &historyRequestError{cause: err}
	}
	if len(body) > historyMaxResponseBytes {
		return nil, cookie, errors.New("gemini web history response exceeded size limit")
	}
	payload, err := decodeBatchRPC(body, rpcID)
	return payload, cookie, err
}

func isJSONNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// Unlike the answer-stream parser this decoder rejects malformed/truncated
// frames and unrecognized records. A missing list must never mean no history.
func decodeBatchRPC(body []byte, rpcID string) (json.RawMessage, error) {
	rest := bytes.TrimSpace(body)
	rest = bytes.TrimPrefix(rest, []byte(")]}'"))
	var payload json.RawMessage
	matched := false
	for {
		rest = bytes.TrimSpace(rest)
		if len(rest) == 0 {
			break
		}
		// Length prefixes have used different units. Only their lexical shape
		// matters; balanced JSON determines the complete frame boundary.
		if rest[0] >= '0' && rest[0] <= '9' {
			i := 0
			for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
				i++
			}
			if i == len(rest) || (rest[i] != '\n' && rest[i] != '\r') {
				return nil, errHistoryProtocol
			}
			rest = bytes.TrimSpace(rest[i:])
		}
		if len(rest) == 0 || rest[0] != '[' {
			return nil, errHistoryProtocol
		}
		end := balanced(string(rest))
		if end < 0 {
			return nil, errHistoryProtocol
		}
		var rows []json.RawMessage
		if json.Unmarshal(rest[:end], &rows) != nil {
			return nil, errHistoryProtocol
		}
		rest = rest[end:]
		for _, row := range rows {
			var fields []json.RawMessage
			var kind string
			if json.Unmarshal(row, &fields) != nil || len(fields) < 1 || json.Unmarshal(fields[0], &kind) != nil {
				return nil, errHistoryProtocol
			}
			switch kind {
			case "di", "af.httprm", "e":
				continue
			case "wrb.fr":
			default:
				return nil, errHistoryProtocol
			}
			if len(fields) < 3 {
				return nil, errHistoryProtocol
			}
			if len(fields) > 5 && !isJSONNull(fields[5]) {
				if rpcErr := parseRPCError(fields[5]); rpcErr != nil {
					return nil, rpcErr
				}
				var status []json.RawMessage
				var code *int
				if json.Unmarshal(fields[5], &status) != nil || len(status) == 0 || json.Unmarshal(status[0], &code) != nil || code == nil || *code != 0 {
					return nil, errHistoryProtocol
				}
			}
			var responseRPC string
			if json.Unmarshal(fields[1], &responseRPC) != nil || responseRPC != rpcID || matched {
				return nil, errHistoryProtocol
			}
			matched = true
			if isJSONNull(fields[2]) {
				payload = json.RawMessage("null")
				continue
			}
			var encoded string
			if json.Unmarshal(fields[2], &encoded) != nil || !json.Valid([]byte(encoded)) {
				return nil, errHistoryProtocol
			}
			payload = json.RawMessage(encoded)
		}
	}
	if !matched {
		return nil, errHistoryProtocol
	}
	return payload, nil
}

func decodeHistoryPage(payload json.RawMessage) ([]string, string, error) {
	// Observed on the live MaZiqc pinned filter (2026-10-21): an account
	// with no pinned chats returns the empty JSPB message [], not [null,null,[]].
	// A null RPC payload remains invalid; only this explicit array is empty.
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[]")) {
		return []string{}, "", nil
	}
	var fields []json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || len(fields) < 3 {
		return nil, "", errHistoryProtocol
	}
	next := ""
	if !isJSONNull(fields[1]) && json.Unmarshal(fields[1], &next) != nil {
		return nil, "", errHistoryProtocol
	}
	if len(next) > historyMaxCursorBytes {
		return nil, "", errHistoryProtocol
	}
	var chats []json.RawMessage
	if isJSONNull(fields[2]) || json.Unmarshal(fields[2], &chats) != nil || len(chats) > historyPageSize {
		return nil, "", errHistoryProtocol
	}
	ids := make([]string, 0, len(chats))
	for _, chat := range chats {
		var row []json.RawMessage
		var id string
		if json.Unmarshal(chat, &row) != nil || len(row) < 2 || json.Unmarshal(row[0], &id) != nil || !validHistoryConversationID(id) {
			return nil, "", errHistoryProtocol
		}
		ids = append(ids, id)
	}
	return ids, next, nil
}
