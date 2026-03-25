package api

import (
	"database/sql"
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleBotTrap returns a handler for the honeypot verification link.
// When a bot follows the hidden link, the subscriber is flagged as a bot
// and the detection timestamp is recorded. Human users never see the link
// (it is positioned off-screen with zero opacity in the email HTML).
func HandleBotTrap(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if token == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		decoded, err := base64.URLEncoding.DecodeString(token)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		subscriberID := strings.TrimSpace(string(decoded))
		if _, err := uuid.Parse(subscriberID); err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		now := time.Now().UTC()
		result, err := db.ExecContext(r.Context(), `
			UPDATE mailing_subscribers
			SET is_bot = true, bot_detected_at = $2, updated_at = $2
			WHERE id = $1 AND (is_bot = false OR is_bot IS NULL)
		`, subscriberID, now)
		if err != nil {
			log.Printf("[bot-trap] DB error flagging subscriber %s: %v", subscriberID, err)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if rows, _ := result.RowsAffected(); rows > 0 {
			log.Printf("[bot-trap] Flagged subscriber %s as bot at %s (User-Agent: %s, IP: %s)",
				subscriberID, now.Format(time.RFC3339), r.UserAgent(), r.RemoteAddr)
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
