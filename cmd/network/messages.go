package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type agentIdentity struct {
	ID       string
	Name     string
	SubnetID string
}

type wireMessage struct {
	ID            string `json:"id"`
	From          string `json:"from"`
	FromID        string `json:"from_id"`
	To            string `json:"to"`
	ToID          string `json:"to_id"`
	Text          string `json:"text"`
	CreatedAt     string `json:"created_at"`
	CorrelationID string `json:"correlation_id,omitempty"`
	ThreadID      string `json:"thread_id,omitempty"`
	InReplyTo     string `json:"in_reply_to,omitempty"`
	// Kind classifies the item so agents don't have to parse text.
	// DM kinds: text | json | task (legacy "message" ≡ text).
	// System: friend_request | friend_accepted. Empty on the main network.
	Kind string `json:"kind,omitempty"`
	// Payload is structured JSON for kind=json|task (max 8KiB). Relay does not
	// validate domain schemas — only size/shape.
	Payload json.RawMessage `json:"payload,omitempty"`
	// RequestID ties friend_request / friend_accepted notices to one edge.
	RequestID string `json:"request_id,omitempty"`
	// GrantID ties grant_request / grant_accepted notices, or a consented send.
	GrantID string `json:"grant_id,omitempty"`
}

type messageHub struct {
	mu             sync.Mutex
	backend        inboxBackend
	receipts       receiptBackend
	inFlight       map[string]map[string]wireMessage
	waiters        map[string][]chan struct{}
	receiptWaiters map[string][]chan struct{}
	ws             *wsHub
}

func newMessageHub(redisURL string) *messageHub {
	h := &messageHub{
		receipts:       newMemoryReceipts(),
		inFlight:       make(map[string]map[string]wireMessage),
		waiters:        make(map[string][]chan struct{}),
		receiptWaiters: make(map[string][]chan struct{}),
	}
	if redisURL != "" {
		if rs, err := newRedisInbox(redisURL, nsMarshell); err != nil {
			log.Printf("redis unavailable, using memory inbox: %v", err)
			h.backend = newMemoryInbox()
		} else {
			h.backend = rs
			h.receipts = rs
		}
	} else {
		log.Println("REDIS_URL not set; inbox stored in memory only")
		h.backend = newMemoryInbox()
	}
	return h
}

func (h *messageHub) inboxFor(subnetID string) inboxBackend {
	_ = subnetID
	return h.backend
}

func (h *messageHub) receiptsFor(subnetID string) receiptBackend {
	_ = subnetID
	return h.receipts
}

