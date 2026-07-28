package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	errForbidden = errors.New("permission denied")
)

// Permission keys for network hard gates (console uses natural-language usage policy).
const (
	permMessages  = "messages"
	permHistory   = "history"
	permPeers     = "peers"
	permApprovals = "approvals"
	permExternal  = "external"
)

func defaultPermission(key string) (canRead, canWrite bool) {
	return true, key != permHistory
}

func agentCan(ctx context.Context, pool *pgxpool.Pool, agentID, key string, needRead, needWrite bool) error {
	if pool == nil {
		return errors.New("database unavailable")
	}
	var canRead, canWrite bool
	err := pool.QueryRow(ctx, `
		select can_read, can_write
		from public.agent_permissions
		where agent_id = $1::uuid and permission_key = $2
	`, agentID, key).Scan(&canRead, &canWrite)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			canRead, canWrite = defaultPermission(key)
		} else {
			return err
		}
	}
	if needRead && !canRead {
		return errForbidden
	}
	if needWrite && !canWrite {
		return errForbidden
	}
	return nil
}
