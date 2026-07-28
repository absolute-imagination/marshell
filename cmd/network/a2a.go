package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	a2aProtocolVersion      = "1.0"
	marshellProtocolVersion = "marshell/v1"
	marshellRelayExtension  = "https://marshell.dev/extensions/relay/v1"
	defaultSkillID          = "marshell.messaging"
)

type joinSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type agentCardInput struct {
	AgentID     string
	Name        string
	SubnetID    string
	Description string
	Version     string
	Skills      []joinSkill
	Status      string
}

func buildAgentCard(in agentCardInput) map[string]any {
	name := strings.TrimSpace(in.Name)
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = fmt.Sprintf("Marshell mesh agent %q", name)
	}
	version := strings.TrimSpace(in.Version)
	if version == "" {
		version = "0.0.0"
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "offline"
	}

	skills := in.Skills
	if len(skills) == 0 {
		skills = []joinSkill{{
			ID:          defaultSkillID,
			Name:        "Peer messaging",
			Description: "Send and receive text messages via the Marshell relay",
			Tags:        []string{"marshell", "messaging"},
		}}
	}

	skillMaps := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			continue
		}
		entry := map[string]any{
			"id":   id,
			"name": strings.TrimSpace(s.Name),
		}
		if d := strings.TrimSpace(s.Description); d != "" {
			entry["description"] = d
		}
		if len(s.Tags) > 0 {
			entry["tags"] = s.Tags
		}
		skillMaps = append(skillMaps, entry)
	}
	if len(skillMaps) == 0 {
		skillMaps = []map[string]any{{
			"id":          defaultSkillID,
			"name":        "Peer messaging",
			"description": "Send and receive text messages via the Marshell relay",
			"tags":        []string{"marshell", "messaging"},
		}}
	}

	baseURL := strings.TrimSuffix(publicNetworkURL, "/")
	interfaceURL := fmt.Sprintf("%s/v1/a2a/agents/%s", baseURL, name)

	return map[string]any{
		"name":        name,
		"description": desc,
		"version":     version,
		"supportedInterfaces": []map[string]any{{
			"url":              interfaceURL,
			"protocolBinding":  "HTTP+JSON",
			"protocolVersion":  a2aProtocolVersion,
		}},
		"capabilities": map[string]any{
			"streaming": false,
			"extensions": []map[string]any{{
				"uri":         marshellRelayExtension,
				"description": "Subnet mesh relay, presence, and delivery receipts",
				"params": map[string]any{
					"agent_id":  in.AgentID,
					"subnet_id": in.SubnetID,
					"status":    status,
					"protocol":  marshellProtocolVersion,
				},
			}},
		},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"skills":             skillMaps,
		"securitySchemes": map[string]any{
			"agentKey": map[string]any{
				"type":        "http",
				"scheme":      "bearer",
				"description": "Marshell agent key (mak_…)",
			},
		},
		"securityRequirements": []map[string]any{
			{"agentKey": []any{}},
		},
	}
}

func marshalAgentCard(in agentCardInput) ([]byte, error) {
	card := buildAgentCard(in)
	raw, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func parseStoredCard(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		return nil, err
	}
	return card, nil
}

func cardWithLiveStatus(card map[string]any, status, agentID, subnetID string) map[string]any {
	if card == nil {
		return nil
	}
	out := make(map[string]any, len(card)+4)
	for k, v := range card {
		out[k] = v
	}
	caps, _ := out["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
		out["capabilities"] = caps
	}
	exts, _ := caps["extensions"].([]any)
	if len(exts) == 0 {
		return out
	}
	updated := make([]any, len(exts))
	for i, ext := range exts {
		em, ok := ext.(map[string]any)
		if !ok {
			updated[i] = ext
			continue
		}
		copyExt := make(map[string]any, len(em)+2)
		for k, v := range em {
			copyExt[k] = v
		}
		uri, _ := em["uri"].(string)
		if uri == marshellRelayExtension {
			params, _ := em["params"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			nextParams := make(map[string]any, len(params)+3)
			for k, v := range params {
				nextParams[k] = v
			}
			nextParams["status"] = status
			if agentID != "" {
				nextParams["agent_id"] = agentID
			}
			if subnetID != "" {
				nextParams["subnet_id"] = subnetID
			}
			copyExt["params"] = nextParams
		}
		updated[i] = copyExt
	}
	caps["extensions"] = updated
	return out
}

func agentCardHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "database unavailable",
			})
			return
		}

		name := strings.TrimSpace(r.PathValue("name"))
		if name == "" || !agentNameRe.MatchString(name) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent name"})
			return
		}

		token := bearerToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		card, err := loadAgentCardByName(ctx, pool, token, name)
		if err != nil {
			switch {
			case errors.Is(err, errInvalidToken):
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			case errors.Is(err, errAgentNotFound):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
			default:
				log.Printf("agent card failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "agent card failed"})
			}
			return
		}

		writeJSON(w, http.StatusOK, card)
	}
}

func selfAgentCardHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "database unavailable",
			})
			return
		}

		token := bearerToken(r)
		if !strings.HasPrefix(token, "mak_") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		card, err := loadAgentCardForKey(ctx, pool, token)
		if err != nil {
			switch {
			case errors.Is(err, errInvalidToken):
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
			default:
				log.Printf("self agent card failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "agent card failed"})
			}
			return
		}

		writeJSON(w, http.StatusOK, card)
	}
}

var errAgentNotFound = errors.New("agent not found")

func loadAgentCardForKey(ctx context.Context, pool *pgxpool.Pool, agentKey string) (map[string]any, error) {
	var (
		agentID  string
		name     string
		subnetID string
		status   string
		seen     *time.Time
		cardRaw  []byte
	)
	err := pool.QueryRow(ctx, `
		select a.id::text, a.name, a.subnet_id::text, a.status, a.last_seen_at, a.agent_card
		from public.agents a
		join public.agent_credentials c on c.agent_id = a.id
		where c.revoked_at is null
		  and a.status <> 'revoked'
		  and c.key_hash = extensions.crypt($1, c.key_hash)
		limit 1
	`, agentKey).Scan(&agentID, &name, &subnetID, &status, &seen, &cardRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errInvalidToken
		}
		return nil, err
	}
	return materializePeerCard(agentID, name, subnetID, status, seen, cardRaw)
}

func loadAgentCardByName(ctx context.Context, pool *pgxpool.Pool, token, name string) (map[string]any, error) {
	subnetID, err := resolveSubnetID(ctx, pool, token)
	if err != nil {
		return nil, err
	}

	var (
		agentID        string
		agentSubnetID  string
		status         string
		seen           *time.Time
		cardRaw        []byte
	)
	err = pool.QueryRow(ctx, `
		select a.id::text, a.status, a.last_seen_at, a.agent_card, a.subnet_id::text
		from public.agents a
		where lower(a.name) = lower($2)
		  and a.status <> 'revoked'
		  and a.subnet_id in (
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
		order by case when a.subnet_id = $1::uuid then 0 else 1 end
		limit 1
	`, subnetID, name).Scan(&agentID, &status, &seen, &cardRaw, &agentSubnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errAgentNotFound
		}
		return nil, err
	}
	return materializePeerCard(agentID, name, agentSubnetID, status, seen, cardRaw)
}

func resolveSubnetID(ctx context.Context, pool *pgxpool.Pool, token string) (string, error) {
	var subnetID string
	if strings.HasPrefix(token, "mak_") {
		err := pool.QueryRow(ctx, `
			select a.subnet_id::text
			from public.agents a
			join public.agent_credentials c on c.agent_id = a.id
			where c.revoked_at is null
			  and a.status <> 'revoked'
			  and c.key_hash = extensions.crypt($1, c.key_hash)
			limit 1
		`, token).Scan(&subnetID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", errInvalidToken
			}
			return "", err
		}
		return subnetID, nil
	}

	err := pool.QueryRow(ctx, `
		select id::text
		from public.subnets
		where join_token_hash = extensions.crypt($1, join_token_hash)
		limit 1
	`, token).Scan(&subnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errInvalidToken
		}
		return "", err
	}
	return subnetID, nil
}

func materializePeerCard(agentID, name, subnetID, status string, seen *time.Time, cardRaw []byte) (map[string]any, error) {
	live := status
	if seen != nil && time.Since(seen.UTC()) < 2*time.Minute {
		live = "online"
	} else if live != "revoked" {
		live = "offline"
	}

	stored, err := parseStoredCard(cardRaw)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		fallback, err := marshalAgentCard(agentCardInput{
			AgentID:  agentID,
			Name:     name,
			SubnetID: subnetID,
			Status:   live,
		})
		if err != nil {
			return nil, err
		}
		stored, err = parseStoredCard(fallback)
		if err != nil {
			return nil, err
		}
	}
	return cardWithLiveStatus(stored, live, agentID, subnetID), nil
}
