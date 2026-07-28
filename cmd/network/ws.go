package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongWait     = 45 * time.Second
	wsWriteWait    = 10 * time.Second
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type wsEvent struct {
	Type     string         `json:"type"`
	Messages []wireMessage  `json:"messages,omitempty"`
	Receipt  *messageReceipt `json:"receipt,omitempty"`
	Agent    string         `json:"agent,omitempty"`
	Status   string         `json:"status,omitempty"`
}

type wsClient struct {
	agentID string
	conn    *websocket.Conn
	send    chan []byte
}

type wsHub struct {
	mu      sync.RWMutex
	clients map[string]map[*wsClient]struct{}
}

func newWSHub() *wsHub {
	return &wsHub{clients: make(map[string]map[*wsClient]struct{})}
}

func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.agentID] == nil {
		h.clients[c.agentID] = make(map[*wsClient]struct{})
	}
	h.clients[c.agentID][c] = struct{}{}
}

func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.clients[c.agentID]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.clients, c.agentID)
	}
}

func (h *wsHub) push(agentID string, payload []byte) {
	h.mu.RLock()
	set := h.clients[agentID]
	clients := make([]*wsClient, 0, len(set))
	for c := range set {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		select {
		case c.send <- payload:
		default:
		}
	}
}

func (h *wsHub) onlineAgents() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.clients))
	for id := range h.clients {
		out = append(out, id)
	}
	return out
}

func (hub *messageHub) bindWS(ws *wsHub) {
	hub.ws = ws
}

func (hub *messageHub) emitWS(agentID string, ev wsEvent) {
	if hub.ws == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	hub.ws.push(agentID, raw)
}

func wsHandler(pool *pgxpool.Pool, msgHub *messageHub, wsHub *wsHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ws auth failed"})
			return
		}

		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws upgrade failed: %v", err)
			return
		}

		client := &wsClient{
			agentID: agent.ID,
			conn:    conn,
			send:    make(chan []byte, 16),
		}
		wsHub.register(client)
		msgHub.bindWS(wsHub)

		hello, _ := json.Marshal(wsEvent{
			Type:   "connected",
			Agent:  agent.Name,
			Status: "online",
		})
		client.send <- hello

		go client.writePump()
		client.readPump(wsHub, agent.Name)
	}
}

func (c *wsClient) readPump(wsHub *wsHub, name string) {
	defer func() {
		wsHub.unregister(c)
		_ = c.conn.Close()
	}()
	_ = c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
	log.Printf("ws disconnected: %s", name)
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingInterval)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
