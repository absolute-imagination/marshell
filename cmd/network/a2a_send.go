package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type a2aMessagePart struct {
	Text string `json:"text"`
}

type a2aInboundMessage struct {
	Role  string           `json:"role"`
	Parts []a2aMessagePart `json:"parts"`
}

type a2aSendParams struct {
	Message a2aInboundMessage `json:"message"`
}

type a2aRESTSendRequest struct {
	Message a2aInboundMessage `json:"message"`
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func a2aMessageSendHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recipientName := strings.TrimSpace(r.PathValue("name"))
		if recipientName == "" || !agentNameRe.MatchString(recipientName) {
			writeA2AError(w, http.StatusBadRequest, nil, -32602, "invalid agent name")
			return
		}

		key := bearerToken(r)
		if key == "" {
			writeA2AError(w, http.StatusUnauthorized, nil, -32001, "agent_key required")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeA2AError(w, http.StatusBadRequest, nil, -32700, "invalid body")
			return
		}

		text, rpcID, isRPC, rpcErr := parseA2ASendBody(body)
		if rpcErr != "" {
			writeA2AError(w, http.StatusBadRequest, rpcID, -32602, rpcErr)
			return
		}

		correlationID := strings.TrimSpace(r.Header.Get("X-Correlation-Id"))
		clientMessageID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		from, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeA2AError(w, http.StatusUnauthorized, rpcID, -32001, "invalid agent_key")
				return
			}
			writeA2AError(w, http.StatusInternalServerError, rpcID, -32603, "send failed")
			return
		}

		result, err := deliverMessage(ctx, pool, hub, from, recipientName, text, correlationID, clientMessageID)
		if err != nil {
			switch {
			case errors.Is(err, errForbidden):
				writeA2AError(w, http.StatusForbidden, rpcID, -32003, "permission denied")
			case errors.Is(err, errSendInvalid):
				writeA2AError(w, http.StatusBadRequest, rpcID, -32602, "message text required")
			case errors.Is(err, errSendTooLong):
				writeA2AError(w, http.StatusBadRequest, rpcID, -32602, "text too long")
			case errors.Is(err, errNotFound):
				writeA2AError(w, http.StatusNotFound, rpcID, -32004, "peer not found")
			case errors.Is(err, errInboxDelivery):
				writeA2AError(w, http.StatusServiceUnavailable, rpcID, -32603, "inbox delivery failed")
			default:
				log.Printf("a2a send failed: %v", err)
				writeA2AError(w, http.StatusInternalServerError, rpcID, -32603, "send failed")
			}
			return
		}

		a2aMsg := buildA2AResponseMessage(result.Message)
		relay := map[string]any{
			"status":       result.Status,
			"message_id":   result.Message.ID,
			"poll_status":  "/v1/messages/status?ids=" + result.Message.ID,
			"status_notes": sendStatusNotes(),
		}
		if isRPC {
			writeJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      rpcID,
				"result": map[string]any{
					"message": a2aMsg,
					"relay":   relay,
				},
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, map[string]any{
			"message": a2aMsg,
			"relay":   relay,
		})
	}
}

func a2aJSONRPCHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	send := a2aMessageSendHandler(pool, hub)
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeA2AError(w, http.StatusBadRequest, nil, -32700, "invalid body")
			return
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeA2AError(w, http.StatusBadRequest, nil, -32700, "invalid json-rpc")
			return
		}
		method := strings.TrimSpace(req.Method)
		switch method {
		case "SendMessage", "message/send":
			r = r.Clone(r.Context())
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			send(w, r)
		default:
			writeA2AError(w, http.StatusBadRequest, req.ID, -32601, "method not found")
		}
	}
}

func parseA2ASendBody(body []byte) (text string, rpcID any, isRPC bool, rpcErr string) {
	var rpc jsonRPCRequest
	if err := json.Unmarshal(body, &rpc); err == nil && strings.TrimSpace(rpc.Method) != "" {
		isRPC = true
		rpcID = rpc.ID
		method := strings.TrimSpace(rpc.Method)
		if method != "SendMessage" && method != "message/send" {
			return "", rpcID, true, "unsupported method"
		}
		var params a2aSendParams
		if err := json.Unmarshal(rpc.Params, &params); err != nil {
			return "", rpcID, true, "invalid params"
		}
		text, ok := extractA2AText(params.Message)
		if !ok {
			return "", rpcID, true, "message parts required"
		}
		return text, rpcID, true, ""
	}

	var rest a2aRESTSendRequest
	if err := json.Unmarshal(body, &rest); err != nil {
		return "", nil, false, "invalid json"
	}
	text, ok := extractA2AText(rest.Message)
	if !ok {
		return "", nil, false, "message parts required"
	}
	return text, nil, false, ""
}

func extractA2AText(msg a2aInboundMessage) (string, bool) {
	var parts []string
	for _, p := range msg.Parts {
		if t := strings.TrimSpace(p.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func buildA2AResponseMessage(msg wireMessage) map[string]any {
	meta := map[string]any{
		"marshell.relay.status":      "delivered",
		"marshell.relay.from":        msg.From,
		"marshell.relay.to":          msg.To,
		"marshell.relay.body":        msg.Text,
		"marshell.relay.message_id":  msg.ID,
		"marshell.relay.poll_status": "/v1/messages/status?ids=" + msg.ID,
		"marshell.relay.trace":       "/v1/messages/trace?id=" + msg.ID,
	}
	if msg.CorrelationID != "" {
		meta["marshell.relay.correlation_id"] = msg.CorrelationID
	}
	return map[string]any{
		"messageId": msg.ID,
		"role":      "ROLE_AGENT",
		"parts": []map[string]any{{
			"text": "Message delivered to " + msg.To,
		}},
		"metadata": meta,
	}
}

func sendStatusNotes() map[string]string {
	return map[string]string{
		"delivered": "Queued in recipient inbox (HTTP poll or WebSocket).",
		"received":  "Recipient POST /v1/messages/ack; poll GET /v1/messages/status.",
	}
}


func writeA2AError(w http.ResponseWriter, status int, id any, code int, message string) {
	if id != nil {
		writeJSON(w, status, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": jsonRPCError{Code: code, Message: message},
		})
		return
	}
	writeJSON(w, status, map[string]string{"error": message})
}
