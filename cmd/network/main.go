package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const (
	defaultPort = 8080
)

// publicNetworkURL is set from PUBLIC_NETWORK_URL at startup.
var publicNetworkURL = "http://localhost:8080"

var agentNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

func main() {
	_ = godotenv.Load(".env")

	cfg := loadConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool := openDatabase(ctx, cfg.databaseURL)
	if pool != nil {
		defer pool.Close()
	}

	hub := newMessageHub(cfg.redisURL, pool)
	wsHub := newWSHub()
	joinRL := newRateLimiter(30, time.Minute)
	sendRL := newRateLimiter(120, time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.Handle("POST /v1/agents/join", withRateLimit(http.HandlerFunc(joinHandler(pool)), joinRL))
	mux.HandleFunc("GET /v1/agents/ws", wsHandler(pool, hub, wsHub))
	mux.HandleFunc("GET /v1/peers", peersHandler(pool))
	mux.HandleFunc("GET /v1/a2a/agents/{name}/agent-card.json", agentCardHandler(pool))
	mux.HandleFunc("GET /v1/agents/me/agent-card.json", selfAgentCardHandler(pool))
	mux.HandleFunc("POST /v1/a2a/agents/{name}/message:send", a2aMessageSendHandler(pool, hub))
	mux.HandleFunc("POST /v1/a2a/agents/{name}", a2aJSONRPCHandler(pool, hub))
	mux.Handle("POST /v1/messages/send", withRateLimit(http.HandlerFunc(sendHandler(pool, hub)), sendRL))
	mux.HandleFunc("GET /v1/messages/inbox", inboxHandler(pool, hub))
	mux.HandleFunc("POST /v1/messages/ack", ackHandler(pool, hub))
	mux.HandleFunc("GET /v1/messages/status", statusHandler(pool, hub))
	mux.HandleFunc("GET /v1/messages/trace", traceHandler(pool, hub))
	mux.HandleFunc("GET /v1/messages/history", historyHandler(pool))
	mux.HandleFunc("GET /v1/metrics", metricsHandler())
	mux.HandleFunc("GET /v1/wallet", walletHandler)
	mux.HandleFunc("POST /v1/approvals", createApprovalHandler(pool))
	mux.HandleFunc("GET /v1/approvals", listApprovalsHandler(pool))
	mux.HandleFunc("GET /v1/approvals/{id}", getApprovalHandler(pool))

	server := &http.Server{
		Addr:              cfg.listenAddr + ":" + strconv.Itoa(cfg.port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("network listening on %s:%d", cfg.listenAddr, cfg.port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}
	log.Println("network stopped")
}

type config struct {
	port        int
	listenAddr  string
	databaseURL string
	redisURL    string
}

func loadConfig() config {
	portStr := envOrDefault("PORT", strconv.Itoa(defaultPort))
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		log.Printf("invalid PORT=%q, falling back to %d", portStr, defaultPort)
		port = defaultPort
	}

	// Default 0.0.0.0 for Docker/self-host; override with LISTEN_ADDR=127.0.0.1 if needed.
	listenAddr := strings.TrimSpace(getEnv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}

	databaseURL := getEnv("DATABASE_URL")
	if databaseURL == "" {
		log.Println("DATABASE_URL not set; join API will be unavailable")
	}

	pub := strings.TrimSpace(getEnv("PUBLIC_NETWORK_URL"))
	if pub == "" {
		pub = "http://localhost:8080"
	}
	publicNetworkURL = strings.TrimRight(pub, "/")

	return config{
		port:        port,
		listenAddr:  listenAddr,
		databaseURL: databaseURL,
		redisURL:    getEnv("REDIS_URL"),
	}
}

func getEnv(name string) string {
	if v, ok := os.LookupEnv("NETWORK_" + name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func envOrDefault(name, fallback string) string {
	if value := getEnv(name); value != "" {
		return value
	}
	return fallback
}

func openDatabase(ctx context.Context, databaseURL string) *pgxpool.Pool {
	if databaseURL == "" {
		return nil
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Printf("database pool init failed (degraded mode): %v", err)
		return nil
	}

	// A cold start can beat the network or the pooler by a moment, so try a few times before
	// giving up: a relay that starts without its database stays degraded until it restarts.
	const attempts = 5
	for i := 1; i <= attempts; i++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Println("database ping ok")
			return pool
		}
		log.Printf("database ping failed (attempt %d of %d): %v", i, attempts, err)
		if i < attempts {
			time.Sleep(2 * time.Second)
		}
	}
	log.Println("database unreachable; starting in degraded mode")
	pool.Close()
	return nil
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type joinRequest struct {
	Token       string      `json:"token"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Version     string      `json:"version,omitempty"`
	Skills      []joinSkill `json:"skills,omitempty"`
}

type joinResponse struct {
	AgentID      string `json:"agent_id"`
	AgentKey     string `json:"agent_key"`
	SubnetID     string `json:"subnet_id"`
	WSURL        string `json:"ws_url"`
	AgentCardURL string `json:"agent_card_url"`
}

func joinHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "database unavailable",
			})
			return
		}

		var req joinRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}

		token := strings.TrimSpace(req.Token)
		name := strings.TrimSpace(req.Name)
		if token == "" || name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "token and name are required",
			})
			return
		}
		if !agentNameRe.MatchString(name) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "name must be 1-32 chars: letters, numbers, _ or -",
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		resp, err := joinAgent(ctx, pool, joinAgentInput{
			Token:       token,
			Name:        name,
			Description: strings.TrimSpace(req.Description),
			Version:     strings.TrimSpace(req.Version),
			Skills:      req.Skills,
		})
		if err != nil {
			switch {
			case errors.Is(err, errInvalidToken):
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid join token"})
			default:
				log.Printf("join failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "join failed"})
			}
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

var errInvalidToken = errors.New("invalid token")

type joinAgentInput struct {
	Token       string
	Name        string
	Description string
	Version     string
	Skills      []joinSkill
}

func joinAgent(ctx context.Context, pool *pgxpool.Pool, in joinAgentInput) (*joinResponse, error) {
	token := strings.TrimSpace(in.Token)
	name := strings.TrimSpace(in.Name)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var subnetID string
	err = tx.QueryRow(ctx, `
		select id::text
		from public.subnets
		where join_token_hash = extensions.crypt($1, join_token_hash)
		limit 1
	`, token).Scan(&subnetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errInvalidToken
		}
		return nil, err
	}

	var existingID string
	err = tx.QueryRow(ctx, `
		select id::text
		from public.agents
		where subnet_id = $1::uuid and name = $2
		limit 1
	`, subnetID, name).Scan(&existingID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hasExisting := err == nil

	agentKey, err := randomToken("mak_")
	if err != nil {
		return nil, err
	}

	var agentID string
	if hasExisting {
		agentID = existingID
		_, err = tx.Exec(ctx, `
			update public.agents
			set status = 'online', last_seen_at = now()
			where id = $1::uuid
		`, agentID)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `
			insert into public.agent_credentials (agent_id, key_hash, revoked_at)
			values ($1::uuid, extensions.crypt($2, extensions.gen_salt('bf')), null)
			on conflict (agent_id) do update
			set key_hash = excluded.key_hash, revoked_at = null
		`, agentID, agentKey)
		if err != nil {
			return nil, err
		}
	} else {
		err = tx.QueryRow(ctx, `
			insert into public.agents (subnet_id, name, status, last_seen_at)
			values ($1::uuid, $2, 'online', now())
			returning id::text
		`, subnetID, name).Scan(&agentID)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `
			insert into public.agent_credentials (agent_id, key_hash)
			values ($1::uuid, extensions.crypt($2, extensions.gen_salt('bf')))
		`, agentID, agentKey)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	liveStatus := "online"
	cardJSON, err := marshalAgentCard(agentCardInput{
		AgentID:     agentID,
		Name:        name,
		SubnetID:    subnetID,
		Description: in.Description,
		Version:     in.Version,
		Skills:      in.Skills,
		Status:      liveStatus,
	})
	if err != nil {
		return nil, err
	}
	_, err = pool.Exec(ctx, `
		update public.agents
		set agent_card = $2::jsonb
		where id = $1::uuid
	`, agentID, cardJSON)
	if err != nil {
		log.Printf("agent_card update failed for %s: %v", agentID, err)
	}

	baseURL := strings.TrimSuffix(publicNetworkURL, "/")
	return &joinResponse{
		AgentID:      agentID,
		AgentKey:     agentKey,
		SubnetID:     subnetID,
		WSURL:        toWSURL(publicNetworkURL) + "/v1/agents/ws",
		AgentCardURL: fmt.Sprintf("%s/v1/a2a/agents/%s/agent-card.json", baseURL, name),
	}, nil
}

type peer struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	SubnetID      string         `json:"subnet_id,omitempty"`
	SeenAt        *string        `json:"last_seen_at,omitempty"`
	UsageAllowed  string         `json:"usage_allowed,omitempty"`
	UsageDenied   string         `json:"usage_denied,omitempty"`
	AgentCard     map[string]any `json:"agent_card,omitempty"`
}

func peersHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			writeJSON(w, http.StatusOK, map[string]any{"peers": []peer{}})
			return
		}

		token := strings.TrimSpace(r.Header.Get("Authorization"))
		token = strings.TrimPrefix(token, "Bearer ")
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("agent_key"))
		}
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if strings.HasPrefix(token, "mak_") {
			agent, err := resolveAgentKey(ctx, pool, token)
			if err != nil {
				if errors.Is(err, errUnauthorized) {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
					return
				}
				log.Printf("peers auth failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "peers failed"})
				return
			}
			if err := agentCan(ctx, pool, agent.ID, permPeers, true, false); err != nil {
				if errors.Is(err, errForbidden) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied", "code": "forbidden"})
					return
				}
				log.Printf("peers permission check failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "peers failed"})
				return
			}
		}

		var rows pgx.Rows
		var err error
		var homeSubnetID string

		if strings.HasPrefix(token, "mak_") {
			homeSubnetID, err = resolveHomeSubnetFromAgentKey(ctx, pool, token)
		} else {
			homeSubnetID, err = resolveHomeSubnetFromJoinToken(ctx, pool, token)
		}
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
				return
			}
			log.Printf("peers subnet resolve failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "peers failed"})
			return
		}

		rows, err = pool.Query(ctx, `
			select a.id::text, a.name, a.status, a.last_seen_at, a.subnet_id::text, a.agent_card, a.usage_allowed, a.usage_denied
			from public.agents a
			where a.subnet_id in (
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
			  and a.status <> 'revoked'
			order by a.name
		`, homeSubnetID)
		if err != nil {
			log.Printf("peers query failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "peers failed"})
			return
		}
		defer rows.Close()

		peers := make([]peer, 0)
		for rows.Next() {
			var p peer
			var seen *time.Time
			var subnetID string
			var cardRaw []byte
			if err := rows.Scan(&p.ID, &p.Name, &p.Status, &seen, &subnetID, &cardRaw, &p.UsageAllowed, &p.UsageDenied); err != nil {
				log.Printf("peers scan failed: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "peers failed"})
				return
			}
			p.SubnetID = subnetID
			// Presence is live if the agent polled recently (bridge heartbeat).
			if seen != nil && time.Since(seen.UTC()) < 2*time.Minute {
				p.Status = "online"
			} else if p.Status != "revoked" {
				p.Status = "offline"
			}
			if seen != nil {
				s := seen.UTC().Format(time.RFC3339)
				p.SeenAt = &s
			}
			stored, err := parseStoredCard(cardRaw)
			if err != nil {
				log.Printf("agent_card parse failed for %s: %v", p.ID, err)
			}
			if stored != nil {
				p.AgentCard = cardWithLiveStatus(stored, p.Status, p.ID, subnetID)
			} else {
				fallback, err := marshalAgentCard(agentCardInput{
					AgentID:  p.ID,
					Name:     p.Name,
					SubnetID: subnetID,
					Status:   p.Status,
				})
				if err == nil {
					stored, _ = parseStoredCard(fallback)
					p.AgentCard = cardWithLiveStatus(stored, p.Status, p.ID, subnetID)
				}
			}
			peers = append(peers, p)
		}
		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "peers failed"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
	}
}

func randomToken(prefix string) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}


func toWSURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return base
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("json encode failed: %v", err)
	}
}
