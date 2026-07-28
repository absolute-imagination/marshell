package main

import (
	"sync"
	"time"
)

// Ephemeral retention for inbox, convo history, receipts, and friend-hold queues.
const inboxTTL = 7 * 24 * time.Hour

// Receipt ladder (sup + main network):
//   accepted  — server validated the send (HTTP response; not yet a delivery claim)
//   delivered — message is in the recipient's inbox (HSET / push succeeded)
//   received  — recipient took or acked the message (agent pulled it)
//   read      — recipient explicitly marked read (POST /sup/v1/read)
//   replied   — a reply referenced this message via in_reply_to
// Receipts only move forward; never downgrade.
// Never tell a human "delivered" unless status is delivered or beyond.
type messageReceipt struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	From      string `json:"from"`
	To        string `json:"to"`
	UpdatedAt string `json:"updated_at"`
}

type inboxBackend interface {
	Push(agentID string, msg wireMessage) error
	List(agentID string) ([]wireMessage, error)
	Take(agentID string) ([]wireMessage, error)
	Ack(agentID string, ids []string) (int, []wireMessage, error)
}

type memoryInbox struct {
	mu    sync.Mutex
	inbox map[string][]wireMessage
}

func newMemoryInbox() *memoryInbox {
	return &memoryInbox{inbox: make(map[string][]wireMessage)}
}

func (s *memoryInbox) Push(agentID string, msg wireMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inbox[agentID] = append(s.inbox[agentID], msg)
	return nil
}

func (s *memoryInbox) List(agentID string) ([]wireMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.inbox[agentID]
	if len(msgs) == 0 {
		return []wireMessage{}, nil
	}
	out := make([]wireMessage, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (s *memoryInbox) Take(agentID string) ([]wireMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.inbox[agentID]
	if len(msgs) == 0 {
		return []wireMessage{}, nil
	}
	s.inbox[agentID] = nil
	return msgs, nil
}

type receiptBackend interface {
	SaveReceipt(senderID string, r messageReceipt) error
	GetReceipts(senderID string, ids []string) ([]messageReceipt, error)
	ListReceipts(senderID string) ([]messageReceipt, error)
}

type memoryReceipts struct {
	mu    sync.Mutex
	store map[string]map[string]messageReceipt
}

func newMemoryReceipts() *memoryReceipts {
	return &memoryReceipts{store: make(map[string]map[string]messageReceipt)}
}

func (s *memoryReceipts) SaveReceipt(senderID string, r messageReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store[senderID] == nil {
		s.store[senderID] = make(map[string]messageReceipt)
	}
	s.store[senderID][r.MessageID] = r
	return nil
}

func (s *memoryReceipts) GetReceipts(senderID string, ids []string) ([]messageReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := s.store[senderID]
	if len(bucket) == 0 {
		return []messageReceipt{}, nil
	}
	out := make([]messageReceipt, 0, len(ids))
	for _, id := range ids {
		if r, ok := bucket[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memoryReceipts) ListReceipts(senderID string) ([]messageReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := s.store[senderID]
	if len(bucket) == 0 {
		return []messageReceipt{}, nil
	}
	out := make([]messageReceipt, 0, len(bucket))
	for _, r := range bucket {
		out = append(out, r)
	}
	return out, nil
}

func (s *memoryInbox) Ack(agentID string, ids []string) (int, []wireMessage, error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.inbox[agentID]
	if len(msgs) == 0 {
		return 0, nil, nil
	}
	kept := msgs[:0]
	var acked []wireMessage
	for _, msg := range msgs {
		if _, ok := want[msg.ID]; ok {
			acked = append(acked, msg)
			continue
		}
		kept = append(kept, msg)
	}
	s.inbox[agentID] = kept
	return len(acked), acked, nil
}
