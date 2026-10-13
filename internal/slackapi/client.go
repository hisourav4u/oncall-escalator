package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Client is a minimal Slack Web API client — just the calls this bot needs.
type Client struct {
	token string
	http  *http.Client
}

func New(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 10 * time.Second}}
}

// PostThread posts a message into a thread.
func (c *Client) PostThread(ctx context.Context, channel, threadTS, text string) error {
	return c.postMessage(ctx, channel, threadTS, text)
}

func (c *Client) postMessage(ctx context.Context, channel, threadTS, text string) error {
	m := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		m["thread_ts"] = threadTS
	}
	body, _ := json.Marshal(m)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/chat.postMessage", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("chat.postMessage: %s", out.Error)
	}
	return nil
}

// PostDM opens a DM with a user and posts a message (needs im:write scope).
func (c *Client) PostDM(ctx context.Context, userID, text string) error {
	body, _ := json.Marshal(map[string]any{"users": userID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/conversations.open", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("conversations.open: %s", out.Error)
	}
	return c.postMessage(ctx, out.Channel.ID, "", text)
}

// Permalink returns a clickable link to a message.
func (c *Client) Permalink(ctx context.Context, channel, ts string) (string, error) {
	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		Permalink string `json:"permalink"`
	}
	if err := c.get(ctx, "chat.getPermalink",
		url.Values{"channel": {channel}, "message_ts": {ts}}, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("chat.getPermalink: %s", out.Error)
	}
	return out.Permalink, nil
}

// IsBot reports whether a user is a bot/app account.
func (c *Client) IsBot(ctx context.Context, userID string) (bool, error) {
	_, isBot, err := c.userInfo(ctx, userID)
	return isBot, err
}

// HistMessage is a message as returned by history/replies reads, including any
// reactions (used by the reconciliation safety net).
type HistMessage struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
	Reactions   []struct {
		Name  string   `json:"name"`
		Users []string `json:"users"`
	} `json:"reactions"`
}

// ChannelHistory returns top-level messages since `oldest` (unix seconds string).
func (c *Client) ChannelHistory(ctx context.Context, channel, oldest string) ([]HistMessage, error) {
	var out struct {
		OK       bool          `json:"ok"`
		Error    string        `json:"error"`
		Messages []HistMessage `json:"messages"`
	}
	if err := c.get(ctx, "conversations.history",
		url.Values{"channel": {channel}, "oldest": {oldest}, "limit": {"200"}}, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("conversations.history: %s", out.Error)
	}
	return out.Messages, nil
}

// ThreadReplies returns replies in a thread (including the root), with reactions.
func (c *Client) ThreadReplies(ctx context.Context, channel, threadTS string) ([]HistMessage, error) {
	var out struct {
		OK       bool          `json:"ok"`
		Error    string        `json:"error"`
		Messages []HistMessage `json:"messages"`
	}
	if err := c.get(ctx, "conversations.replies",
		url.Values{"channel": {channel}, "ts": {threadTS}, "limit": {"200"}}, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("conversations.replies: %s", out.Error)
	}
	return out.Messages, nil
}

// UsergroupMembers returns the user IDs belonging to a usergroup (subteam).
func (c *Client) UsergroupMembers(ctx context.Context, usergroupID string) ([]string, error) {
	var out struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		Users []string `json:"users"`
	}
	if err := c.get(ctx, "usergroups.users.list", url.Values{
		"usergroup":        {usergroupID},
		"include_disabled": {"false"},
	}, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("usergroups.users.list: %s", out.Error)
	}
	return out.Users, nil
}

// LookupUserByEmail resolves an email to a Slack user ID.
func (c *Client) LookupUserByEmail(ctx context.Context, email string) (string, error) {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := c.get(ctx, "users.lookupByEmail", url.Values{"email": {email}}, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("users.lookupByEmail(%s): %s", email, out.Error)
	}
	return out.User.ID, nil
}

// UserName resolves a user ID to a display/real name (falls back to the ID).
func (c *Client) UserName(ctx context.Context, userID string) string {
	name, _, err := c.userInfo(ctx, userID)
	if err != nil || name == "" {
		return userID
	}
	return name
}

func (c *Client) userInfo(ctx context.Context, userID string) (name string, isBot bool, err error) {
	var out struct {
		OK   bool `json:"ok"`
		User struct {
			RealName string `json:"real_name"`
			Name     string `json:"name"`
			IsBot    bool   `json:"is_bot"`
			Profile  struct {
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.get(ctx, "users.info", url.Values{"user": {userID}}, &out); err != nil {
		return "", false, err
	}
	if !out.OK {
		return "", false, fmt.Errorf("users.info: user %s", userID)
	}
	name = out.User.Profile.DisplayName
	if name == "" {
		name = out.User.RealName
	}
	if name == "" {
		name = out.User.Name
	}
	return name, out.User.IsBot, nil
}

func (c *Client) get(ctx context.Context, method string, q url.Values, dst any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://slack.com/api/"+method+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(dst)
}
