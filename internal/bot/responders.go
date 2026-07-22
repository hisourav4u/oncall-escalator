package bot

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"escalator/internal/slackapi"
)

// ResponderCache resolves the set of Slack user IDs allowed to acknowledge,
// from configured user IDs + emails + usergroup memberships. Refreshed
// periodically because usergroup membership changes over time.
//
// If no responders are configured at all, Allowed() returns true for everyone
// (any non-bot user's ack counts) — so the bot works out of the box.
type ResponderCache struct {
	slack        *slackapi.Client
	userIDs      []string
	emails       []string
	usergroupIDs []string
	log          *slog.Logger

	mu      sync.RWMutex
	allowed map[string]bool
	openAll bool
}

func NewResponderCache(slack *slackapi.Client, userIDs, emails, usergroupIDs []string, log *slog.Logger) *ResponderCache {
	return &ResponderCache{
		slack:        slack,
		userIDs:      userIDs,
		emails:       emails,
		usergroupIDs: usergroupIDs,
		log:          log,
		openAll:      len(userIDs) == 0 && len(emails) == 0 && len(usergroupIDs) == 0,
		allowed:      map[string]bool{},
	}
}

func (r *ResponderCache) Allowed(userID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.openAll {
		return true
	}
	return r.allowed[userID]
}

// Contains reports strict set membership, ignoring the open-all fallback. Used
// for the on-call set, where "any reply acks" must only apply to people we can
// positively identify as on-call.
func (r *ResponderCache) Contains(userID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allowed[userID]
}

// CombinedAuthorizer implements bot.Authorizer over a responder set (keyword /
// emoji acks) and an on-call set (any-reply acks).
type CombinedAuthorizer struct {
	Responders *ResponderCache
	Oncall     *ResponderCache
}

func (c CombinedAuthorizer) CanAck(userID string) bool {
	return c.Responders.Allowed(userID) || c.Oncall.Contains(userID)
}

func (c CombinedAuthorizer) IsOncall(userID string) bool {
	return c.Oncall.Contains(userID)
}

// Refresh rebuilds the allowed set from Slack. Safe to call on a schedule.
func (r *ResponderCache) Refresh(ctx context.Context) {
	if r.openAll {
		return
	}
	set := map[string]bool{}
	for _, id := range r.userIDs {
		set[id] = true
	}
	for _, email := range r.emails {
		if uid, err := r.slack.LookupUserByEmail(ctx, email); err == nil && uid != "" {
			set[uid] = true
		} else if err != nil {
			r.log.Warn("responder email lookup failed", "email", email, "err", err)
		}
	}
	for _, gid := range r.usergroupIDs {
		if members, err := r.slack.UsergroupMembers(ctx, gid); err == nil {
			for _, uid := range members {
				set[uid] = true
			}
		} else {
			r.log.Warn("responder usergroup lookup failed", "usergroup", gid, "err", err)
		}
	}
	r.mu.Lock()
	r.allowed = set
	r.mu.Unlock()
	r.log.Info("responders refreshed", "count", len(set))
}

// RunRefresh refreshes on an interval until ctx is cancelled.
func (r *ResponderCache) RunRefresh(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Refresh(ctx)
		}
	}
}

// TargetResolver renders the escalation target into a Slack mention string,
// caching email→ID lookups. Subteam IDs and user IDs need no lookup.
type TargetResolver struct {
	slack   *slackapi.Client
	targets []string
	log     *slog.Logger

	mu     sync.Mutex
	cached string
}

func NewTargetResolver(slack *slackapi.Client, targets []string, log *slog.Logger) *TargetResolver {
	return &TargetResolver{slack: slack, targets: targets, log: log}
}

// Mention returns the rendered @-mention string, e.g. "<!subteam^S1> <@U2>".
func (t *TargetResolver) Mention(ctx context.Context) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != "" {
		return t.cached
	}
	var parts []string
	for _, tg := range t.targets {
		switch {
		case strings.HasPrefix(tg, "S"): // subteam / usergroup
			parts = append(parts, "<!subteam^"+tg+">")
		case strings.HasPrefix(tg, "U") || strings.HasPrefix(tg, "W"): // user id
			parts = append(parts, "<@"+tg+">")
		case strings.Contains(tg, "@"): // email
			if uid, err := t.slack.LookupUserByEmail(ctx, tg); err == nil && uid != "" {
				parts = append(parts, "<@"+uid+">")
			} else {
				t.log.Warn("escalation target email lookup failed", "email", tg, "err", err)
			}
		default:
			parts = append(parts, tg)
		}
	}
	t.cached = strings.Join(parts, " ")
	if t.cached == "" {
		t.cached = "the escalation contacts"
	}
	return t.cached
}
