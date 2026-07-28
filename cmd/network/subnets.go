package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// visibleSubnetIDs returns the home subnet plus any actively linked peer subnets.
func visibleSubnetIDs(ctx context.Context, pool *pgxpool.Pool, homeSubnetID string) ([]string, error) {
	if pool == nil || homeSubnetID == "" {
		return nil, errors.New("database unavailable")
	}

	rows, err := pool.Query(ctx, `
		select $1::uuid
		union
		select case
			when l.from_subnet_id = $1::uuid then l.to_subnet_id
			else l.from_subnet_id
		end
		from public.subnet_links l
		where l.status = 'active'
		  and ($1::uuid = l.from_subnet_id or $1::uuid = l.to_subnet_id)
	`, homeSubnetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]string, 0, 4)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func resolveHomeSubnetFromAgentKey(ctx context.Context, pool *pgxpool.Pool, agentKey string) (string, error) {
	var subnetID string
	err := pool.QueryRow(ctx, `
		select a.subnet_id::text
		from public.agents a
		join public.agent_credentials c on c.agent_id = a.id
		where c.revoked_at is null
		  and a.status <> 'revoked'
		  and c.key_hash = extensions.crypt($1, c.key_hash)
		limit 1
	`, agentKey).Scan(&subnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errUnauthorized
		}
		return "", err
	}
	return subnetID, nil
}

func resolveHomeSubnetFromJoinToken(ctx context.Context, pool *pgxpool.Pool, joinToken string) (string, error) {
	var subnetID string
	err := pool.QueryRow(ctx, `
		select id::text
		from public.subnets
		where join_token_hash = extensions.crypt($1, join_token_hash)
		limit 1
	`, joinToken).Scan(&subnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errUnauthorized
		}
		return "", err
	}
	return subnetID, nil
}

func lookupAgentSubnet(ctx context.Context, pool *pgxpool.Pool, agentID string) string {
	if pool == nil || agentID == "" {
		return ""
	}
	var subnetID string
	err := pool.QueryRow(ctx, `
		select subnet_id::text from public.agents where id = $1::uuid limit 1
	`, agentID).Scan(&subnetID)
	if err != nil {
		return ""
	}
	return subnetID
}
