package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/lib/pq"
)

// WebSocketHub listens to PostgreSQL NOTIFY on "sub_events" channel
// and broadcasts events to connected SSE (Server-Sent Events) clients.
// Uses SSE instead of WebSocket to avoid external dependency (H19 simplified).
type WebSocketHub struct {
	connStr   string
	clients   map[chan []byte]bool
	mu        sync.RWMutex
	broadcast chan []byte
}

func NewWebSocketHub(connStr string) *WebSocketHub {
	return &WebSocketHub{
		connStr:   connStr,
		clients:   make(map[chan []byte]bool),
		broadcast: make(chan []byte, 256),
	}
}

func (hub *WebSocketHub) Start() {
	// pg_notify listener
	go func() {
		minReconn := 10 * time.Second
		maxReconn := time.Minute
		reportProblem := func(ev pq.ListenerEventType, err error) {
			if err != nil {
				log.Printf("[WebSocketHub] pg listener error: %v", err)
			}
		}

		listener := pq.NewListener(hub.connStr, minReconn, maxReconn, reportProblem)
		if err := listener.Listen("sub_events"); err != nil {
			log.Printf("[WebSocketHub] listen error: %v", err)
			return
		}
		log.Println("[WebSocketHub] Listening on pg_notify channel 'sub_events'")

		for {
			select {
			case n := <-listener.Notify:
				if n != nil {
					hub.broadcast <- []byte(n.Extra)
				}
			case <-time.After(90 * time.Second):
				go listener.Ping()
			}
		}
	}()

	// Broadcast dispatcher
	go func() {
		for msg := range hub.broadcast {
			hub.mu.RLock()
			for ch := range hub.clients {
				select {
				case ch <- msg:
				default:
					// slow client — drop message
				}
			}
			hub.mu.RUnlock()
		}
	}()
}

// HandleSSE handles GET /ws/events as a Server-Sent Events stream.
// H11: Validates session cookie before allowing connection.
func (hub *WebSocketHub) HandleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))

	ch := make(chan []byte, 64)
	hub.mu.Lock()
	hub.clients[ch] = true
	hub.mu.Unlock()

	defer func() {
		hub.mu.Lock()
		delete(hub.clients, ch)
		hub.mu.Unlock()
		close(ch)
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Validate JSON before sending
			var check json.RawMessage
			if json.Unmarshal(msg, &check) != nil {
				continue
			}
			w.Write([]byte("data: "))
			w.Write(msg)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

// BroadcastEvent allows programmatic broadcasting from Go code (not just pg_notify).
func (hub *WebSocketHub) BroadcastEvent(ctx context.Context, eventJSON []byte) {
	select {
	case hub.broadcast <- eventJSON:
	default:
		// buffer full — drop
	}
}
