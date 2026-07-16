package slackapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Envelope is the outer Slack Events API payload.
type Envelope struct {
	Type      string          `json:"type"` // url_verification | event_callback
	Challenge string          `json:"challenge"`
	TeamID    string          `json:"team_id"`
	Event     json.RawMessage `json:"event"`
}

// EventType peeks at the inner event's type without full decoding.
func (e Envelope) EventType() string {
	var t struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(e.Event, &t)
	return t.Type
}

// MessageEvent covers `message` events.
type MessageEvent struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

func (e Envelope) Message() (*MessageEvent, error) {
	var m MessageEvent
	err := json.Unmarshal(e.Event, &m)
	return &m, err
}

// ReactionEvent covers `reaction_added` events.
type ReactionEvent struct {
	Type     string `json:"type"`
	User     string `json:"user"`
	Reaction string `json:"reaction"`
	Item     struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
}

func (e Envelope) Reaction() (*ReactionEvent, error) {
	var r ReactionEvent
	err := json.Unmarshal(e.Event, &r)
	return &r, err
}

// VerifySignature validates Slack's request signature (v0 HMAC-SHA256).
// Returns true (with a warning left to the caller) when secret is empty.
func VerifySignature(secret string, r *http.Request, body []byte) bool {
	if secret == "" {
		return true
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(n, 0)) > 5*time.Minute {
		return false // replay protection
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, string(body))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}