func (h *messageHub) push(msg wireMessage, recipientSubnet, senderSubnet string) error {
	if err := h.inboxFor(recipientSubnet).Push(msg.ToID, msg); err != nil {
		return err
	}
	h.mu.Lock()
	// In recipient inbox ⇒ delivered (not merely accepted by the HTTP handler).
	h.setReceiptLocked(senderSubnet, msg.FromID, msg.ID, "delivered", msg.From, msg.To)
	for _, ch := range h.waiters[msg.ToID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()

	h.emitWS(msg.ToID, wsEvent{
		Type:     "inbox",
		Messages: []wireMessage{msg},
	})
	return nil
}

func receiptRank(status string) int {
	switch status {
	case "accepted":
		return 1
	case "delivered":
		return 2
	case "received":
		return 3
	case "read":
		return 4
	case "replied":
		return 5
	default:
		return 0
	}
}

func (h *messageHub) setReceiptLocked(senderSubnet, senderID, msgID, status, from, to string) {
	r := messageReceipt{
		MessageID: msgID,
		Status:    status,
		From:      from,
		To:        to,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := h.receiptsFor(senderSubnet).SaveReceipt(senderID, r); err != nil {
		log.Printf("receipt save failed: %v", err)
	}
	for _, ch := range h.receiptWaiters[senderID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *messageHub) notifyReceived(ctx context.Context, pool *pgxpool.Pool, acked []wireMessage) {
	if len(acked) == 0 {
		return
	}
	h.mu.Lock()
	for _, msg := range acked {
		senderSubnet := lookupAgentSubnet(ctx, pool, msg.FromID)
		h.setReceiptLocked(senderSubnet, msg.FromID, msg.ID, "received", msg.From, msg.To)
		receipt := messageReceipt{
			MessageID: msg.ID,
			Status:    "received",
			From:      msg.From,
			To:        msg.To,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		h.mu.Unlock()
		h.emitWS(msg.FromID, wsEvent{Type: "receipt", Receipt: &receipt})
		h.mu.Lock()
	}
	h.mu.Unlock()
}

func (h *messageHub) waitFor(agentID string, timeout time.Duration) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.waiters[agentID] = append(h.waiters[agentID], ch)
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		waiters := h.waiters[agentID]
		filtered := waiters[:0]
		for _, w := range waiters {
			if w != ch {
				filtered = append(filtered, w)
			}
		}
		if len(filtered) == 0 {
			delete(h.waiters, agentID)
		} else {
			h.waiters[agentID] = filtered
		}
		h.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}

func (h *messageHub) waitForReceipts(agentID string, timeout time.Duration) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.receiptWaiters[agentID] = append(h.receiptWaiters[agentID], ch)
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		waiters := h.receiptWaiters[agentID]
		filtered := waiters[:0]
		for _, w := range waiters {
			if w != ch {
				filtered = append(filtered, w)
			}
		}
		if len(filtered) == 0 {
			delete(h.receiptWaiters, agentID)
		} else {
			h.receiptWaiters[agentID] = filtered
		}
		h.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}

func (h *messageHub) peek(agentID, subnetID string) []wireMessage {
	msgs, err := h.inboxFor(subnetID).List(agentID)
	if err != nil {
		log.Printf("inbox list failed: %v", err)
		return []wireMessage{}
	}
	return msgs
}

func (h *messageHub) take(agentID, subnetID string) []wireMessage {
	msgs, err := h.inboxFor(subnetID).Take(agentID)
	if err != nil {
		log.Printf("inbox take failed: %v", err)
		return []wireMessage{}
	}
	if len(msgs) == 0 {
		return msgs
	}
	h.mu.Lock()
	if h.inFlight[agentID] == nil {
		h.inFlight[agentID] = make(map[string]wireMessage)
	}
	for _, m := range msgs {
		h.inFlight[agentID][m.ID] = m
	}
	h.mu.Unlock()
	return msgs
}

// takeAndReceive destructively drains the inbox and marks receipts received.
func (h *messageHub) takeAndReceive(ctx context.Context, pool *pgxpool.Pool, agentID, subnetID string) []wireMessage {
	msgs := h.take(agentID, subnetID)
	if len(msgs) > 0 {
		h.notifyReceived(ctx, pool, msgs)
	}
	return msgs
}

func ackedIDs(acked []wireMessage) map[string]struct{} {
	set := make(map[string]struct{}, len(acked))
	for _, m := range acked {
		set[m.ID] = struct{}{}
	}
	return set
}

func (h *messageHub) ack(ctx context.Context, pool *pgxpool.Pool, agentID, subnetID string, ids []string) int {
	n, acked, err := h.inboxFor(subnetID).Ack(agentID, ids)
	if err != nil {
		log.Printf("inbox ack failed: %v", err)
		return 0
	}
	found := ackedIDs(acked)
	h.mu.Lock()
	for _, id := range ids {
		if _, ok := found[id]; ok {
			delete(h.inFlight[agentID], id)
			continue
		}
		if m, ok := h.inFlight[agentID][id]; ok {
			acked = append(acked, m)
			delete(h.inFlight[agentID], id)
		}
	}
	h.mu.Unlock()
	h.notifyReceived(ctx, pool, acked)
	if len(acked) > n {
		return len(acked)
	}
	return n
}

func (h *messageHub) getReceipts(agentID, subnetID string, ids []string) []messageReceipt {
	if len(ids) == 0 {
		rs, err := h.receiptsFor(subnetID).ListReceipts(agentID)
		if err != nil {
			log.Printf("receipt list failed: %v", err)
			return []messageReceipt{}
		}
		return rs
	}
	rs, err := h.receiptsFor(subnetID).GetReceipts(agentID, ids)
	if err != nil {
		log.Printf("receipt get failed: %v", err)
		return []messageReceipt{}
	}
	return rs
}

func bearerToken(r *http.Request) string {
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimPrefix(token, "bearer ")
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("agent_key"))
	}
	return strings.TrimSpace(token)
}

