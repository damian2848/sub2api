package service

import (
	"context"
	"net/http"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// CodexTicketUsage contains only provenance, never ticket state or cookies.
type CodexTicketUsage struct {
	SourceAccountID   int64  `json:"source_account_id,omitempty"`
	SourceAccountName string `json:"source_account_name,omitempty"`
	Acquisition       string `json:"acquisition"`
	Gateway           string `json:"gateway,omitempty"`
	Transport         string `json:"transport"`
	RequestKind       string `json:"request_kind"`
}

type codexTicketAccountTestKey struct{}

func recordCodexTicketUsage(ctx context.Context, account *Account, ticket *openAICodexTicket, at time.Time, transport string, status int) {
	if account == nil || ticket == nil || IsAstraSourceAcquisition(ctx) || IsOpenAICodexStateProbeRequest(ctx) {
		return
	}
	usage := &CodexTicketUsage{SourceAccountID: ticket.SourceAccountID, SourceAccountName: clipFlowText(ticket.SourceAccountName, 80), Acquisition: "unknown", Gateway: clipFlowText(ticket.Gateway, 160), Transport: transport, RequestKind: "business"}
	if test, _ := ctx.Value(codexTicketAccountTestKey{}).(bool); test {
		usage.RequestKind = "account_test"
	}
	switch {
	case ticket.SourceAccountID > 0 && ticket.SourceAccountID != account.ID:
		usage.Acquisition = "reuse"
	case ticket.Acquisition == "self" || ticket.SourceAccountID == account.ID:
		usage.Acquisition = "self"
		usage.SourceAccountID, usage.SourceAccountName = account.ID, clipFlowText(account.Name, 80)
	}
	recordCodexHarvestFlow(CodexHarvestFlowEvent{At: at, Stage: "usage", Kind: "ticket_used", AccountID: account.ID, AccountName: account.Name, Model: ticket.Model, Node: ticket.HarvestNodeName, HTTPStatus: status, TicketUsage: usage})
}

func recordCodexWSTicketUsage(ctx context.Context, account *Account, binding *openAIWSTurnBinding, at time.Time) {
	if binding != nil {
		recordCodexTicketUsage(ctx, account, binding.ticket, at, "websocket", 0)
	}
}

// Record only successfully written response.create frames, including subsequent
// turns on a reused connection. Control frames and pre-send failures are excluded.
type codexTicketUsageFrameConn struct {
	openaiwsv2.FrameConn
	account *Account
	binding *openAIWSTurnBinding
}

func (c *codexTicketUsageFrameConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	at := time.Now()
	if err := c.FrameConn.WriteFrame(ctx, kind, payload); err != nil {
		return err
	}
	if kind == coderws.MessageText && gjson.GetBytes(payload, "type").String() == "response.create" {
		recordCodexWSTicketUsage(ctx, c.account, c.binding, at)
	}
	return nil
}

// HTTP status is a receipt, not a claim that the model response succeeded.
func recordCodexHTTPTicketUsage(ctx context.Context, account *Account, ticket *openAICodexTicket, at time.Time, response *http.Response, written bool) {
	if response == nil && !written {
		return
	}
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	recordCodexTicketUsage(ctx, account, ticket, at, "sse", status)
}
