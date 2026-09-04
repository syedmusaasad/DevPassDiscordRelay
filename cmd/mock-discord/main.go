// cmd/mock-discord: test double for the Discord gateway and REST API.
//
// Implements:
//   - Gateway WebSocket (HELLO/IDENTIFY/READY/heartbeat/dispatch MESSAGE_CREATE, INTERACTION_CREATE)
//   - REST API (create message, edit message, create thread, typing, upload, rate-limit injection)
//   - Control HTTP endpoint (/control/*) for tests to inject events and read relay sends
//
// Usage:
//
//	mock-discord --port 9876 --gateway-port 9877
//
// Environment:
//
//	MOCK_DISCORD_PORT         HTTP port for REST + control (default 9876)
//	MOCK_DISCORD_GATEWAY_PORT WebSocket gateway port (default 9877)
//
// Control endpoints:
//
//	POST /control/dispatch      body: {"t":"MESSAGE_CREATE","d":{...}}  — inject gateway event
//	GET  /control/sent          — return all messages the relay sent (array)
//	POST /control/ratelimit     body: {"path":"/channels/X/messages","retry_after":1.0}
//	DELETE /control/sent        — clear sent log
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ── Gateway protocol constants ───────────────────────────────────────────────

const (
	opDispatch    = 0
	opHeartbeat   = 1
	opIdentify    = 2
	opHello       = 10
	opHeartbeatAck = 11
)

