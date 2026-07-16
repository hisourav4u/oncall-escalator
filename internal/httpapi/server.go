package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"escalator/internal/bot"
	"escalator/internal/slackapi"
	"escalator/internal/store"
)

type Server struct {
	bot           *bot.Bot
	store         *store.Store
	slack         *slackapi.Client
	signingSecret string
	statsToken    string
	log           *slog.Logger

	nameMu sync.Mutex
	names  map[string]string
}

func New(b *bot.Bot, st *store.Store, slack *slackapi.Client, signingSecret, statsToken string, log *slog.Logger) *Server {
	return &Server{
		bot: b, store: st, slack: slack,
		signingSecret: signingSecret, statsToken: statsToken, log: log,
		names: map[string]string{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /slack/events", s.handleEvents)
	mux.HandleFunc("GET /stats", s.handleStats)
	return mux
}

// stats proxies to the store (kept as a method so the stats handler stays clean).
func (s *Server) stats(ctx context.Context, window time.Duration) (*store.Metrics, error) {
	return s.store.Stats(ctx, window)
}

// nameFor resolves a Slack user ID to a display name, cached for the process.
func (s *Server) nameFor(ctx context.Context, userID string) string {
	s.nameMu.Lock()
	if n, ok := s.names[userID]; ok {
		s.nameMu.Unlock()
		return n
	}
	s.nameMu.Unlock()
	n := s.slack.UserName(ctx, userID)
	s.nameMu.Lock()
	s.names[userID] = n
	s.nameMu.Unlock()
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const maxBody = 1 << 20

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !slackapi.VerifySignature(s.signingSecret, r, body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	var env slackapi.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	if env.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(env.Challenge))
		return
	}

	// A retry header means Slack previously failed to deliver this event — log
	// it at INFO so delivery trouble is visible without DEBUG.
	if rn := r.Header.Get("X-Slack-Retry-Num"); rn != "" {
		s.log.Info("slack event RETRY", "num", rn, "reason", r.Header.Get("X-Slack-Retry-Reason"), "event", env.EventType())
	}

	switch env.EventType() {
	case "message":
		if m, err := env.Message(); err == nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				s.bot.HandleMessage(ctx, m)
			}()
		}
	case "reaction_added":
		if e, err := env.Reaction(); err == nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				s.bot.HandleReaction(ctx, e)
			}()
		}
	}
	w.WriteHeader(http.StatusOK)
}