func resolveAgentKey(ctx context.Context, pool *pgxpool.Pool, agentKey string) (*agentIdentity, error) {
	if pool == nil {
		return nil, errors.New("database unavailable")
	}
	if !strings.HasPrefix(agentKey, "mak_") && !strings.HasPrefix(agentKey, "sak_") {
		return nil, errUnauthorized
	}

	var id agentIdentity
	err := pool.QueryRow(ctx, `
		select a.id::text, a.name, a.subnet_id::text
		from public.agents a
		join public.agent_credentials c on c.agent_id = a.id
		where c.revoked_at is null
		  and a.status <> 'revoked'
		  and c.key_hash = extensions.crypt($1, c.key_hash)
		limit 1
	`, agentKey).Scan(&id.ID, &id.Name, &id.SubnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errUnauthorized
		}
		return nil, err
	}

	_, _ = pool.Exec(ctx, `
		update public.agents
		set status = 'online', last_seen_at = now()
		where id = $1::uuid
	`, id.ID)

	return &id, nil
}

func resolveAgentByName(ctx context.Context, pool *pgxpool.Pool, homeSubnetID, name string) (*agentIdentity, error) {
	var id agentIdentity
	err := pool.QueryRow(ctx, `
		select id::text, name, subnet_id::text
		from public.agents
		where lower(name) = lower($2)
		  and status <> 'revoked'
		  and subnet_id in (
		    select $1::uuid
		    union
		    select case
		      when l.from_subnet_id = $1::uuid then l.to_subnet_id
		      else l.from_subnet_id
		    end
		    from public.subnet_links l
		    where l.status = 'active'
		      and ($1::uuid = l.from_subnet_id or $1::uuid = l.to_subnet_id)
		  )
		order by case when subnet_id = $1::uuid then 0 else 1 end
		limit 1
	`, homeSubnetID, name).Scan(&id.ID, &id.Name, &id.SubnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	return &id, nil
}

var (
	errUnauthorized = errors.New("unauthorized")
	errNotFound     = errors.New("not found")
)

type sendRequest struct {
	To              string          `json:"to"`
	Text            string          `json:"text"`
	CorrelationID   string          `json:"correlation_id"`
	ClientMessageID string          `json:"client_message_id"`
	ThreadID        string          `json:"thread_id"`
	InReplyTo       string          `json:"in_reply_to"`
	Kind            string          `json:"kind"`
	Payload         json.RawMessage `json:"payload"`
	GrantID         string          `json:"grant_id"`
}

type sendResult struct {
	Message   wireMessage
	Status    string
	Duplicate bool
}

// findRecentDuplicateSend returns an identical send from the same sender to the same
// recipient within the last 2 minutes (e.g. ask timeout → retry).
func findRecentDuplicateSend(ctx context.Context, pool *pgxpool.Pool, fromID, toID, text string) (*wireMessage, error) {
	if pool == nil {
		return nil, nil
	}
	var msg wireMessage
	var created time.Time
	err := pool.QueryRow(ctx, `
		select id, from_name, to_name, body, created_at, coalesce(correlation_id, '')
		from public.agent_messages
		where from_agent_id = $1::uuid
		  and to_agent_id = $2::uuid
		  and body = $3
		  and created_at > now() - interval '2 minutes'
		order by created_at desc
		limit 1
	`, fromID, toID, text).Scan(
		&msg.ID, &msg.From, &msg.To, &msg.Text, &created, &msg.CorrelationID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	msg.FromID = fromID
	msg.ToID = toID
	msg.CreatedAt = created.UTC().Format(time.RFC3339)
	return &msg, nil
}

func findByClientMessageID(ctx context.Context, pool *pgxpool.Pool, fromID, toID, clientMessageID string) (*wireMessage, error) {
	if pool == nil || strings.TrimSpace(clientMessageID) == "" {
		return nil, nil
	}
	var msg wireMessage
	var created time.Time
	var storedToID string
	err := pool.QueryRow(ctx, `
		select id, from_name, to_name, body, created_at, coalesce(correlation_id, ''), to_agent_id::text
		from public.agent_messages
		where from_agent_id = $1::uuid
		  and client_message_id = $2
		limit 1
	`, fromID, clientMessageID).Scan(
		&msg.ID, &msg.From, &msg.To, &msg.Text, &created, &msg.CorrelationID, &storedToID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if storedToID != toID {
		return nil, errIdempotencyMismatch
	}
	msg.FromID = fromID
	msg.ToID = storedToID
	msg.CreatedAt = created.UTC().Format(time.RFC3339)
	return &msg, nil
}

func claimOutboundMessage(ctx context.Context, pool *pgxpool.Pool, subnetID string, from, recipient *agentIdentity, msg wireMessage, clientMessageID string) (bool, error) {
	if pool == nil || clientMessageID == "" {
		return true, nil
	}
	tag, err := pool.Exec(ctx, `
		insert into public.agent_messages (
			id, subnet_id, from_agent_id, to_agent_id, from_name, to_name, body, created_at, correlation_id, client_message_id
		) values (
			$1, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8::timestamptz, nullif($9, ''), $10
		)
		on conflict (from_agent_id, client_message_id)
		where client_message_id is not null and client_message_id <> ''
		do nothing
	`, msg.ID, subnetID, from.ID, recipient.ID, from.Name, recipient.Name, msg.Text, msg.CreatedAt, msg.CorrelationID, clientMessageID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// deliverMessage routes a text message to a peer on the home subnet or a linked subnet.
func deliverMessage(ctx context.Context, pool *pgxpool.Pool, hub *messageHub, from *agentIdentity, toName, text, correlationID, clientMessageID string) (*sendResult, error) {
	toName = strings.TrimSpace(toName)
	text = strings.TrimSpace(text)
	if toName == "" || text == "" {
		return nil, errSendInvalid
	}
	if len(text) > 16_000 {
		return nil, errSendTooLong
	}

	if err := agentCan(ctx, pool, from.ID, permMessages, false, true); err != nil {
		return nil, err
	}

	recipient, err := resolveAgentByName(ctx, pool, from.SubnetID, toName)
	if err != nil {
		return nil, err
	}

	clientMessageID = strings.TrimSpace(clientMessageID)

	if clientMessageID != "" {
		if existing, err := findByClientMessageID(ctx, pool, from.ID, recipient.ID, clientMessageID); err != nil {
			return nil, err
		} else if existing != nil {
			metricsIncSendDeduped()
			return &sendResult{Message: *existing, Status: "delivered"}, nil
		}
	}

	if dup, err := findRecentDuplicateSend(ctx, pool, from.ID, recipient.ID, text); err != nil {
		return nil, err
	} else if dup != nil {
		metricsIncSendDeduped()
		return &sendResult{Message: *dup, Status: "delivered"}, nil
	}

	msgID, err := randomToken("msg_")
	if err != nil {
		return nil, err
	}

	msg := wireMessage{
		ID:            msgID,
		From:          from.Name,
		FromID:        from.ID,
		To:            recipient.Name,
		ToID:          recipient.ID,
		Text:          text,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		CorrelationID: strings.TrimSpace(correlationID),
	}

	if clientMessageID != "" {
		claimed, err := claimOutboundMessage(ctx, pool, from.SubnetID, from, recipient, msg, clientMessageID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			if existing, err := findByClientMessageID(ctx, pool, from.ID, recipient.ID, clientMessageID); err != nil {
				return nil, err
			} else if existing != nil {
				metricsIncSendDeduped()
				return &sendResult{Message: *existing, Status: "delivered"}, nil
			}
			return nil, errInboxDelivery
		}
	}

	if err := hub.push(msg, recipient.SubnetID, from.SubnetID); err != nil {
		if clientMessageID != "" {
			_, _ = pool.Exec(ctx, `delete from public.agent_messages where id = $1`, msgID)
		}
		return nil, errInboxDelivery
	}
	if clientMessageID == "" {
		persistMessage(ctx, pool, from.SubnetID, msg, clientMessageID)
	}
	metricsIncSends()

	preview := truncateSummary(text, 500)
	logAgentEvent(ctx, pool, from.ID, from.SubnetID, "sent", "→ "+recipient.Name+": "+preview)
	logAgentEvent(ctx, pool, recipient.ID, recipient.SubnetID, "received", "← "+from.Name+": "+preview)

	return &sendResult{Message: msg, Status: "delivered"}, nil
}

var (
	errSendInvalid         = errors.New("invalid send request")
	errSendTooLong         = errors.New("text too long")
	errInboxDelivery       = errors.New("inbox delivery failed")
	errIdempotencyMismatch = errors.New("client_message_id already used for a different recipient")
)

func sendHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		var req sendRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if cid := strings.TrimSpace(r.Header.Get("X-Correlation-Id")); cid != "" && strings.TrimSpace(req.CorrelationID) == "" {
			req.CorrelationID = cid
		}
		if cmid := strings.TrimSpace(r.Header.Get("Idempotency-Key")); cmid != "" && strings.TrimSpace(req.ClientMessageID) == "" {
			req.ClientMessageID = cmid
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		from, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			log.Printf("send auth failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "send failed"})
			return
		}

		result, err := deliverMessage(ctx, pool, hub, from, req.To, req.Text, req.CorrelationID, req.ClientMessageID)
		if err != nil {
			switch {
			case errors.Is(err, errForbidden):
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied", "code": "forbidden"})
			case errors.Is(err, errSendInvalid):
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to and text are required"})
			case errors.Is(err, errSendTooLong):
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text too long"})
			case errors.Is(err, errNotFound):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "peer not found", "code": "peer_offline"})
			case errors.Is(err, errInboxDelivery):
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"error": "inbox delivery failed — message not queued",
					"code":  "inbox_unavailable",
				})
			case errors.Is(err, errIdempotencyMismatch):
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "client_message_id already used for a different recipient",
					"code":  "idempotency_mismatch",
				})
			default:
				log.Printf("send failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "send failed"})
			}
			return
		}

		payload := map[string]any{
			"id":      result.Message.ID,
			"status":  result.Status,
			"from":    result.Message.From,
			"to":      result.Message.To,
			"created": result.Message.CreatedAt,
			"status_notes": map[string]string{
				"delivered": "Message is queued in the recipient inbox (HTTP poll or WebSocket).",
				"received":  "Recipient called POST /v1/messages/ack; poll GET /v1/messages/status or WebSocket receipt events.",
			},
			"poll_status": "/v1/messages/status?ids=" + result.Message.ID,
		}
		if result.Message.CorrelationID != "" {
			payload["correlation_id"] = result.Message.CorrelationID
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

func truncateSummary(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func logAgentEvent(ctx context.Context, pool *pgxpool.Pool, agentID, subnetID, kind, summary string) {
	if pool == nil {
		return
	}
	_, err := pool.Exec(ctx, `
		insert into public.agent_events (agent_id, subnet_id, kind, summary)
		values ($1::uuid, $2::uuid, $3, $4)
	`, agentID, subnetID, kind, summary)
	if err != nil {
		log.Printf("agent event log failed: %v", err)
	}
}

func persistMessage(ctx context.Context, pool *pgxpool.Pool, subnetID string, msg wireMessage, clientMessageID string) {
	if pool == nil {
		return
	}
	_, err := pool.Exec(ctx, `
		insert into public.agent_messages (
			id, subnet_id, from_agent_id, to_agent_id, from_name, to_name, body, created_at, correlation_id, client_message_id
		) values (
			$1, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8::timestamptz, nullif($9, ''), nullif($10, '')
		)
		on conflict (id) do nothing
	`, msg.ID, subnetID, msg.FromID, msg.ToID, msg.From, msg.To, msg.Text, msg.CreatedAt, msg.CorrelationID, strings.TrimSpace(clientMessageID))
	if err != nil {
		log.Printf("persist message failed: %v", err)
	}
}

type historyItem struct {
	ID              string          `json:"id"`
	Direction       string          `json:"direction"` // in | out
	Peer            string          `json:"peer"`
	Text            string          `json:"text"`
	CreatedAt       string          `json:"created_at"`
	CorrelationID   string          `json:"correlation_id,omitempty"`
	ClientMessageID string          `json:"client_message_id,omitempty"`
	ThreadID        string          `json:"thread_id,omitempty"`
	InReplyTo       string          `json:"in_reply_to,omitempty"`
	Kind            string          `json:"kind,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

func historyHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "history failed"})
			return
		}

		with := strings.TrimSpace(r.URL.Query().Get("with"))
		limit := 50
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				if n > 200 {
					n = 200
				}
				limit = n
			}
		}

		items, err := loadHistory(ctx, pool, agent, with, limit)
		if err != nil {
			log.Printf("history query failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "history failed"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"agent":    agent.Name,
			"messages": items,
		})
	}
}

func loadHistory(ctx context.Context, pool *pgxpool.Pool, agent *agentIdentity, with string, limit int) ([]historyItem, error) {
	items, err := loadHistoryFromMessages(ctx, pool, agent, with, limit)
	if err == nil && len(items) > 0 {
		return items, nil
	}
	if err != nil {
		log.Printf("agent_messages history unavailable: %v", err)
	}
	return loadHistoryFromEvents(ctx, pool, agent, with, limit)
}

func loadHistoryFromMessages(ctx context.Context, pool *pgxpool.Pool, agent *agentIdentity, with string, limit int) ([]historyItem, error) {
	rows, err := pool.Query(ctx, `
		select id, from_name, to_name, body, created_at, coalesce(correlation_id, ''), coalesce(client_message_id, '')
		from public.agent_messages
		where (from_agent_id = $1::uuid or to_agent_id = $1::uuid)
		  and created_at > now() - interval '24 hours'
		  and (
			$2 = '' or lower(from_name) = lower($2) or lower(to_name) = lower($2)
		  )
		order by created_at desc
		limit $3
	`, agent.ID, with, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]historyItem, 0)
	for rows.Next() {
		var id, fromName, toName, body, correlationID, clientMessageID string
		var created time.Time
		if err := rows.Scan(&id, &fromName, &toName, &body, &created, &correlationID, &clientMessageID); err != nil {
			return nil, err
		}
		item := historyItem{
			ID:              id,
			Text:            body,
			CreatedAt:       created.UTC().Format(time.RFC3339),
			CorrelationID:   correlationID,
			ClientMessageID: clientMessageID,
		}
		if strings.EqualFold(fromName, agent.Name) {
			item.Direction = "out"
			item.Peer = toName
		} else {
			item.Direction = "in"
			item.Peer = fromName
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadHistoryFromEvents(ctx context.Context, pool *pgxpool.Pool, agent *agentIdentity, with string, limit int) ([]historyItem, error) {
	rows, err := pool.Query(ctx, `
		select id::text, kind, summary, created_at
		from public.agent_events
		where agent_id = $1::uuid
		  and kind in ('sent', 'received')
		  and created_at > now() - interval '24 hours'
		order by created_at desc
		limit $2
	`, agent.ID, limit*2)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]historyItem, 0)
	withLower := strings.ToLower(with)
	for rows.Next() {
		var id, kind, summary string
		var created time.Time
		if err := rows.Scan(&id, &kind, &summary, &created); err != nil {
			return nil, err
		}
		item, ok := parseEventSummary(id, kind, summary, created)
		if !ok {
			continue
		}
		if withLower != "" && strings.ToLower(item.Peer) != withLower {
			continue
		}
		items = append(items, item)
		if len(items) >= limit {
			break
		}
	}
	return items, rows.Err()
}

func parseEventSummary(id, kind, summary string, created time.Time) (historyItem, bool) {
	summary = strings.TrimSpace(summary)
	dir := ""
	rest := ""
	switch {
	case strings.HasPrefix(summary, "→ "):
		dir = "out"
		rest = strings.TrimPrefix(summary, "→ ")
	case strings.HasPrefix(summary, "← "):
		dir = "in"
		rest = strings.TrimPrefix(summary, "← ")
	case kind == "sent":
		dir = "out"
		rest = summary
	case kind == "received":
		dir = "in"
		rest = summary
	default:
		return historyItem{}, false
	}
	peer := ""
	text := rest
	if i := strings.Index(rest, ": "); i >= 0 {
		peer = strings.TrimSpace(rest[:i])
		text = strings.TrimSpace(rest[i+2:])
	}
	if peer == "" {
		peer = "unknown"
	}
	return historyItem{
		ID:        "evt_" + id,
		Direction: dir,
		Peer:      peer,
		Text:      text,
		CreatedAt: created.UTC().Format(time.RFC3339),
	}, true
}

func inboxHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		authCtx, authCancel := context.WithTimeout(r.Context(), 8*time.Second)
		agent, err := resolveAgentKey(authCtx, pool, key)
		authCancel()
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			log.Printf("inbox auth failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "inbox failed"})
			return
		}

		peek := r.URL.Query().Get("peek") == "1" || r.URL.Query().Get("peek") == "true"
		waitSec := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("wait")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				if n > 120 {
					n = 120
				}
				waitSec = n
			}
		}

		var msgs []wireMessage
		if peek {
			msgs = hub.peek(agent.ID, agent.SubnetID)
			if len(msgs) == 0 && waitSec > 0 {
				hub.waitFor(agent.ID, time.Duration(waitSec)*time.Second)
				msgs = hub.peek(agent.ID, agent.SubnetID)
			}
		} else {
			msgs = hub.take(agent.ID, agent.SubnetID)
			if len(msgs) == 0 && waitSec > 0 {
				hub.waitFor(agent.ID, time.Duration(waitSec)*time.Second)
				msgs = hub.take(agent.ID, agent.SubnetID)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"messages": msgs,
			"agent":    agent.Name,
			"peek":     peek,
			"wait":     waitSec,
		})
	}
}

type ackRequest struct {
	IDs []string `json:"ids"`
}

func ackHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		var req ackRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ack failed"})
			return
		}

		n := hub.ack(ctx, pool, agent.ID, agent.SubnetID, req.IDs)
		if n > 0 {
			metricsIncAcks()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"acked": n,
			"note":  "Sender is notified via GET /v1/messages/status or WebSocket receipt events.",
		})
	}
}

func statusHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "status failed"})
			return
		}

		var ids []string
		if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if id := strings.TrimSpace(part); id != "" {
					ids = append(ids, id)
				}
			}
		}

		waitSec := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("wait")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				if n > 120 {
					n = 120
				}
				waitSec = n
			}
		}

		receipts := hub.getReceipts(agent.ID, agent.SubnetID, ids)
		if len(receipts) == 0 && waitSec > 0 && len(ids) > 0 {
			hub.waitForReceipts(agent.ID, time.Duration(waitSec)*time.Second)
			receipts = hub.getReceipts(agent.ID, agent.SubnetID, ids)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"agent":    agent.Name,
			"receipts": receipts,
			"status_notes": map[string]string{
				"delivered": "Queued in recipient inbox.",
				"received":  "Recipient acknowledged via POST /v1/messages/ack.",
			},
		})
	}
}

func traceHandler(pool *pgxpool.Pool, hub *messageHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}
		msgID := strings.TrimSpace(r.URL.Query().Get("id"))
		if msgID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trace failed"})
			return
		}

		var (
			fromName, toName, body, correlationID string
			fromID, toID                          string
			created                               time.Time
		)
		err = pool.QueryRow(ctx, `
			select from_agent_id::text, to_agent_id::text, from_name, to_name, body,
			       coalesce(correlation_id, ''), created_at
			from public.agent_messages
			where id = $1
			  and subnet_id = $2::uuid
		`, msgID, agent.SubnetID).Scan(&fromID, &toID, &fromName, &toName, &body, &correlationID, &created)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
				return
			}
			log.Printf("trace query failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trace failed"})
			return
		}

		senderID := fromID
		senderSubnet := lookupAgentSubnet(ctx, pool, senderID)
		receipts := hub.getReceipts(senderID, senderSubnet, []string{msgID})

		replyRows, err := pool.Query(ctx, `
			select id, from_name, to_name, body, created_at
			from public.agent_messages
			where subnet_id = $1::uuid
			  and from_agent_id = $2::uuid
			  and to_agent_id = $3::uuid
			  and created_at > $4::timestamptz
			order by created_at asc
			limit 5
		`, agent.SubnetID, toID, fromID, created)
		replies := make([]map[string]any, 0)
		if err == nil {
			defer replyRows.Close()
			for replyRows.Next() {
				var rid, rfrom, rto, rbody string
				var rcreated time.Time
				if err := replyRows.Scan(&rid, &rfrom, &rto, &rbody, &rcreated); err != nil {
					break
				}
				replies = append(replies, map[string]any{
					"id":         rid,
					"from":       rfrom,
					"to":         rto,
					"body":       rbody,
					"created_at": rcreated.UTC().Format(time.RFC3339),
				})
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"message": map[string]any{
				"id":             msgID,
				"from":           fromName,
				"to":             toName,
				"body":           body,
				"correlation_id": correlationID,
				"created_at":     created.UTC().Format(time.RFC3339),
			},
			"receipts": receipts,
			"replies":  replies,
		})
	}
}