type gatewayMsg struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int            `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// ── Shared state ─────────────────────────────────────────────────────────────

type server struct {
	mu         sync.Mutex
	sent       []json.RawMessage      // everything the relay POSTed to our REST API
	events     chan json.RawMessage    // events to dispatch to connected WS clients
	wsConns    []*websocket.Conn
	ratelimits map[string]float64     // path -> retry_after seconds
	channels   map[string][]storedMsg // channelID -> messages
	nextMsgID  int64
	gwPort     string                 // gateway WS port (for returning in REST gateway endpoint)
}

type storedMsg struct {
	ID        string          `json:"id"`
	ChannelID string          `json:"channel_id"`
	Content   string          `json:"content"`
	Raw       json.RawMessage `json:"_raw,omitempty"`
}

func newServer() *server {
	return &server{
		events:     make(chan json.RawMessage, 64),
		ratelimits: make(map[string]float64),
		channels:   make(map[string][]storedMsg),
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	port := envOrDefault("MOCK_DISCORD_PORT", "9876")
	gwPort := envOrDefault("MOCK_DISCORD_GATEWAY_PORT", "9877")

	srv := newServer()
	srv.gwPort = gwPort

	// Gateway WS server
	gwMux := http.NewServeMux()
	gwMux.HandleFunc("/", srv.handleGateway)
	gwHTTP := &http.Server{Addr: ":" + gwPort, Handler: gwMux}
	go func() {
		log.Printf("mock-discord: gateway ws on :%s", gwPort)
		if err := gwHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// REST + control server
	mux := http.NewServeMux()

	// Discord REST endpoints (discordgo uses these paths)
	mux.HandleFunc("/api/v10/", srv.handleREST)
	mux.HandleFunc("/api/", srv.handleREST)

	// Control endpoints
	mux.HandleFunc("/control/dispatch", srv.handleControlDispatch)
	mux.HandleFunc("/control/sent", srv.handleControlSent)
	mux.HandleFunc("/control/ratelimit", srv.handleControlRatelimit)

	// Gateway URL endpoint (discordgo fetches this before connecting)
	mux.HandleFunc("/api/v10/gateway", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"url":"ws://127.0.0.1:%s"}`, gwPort)
	})
	mux.HandleFunc("/api/gateway", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"url":"ws://127.0.0.1:%s"}`, gwPort)
	})

	log.Printf("mock-discord: REST+control on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// ── Gateway WebSocket ────────────────────────────────────────────────────────

func (s *server) handleGateway(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("gateway: upgrade: %v", err)
		return
	}
	defer conn.Close()

	s.mu.Lock()
	s.wsConns = append(s.wsConns, conn)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for i, c := range s.wsConns {
			if c == conn {
				s.wsConns = append(s.wsConns[:i], s.wsConns[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	// Send HELLO
	hello := gatewayMsg{Op: opHello, D: json.RawMessage(`{"heartbeat_interval":30000}`)}
	if err := conn.WriteJSON(hello); err != nil {
		return
	}

	seq := 0
	dispatch := func(t string, d interface{}) {
		seq++
		seq := seq
		data, _ := json.Marshal(d)
		msg := gatewayMsg{Op: opDispatch, D: data, S: &seq, T: t}
		conn.WriteJSON(msg)
	}

	// Read loop
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var msg gatewayMsg
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Op {
			case opIdentify:
				// Send READY
				dispatch("READY", map[string]interface{}{
					"v":                10,
					"user":             map[string]interface{}{"id": "000000000000000001", "username": "mockbot", "discriminator": "0000", "bot": true},
					"guilds":           []interface{}{},
					"session_id":       "mock-session",
					"resume_gateway_url": "ws://127.0.0.1:0",
					"application":      map[string]interface{}{"id": "000000000000000001", "flags": 0},
				})
			case opHeartbeat:
				conn.WriteJSON(gatewayMsg{Op: opHeartbeatAck})
			}
		}
	}()

	// Dispatch events to this connection
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-readDone:
			return
		case ev := <-s.events:
			var raw gatewayMsg
			if err := json.Unmarshal(ev, &raw); err == nil {
				seq++
				seq := seq
				raw.Op = opDispatch
				raw.S = &seq
				conn.WriteJSON(raw)
			}
		case <-ticker.C:
			conn.WriteJSON(gatewayMsg{Op: opHeartbeatAck})
		}
	}
}

// ── REST API ─────────────────────────────────────────────────────────────────

func (s *server) handleREST(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	w.Header().Set("Content-Type", "application/json")

	// Gateway URL endpoint — discordgo fetches this for any API version (v9, v10, etc.)
	// Handle before incrementing nextMsgID so IDs stay contiguous for messages.
	if (strings.HasSuffix(path, "/gateway") || strings.HasSuffix(path, "/gateway/bot")) && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"url":"ws://127.0.0.1:%s","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":14400000,"max_concurrency":1}}`, s.gwPort)
		return
	}

	// Check rate limit injection
	s.mu.Lock()
	retryAfter, limited := s.ratelimits[path]
	s.mu.Unlock()
	if limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", fmt.Sprintf("%.3f", retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"message":"You are being rate limited.","retry_after":%.3f,"global":false}`, retryAfter)
		return
	}

	// Record the request body
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.mu.Lock()
			s.sent = append(s.sent, body)
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	s.nextMsgID++
	id := fmt.Sprintf("%d", s.nextMsgID)
	s.mu.Unlock()

	switch {
	case contains(path, "/messages") && r.Method == http.MethodPost:
		// Create message
		w.WriteHeader(http.StatusOK)
		msg := map[string]interface{}{
			"id":         id,
			"channel_id": extractChannelID(path),
			"content":    "",
			"type":       0,
			"author":     map[string]interface{}{"id": "000000000000000001", "username": "mockbot", "bot": true},
		}
		json.NewEncoder(w).Encode(msg)

	case contains(path, "/messages/") && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		// Edit message
		w.WriteHeader(http.StatusOK)
		msg := map[string]interface{}{
			"id":         extractMsgIDFromPath(path),
			"channel_id": extractChannelID(path),
			"content":    "",
		}
		json.NewEncoder(w).Encode(msg)

	case contains(path, "/threads") && r.Method == http.MethodPost:
		// Create thread
		w.WriteHeader(http.StatusOK)
		ch := map[string]interface{}{
			"id":   id,
			"name": "thread",
			"type": 11,
		}
		json.NewEncoder(w).Encode(ch)

	case contains(path, "/typing") && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)

	default:
		// Generic 200
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}
}

// ── Control endpoints ─────────────────────────────────────────────────────────

func (s *server) handleControlDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ev json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case s.events <- ev:
	default:
		http.Error(w, "event queue full", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleControlSent(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		out := make([]json.RawMessage, len(s.sent))
		copy(out, s.sent)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	case http.MethodDelete:
		s.mu.Lock()
		s.sent = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleControlRatelimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path       string  `json:"path"`
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if body.RetryAfter <= 0 {
		delete(s.ratelimits, body.Path)
	} else {
		s.ratelimits[body.Path] = body.RetryAfter
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func extractChannelID(path string) string {
	// /api/v10/channels/CHANNEL_ID/messages
	parts := splitPath(path)
	for i, p := range parts {
		if p == "channels" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return "unknown"
}

func extractMsgIDFromPath(path string) string {
	parts := splitPath(path)
	for i, p := range parts {
		if p == "messages" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return "unknown"
}

func splitPath(path string) []string {
	var parts []string
	cur := ""
	for _, c := range path {
		if c == '/' {
			if cur != "" {
				parts = append(parts, cur)
				cur = ""
			}
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func init() {
	_ = strconv.Itoa // keep import used
}
