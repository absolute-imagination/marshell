package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore keeps the inbox and the delivery receipts in Postgres (tables agent_inbox and agent_receipts).
// It is the default when REDIS_URL is not set and a database is configured, so unread messages survive a
// restart without running Redis. Semantics match redisInbox: Take and Ack remove what they return, and
// rows older than inboxTTL are swept.
type pgStore struct {
	pool *pgxpool.Pool
}

const pgStoreTimeout = 5 * time.Second

func newPGStore(pool *pgxpool.Pool) *pgStore {
	s := &pgStore{pool: pool}
	go s.sweepLoop()
	return s
}

// sweepLoop drops inbox rows and receipts past the retention window, once at start and then hourly.
func (s *pgStore) sweepLoop() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cutoff := time.Now().Add(-inboxTTL)
		if _, err := s.pool.Exec(ctx, `DELETE FROM public.agent_inbox WHERE created_at < $1`, cutoff); err != nil {
			log.Printf("pg inbox sweep: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM public.agent_receipts WHERE updated_at < $1`, cutoff); err != nil {
			log.Printf("pg receipts sweep: %v", err)
		}
		cancel()
		time.Sleep(time.Hour)
	}
}

// queryJSON runs a query whose single column is JSON text and decodes each row into T, skipping rows
// that no longer decode, as the Redis store does.
func queryJSON[T any](pool *pgxpool.Pool, sql string, args ...any) ([]T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreTimeout)
	defer cancel()
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *pgStore) Push(agentID string, msg wireMessage) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreTimeout)
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO public.agent_inbox (agent_id, message_id, message)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (agent_id, message_id) DO UPDATE SET message = EXCLUDED.message`,
		agentID, msg.ID, string(raw))
	return err
}

func (s *pgStore) List(agentID string) ([]wireMessage, error) {
	return queryJSON[wireMessage](s.pool, `
		SELECT message::text FROM public.agent_inbox
		WHERE agent_id = $1 ORDER BY created_at, message_id`, agentID)
}

func (s *pgStore) Take(agentID string) ([]wireMessage, error) {
	return queryJSON[wireMessage](s.pool, `
		WITH taken AS (
			DELETE FROM public.agent_inbox WHERE agent_id = $1
			RETURNING message, created_at, message_id
		)
		SELECT message::text FROM taken ORDER BY created_at, message_id`, agentID)
}

func (s *pgStore) Ack(agentID string, ids []string) (int, []wireMessage, error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	acked, err := queryJSON[wireMessage](s.pool, `
		WITH taken AS (
			DELETE FROM public.agent_inbox WHERE agent_id = $1 AND message_id = ANY($2)
			RETURNING message, created_at, message_id
		)
		SELECT message::text FROM taken ORDER BY created_at, message_id`, agentID, ids)
	if err != nil {
		return 0, nil, err
	}
	return len(acked), acked, nil
}

func (s *pgStore) SaveReceipt(senderID string, r messageReceipt) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreTimeout)
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO public.agent_receipts (sender_id, message_id, receipt, updated_at)
		VALUES ($1, $2, $3::jsonb, now())
		ON CONFLICT (sender_id, message_id) DO UPDATE SET receipt = EXCLUDED.receipt, updated_at = now()`,
		senderID, r.MessageID, string(raw))
	return err
}

func (s *pgStore) GetReceipts(senderID string, ids []string) ([]messageReceipt, error) {
	if len(ids) == 0 {
		return []messageReceipt{}, nil
	}
	return queryJSON[messageReceipt](s.pool, `
		SELECT receipt::text FROM public.agent_receipts
		WHERE sender_id = $1 AND message_id = ANY($2)`, senderID, ids)
}

func (s *pgStore) ListReceipts(senderID string) ([]messageReceipt, error) {
	return queryJSON[messageReceipt](s.pool, `
		SELECT receipt::text FROM public.agent_receipts WHERE sender_id = $1`, senderID)
}
