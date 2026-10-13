//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func usageEvents() []CodexHarvestFlowEvent {
	var out []CodexHarvestFlowEvent
	for _, event := range listCodexHarvestFlowEvents() {
		if event.Kind == "ticket_used" {
			out = append(out, event)
		}
	}
	return out
}

func TestTicketUsageHTTPDispatch(t *testing.T) {
	for _, mode := range []string{"self", "reuse", "unknown", "account_test", "rejected", "strict", "unsent", "written_error", "replaced", "fast", "no_ticket"} {
		t.Run(mode, func(t *testing.T) {
			resetCodexHarvestFlow()
			t.Cleanup(resetCodexHarvestFlow)
			svc, account, ticket := accountTestTicketFixture(t)
			gateway := svc.openaiGatewayService
			account.Name = "consumer"
			ticket.Acquisition = "self"
			if mode == "reuse" {
				ticket.Acquisition, ticket.SourceAccountID, ticket.SourceAccountName = "reuse", 88, "donor"
			}
			if mode == "unknown" {
				ticket.Acquisition = ""
			}
			require.NoError(t, gateway.storeOpenAICodexTicket(t.Context(), account, ticket))
			if mode == "strict" {
				gateway.settingService = NewSettingService(&codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{SettingKeyOpenAICodexTicketStrict: "true"}}}, gateway.cfg)
			}
			gateway.httpUpstream = &accountTicketUpstream{do: func(req *http.Request, _ string) (*http.Response, error) {
				if mode == "unsent" {
					return nil, errors.New("dial failed")
				}
				if mode == "written_error" {
					httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
					return nil, io.ErrUnexpectedEOF
				}
				if mode == "fast" {
					req.Header.Set(openAICodexTurnStateHeader, ticket.State)
				}
				if mode == "replaced" {
					gateway.openaiCodexTickets.Delete(openAICodexTicketKey(account.ID, ticket.Model))
				}
				status := 200
				if mode == "rejected" {
					status = 401
				}
				headers := http.Header{}
				if mode == "strict" {
					headers.Set(openAICodexTurnStateHeader, "malformed")
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			}}
			body := []byte(`{"model":"gpt-6-astra","stream":true}`)
			req, err := http.NewRequestWithContext(t.Context(), "POST", chatgptCodexAPIURL, strings.NewReader(string(body)))
			require.NoError(t, err)
			before := time.Now()
			var response *http.Response
			if mode == "account_test" {
				response, err = svc.doOpenAIAccountTestUpstream(req, "", account, true, body)
			} else {
				if mode != "no_ticket" && mode != "fast" {
					require.NoError(t, gateway.applyOpenAICodexTicket(t.Context(), account, ticket.Model, req.Header, "sse"))
				}
				response, err = gateway.doOpenAIUpstream(req, "", account)
			}
			if mode == "unsent" || mode == "written_error" || mode == "strict" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if response != nil {
				require.NoError(t, response.Body.Close())
				require.NoError(t, response.Body.Close())
			}
			events := usageEvents()
			if mode == "unsent" || mode == "no_ticket" {
				require.Empty(t, events)
				return
			}
			require.Len(t, events, 1, "one dispatch must not duplicate on body close")
			e := events[0]
			require.Equal(t, account.ID, e.AccountID)
			require.Equal(t, account.Name, e.AccountName)
			require.Equal(t, ticket.Model, e.Model)
			require.WithinRange(t, e.At, before, time.Now())
			require.Equal(t, "sse", e.TicketUsage.Transport)
			require.Equal(t, ticket.Gateway, e.TicketUsage.Gateway)
			switch mode {
			case "reuse":
				require.Equal(t, "reuse", e.TicketUsage.Acquisition)
				require.EqualValues(t, 88, e.TicketUsage.SourceAccountID)
				require.Equal(t, "donor", e.TicketUsage.SourceAccountName)
			case "unknown":
				require.Equal(t, "unknown", e.TicketUsage.Acquisition)
				require.Zero(t, e.TicketUsage.SourceAccountID)
			default:
				require.Equal(t, "self", e.TicketUsage.Acquisition)
				require.Equal(t, account.ID, e.TicketUsage.SourceAccountID)
			}
			if mode == "account_test" {
				require.Equal(t, "account_test", e.TicketUsage.RequestKind)
			} else {
				require.Equal(t, "business", e.TicketUsage.RequestKind)
			}
			if mode == "rejected" {
				require.Equal(t, 401, e.HTTPStatus)
			}
			encoded, err := json.Marshal(e)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), ticket.State)
			require.NotContains(t, string(encoded), ticket.HarvestCookies[0])
			require.NotContains(t, string(encoded), ticket.HarvestProxyURL)
		})
	}
}

type usageFrameStub struct {
	openaiwsv2.FrameConn
	err error
}

func (c *usageFrameStub) WriteFrame(context.Context, coderws.MessageType, []byte) error { return c.err }

func TestTicketUsageWebSocketTurns(t *testing.T) {
	resetCodexHarvestFlow()
	t.Cleanup(resetCodexHarvestFlow)
	account := &Account{ID: 1, Name: "consumer"}
	ticket := &openAICodexTicket{AccountID: 1, Model: "gpt-6-astra", Acquisition: "reuse", SourceAccountID: 2, SourceAccountName: "source", Gateway: "unified-42"}
	inner := &usageFrameStub{}
	conn := &codexTicketUsageFrameConn{FrameConn: inner, account: account, binding: &openAIWSTurnBinding{ticket: ticket}}
	for i := 0; i < 2; i++ {
		require.NoError(t, conn.WriteFrame(t.Context(), coderws.MessageText, []byte(`{"type":"response.create"}`)))
	}
	require.NoError(t, conn.WriteFrame(t.Context(), coderws.MessageText, []byte(`{"type":"session.update"}`)))
	inner.err = io.ErrClosedPipe
	require.Error(t, conn.WriteFrame(t.Context(), coderws.MessageText, []byte(`{"type":"response.create"}`)))
	events := usageEvents()
	require.Len(t, events, 2)
	require.Equal(t, "websocket", events[0].TicketUsage.Transport)
	require.EqualValues(t, 2, events[0].TicketUsage.SourceAccountID)
	require.NotEqual(t, events[0].ID, events[1].ID)
}

func TestTicketUsageIgnoresProbes(t *testing.T) {
	resetCodexHarvestFlow()
	t.Cleanup(resetCodexHarvestFlow)
	account := &Account{ID: 1}
	ticket := &openAICodexTicket{AccountID: 1, Acquisition: "self"}
	recordCodexTicketUsage(WithAstraSourceAcquisition(t.Context()), account, ticket, time.Now(), "sse", 200)
	recordCodexTicketUsage(context.WithValue(t.Context(), openAICodexStateProbeContextKey{}, true), account, ticket, time.Now(), "sse", 200)
	require.Empty(t, usageEvents())
}
