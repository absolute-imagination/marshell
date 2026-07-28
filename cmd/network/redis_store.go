package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Atomically read all inbox messages and delete the key in one round trip.
var takeInboxScript = redis.NewScript(`
local vals = redis.call('HVALS', KEYS[1])
if #vals > 0 then
  redis.call('DEL', KEYS[1])
end
return vals
`)

const (
	nsMarshell = "marshell"
)

type redisInbox struct {
	client    *redis.Client
	namespace string
}

func newRedisInbox(redisURL, namespace string) (*redisInbox, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	log.Printf("redis store connected (namespace=%s)", namespace)
	return &redisInbox{client: client, namespace: namespace}, nil
}

func (s *redisInbox) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *redisInbox) inboxKey(agentID string) string {
	return s.namespace + ":inbox:" + agentID
}

func (s *redisInbox) receiptKey(senderID string) string {
	return s.namespace + ":receipt:" + senderID
}





func (s *redisInbox) touch(ctx context.Context, key string) {
	_ = s.client.Expire(ctx, key, inboxTTL).Err()
}

func (s *redisInbox) Push(agentID string, msg wireMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	key := s.inboxKey(agentID)
	if err := s.client.HSet(ctx, key, msg.ID, raw).Err(); err != nil {
		return err
	}
	s.touch(ctx, key)
	return nil
}

func (s *redisInbox) List(agentID string) ([]wireMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key := s.inboxKey(agentID)
	raw, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []wireMessage{}, nil
	}
	msgs := make([]wireMessage, 0, len(raw))
	for _, v := range raw {
		var msg wireMessage
		if err := json.Unmarshal([]byte(v), &msg); err != nil {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

func (s *redisInbox) Take(agentID string) ([]wireMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key := s.inboxKey(agentID)
	vals, err := takeInboxScript.Run(ctx, s.client, []string{key}).StringSlice()
	if err != nil {
		return nil, err
	}
	msgs := make([]wireMessage, 0, len(vals))
	for _, v := range vals {
		var msg wireMessage
		if err := json.Unmarshal([]byte(v), &msg); err != nil {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

func (s *redisInbox) Ack(agentID string, ids []string) (int, []wireMessage, error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key := s.inboxKey(agentID)
	fields := make([]string, 0, len(ids))
	for _, id := range ids {
		fields = append(fields, id)
	}
	vals, err := s.client.HMGet(ctx, key, fields...).Result()
	if err != nil {
		return 0, nil, err
	}
	var acked []wireMessage
	delFields := make([]string, 0, len(ids))
	for i, v := range vals {
		if v == nil {
			continue
		}
		str, ok := v.(string)
		if !ok {
			continue
		}
		var msg wireMessage
		if err := json.Unmarshal([]byte(str), &msg); err != nil {
			continue
		}
		acked = append(acked, msg)
		delFields = append(delFields, fields[i])
	}
	if len(delFields) == 0 {
		return 0, nil, nil
	}
	if err := s.client.HDel(ctx, key, delFields...).Err(); err != nil {
		return 0, nil, err
	}
	s.touch(ctx, key)
	return len(acked), acked, nil
}

func (s *redisInbox) SaveReceipt(senderID string, r messageReceipt) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	key := s.receiptKey(senderID)
	if err := s.client.HSet(ctx, key, r.MessageID, raw).Err(); err != nil {
		return err
	}
	s.touch(ctx, key)
	return nil
}

func (s *redisInbox) GetReceipts(senderID string, ids []string) ([]messageReceipt, error) {
	if len(ids) == 0 {
		return []messageReceipt{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key := s.receiptKey(senderID)
	vals, err := s.client.HMGet(ctx, key, ids...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]messageReceipt, 0, len(ids))
	for _, v := range vals {
		if v == nil {
			continue
		}
		str, ok := v.(string)
		if !ok {
			continue
		}
		var r messageReceipt
		if err := json.Unmarshal([]byte(str), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *redisInbox) ListReceipts(senderID string) ([]messageReceipt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key := s.receiptKey(senderID)
	raw, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []messageReceipt{}, nil
	}
	out := make([]messageReceipt, 0, len(raw))
	for _, v := range raw {
		var r messageReceipt
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
