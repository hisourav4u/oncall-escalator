package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is sourced entirely from environment variables so the bot stays a
// single, stateless-to-configure binary.
type Config struct {
	Port   string
	DBPath string

	SlackBotToken      string
	SlackSigningSecret string

	// Optional token that gates GET /stats (?token=…). If empty, /stats is off.
	StatsToken string

	// Trigger: mention of any of these Slack usergroups (subteam IDs, e.g.
	// S0123ABCD) starts an escalation timer. Optionally also match literal
	// keywords in message text.
	WatchUsergroupIDs []string
	WatchKeywords     []string

	// Only arm when the mention is a top-level (thread-starting) message; ignore
	// mentions that are replies inside an existing thread. Default false.
	TriggerTopLevelOnly bool
	// If the mention message *starts* with one of these words, treat it as
	// gratitude/closing/informational and do NOT arm (e.g. "thanks @devops-oncall").
	TriggerIgnoreLeading []string

	// Two-tier timing (from the triggering message):
	//   NudgeAfter    — DM the on-call human a reminder (default 15m).
	//   EscalateAfter — publicly escalate to the wider team (default 30m).
	NudgeAfter    time.Duration
	EscalateAfter time.Duration

	// How to deliver the 15-min nudge: "dm" (private, needs im:write) or
	// "thread_mention" (public @-ping in the thread; no extra scope).
	NudgeMode string
	// User IDs to never nudge (in addition to auto-skipping bot members like
	// the incident.io account).
	NudgeExcludeUserIDs []string

	// Escalation target(s): each entry is a subteam ID (S…), user ID (U…),
	// or an email (resolved to a user at send time).
	EscalationTargets []string

	// Who may acknowledge. Union of these resolves to a set of Slack user IDs.
	// If ALL are empty, any non-bot user's ack counts.
	ResponderUsergroupIDs []string
	ResponderUserIDs      []string
	ResponderEmails       []string

	// What counts as an ack.
	AckKeywords []string
	AckEmojis   []string

	// When true, ANY thread reply from a member of a watched (on-call) usergroup
	// counts as an ack — they've clearly engaged, so escalating would be noise.
	AckAnyOncallReply bool

	// Users whose ANY thread reply counts as an ack, whatever the wording.
	// Narrower than AckAnyOncallReply: this is a named list rather than whoever
	// happens to be in the usergroup, so it survives rota changes.
	AckAnyReplyUserIDs []string

	TickInterval     time.Duration
	ResponderRefresh time.Duration

	// Reconciliation safety net (catches dropped Slack events).
	ReconcileInterval time.Duration
	ReconcileWindow   time.Duration
	ReconcileChannels []string

	LogLevel string // debug | info | warn | error
}

func Load() (*Config, error) {
	c := &Config{
		Port:                  env("PORT", "8090"),
		DBPath:                env("DB_PATH", "escalator.db"),
		SlackBotToken:         env("SLACK_BOT_TOKEN", ""),
		SlackSigningSecret:    env("SLACK_SIGNING_SECRET", ""),
		StatsToken:            env("STATS_TOKEN", ""),
		WatchUsergroupIDs:     list(env("WATCH_USERGROUP_IDS", "")),
		WatchKeywords:         list(env("WATCH_KEYWORDS", "")),
		TriggerTopLevelOnly:   envBool("TRIGGER_TOPLEVEL_ONLY", false),
		TriggerIgnoreLeading:  list(env("TRIGGER_IGNORE_LEADING", "thanks,thank,thankyou,thx,ty,tysm,tq,cheers,appreciate,appreciated,kudos,nvm,resolved,sorted,fixed,done,fyi,cc,np")),
		NudgeAfter:            dur("NUDGE_AFTER", 15*time.Minute),
		EscalateAfter:         dur("ESCALATE_AFTER", 30*time.Minute),
		NudgeMode:             env("NUDGE_MODE", "dm"),
		NudgeExcludeUserIDs:   list(env("NUDGE_EXCLUDE_USER_IDS", "")),
		EscalationTargets:     list(env("ESCALATION_TARGETS", "")),
		ResponderUsergroupIDs: list(env("RESPONDER_USERGROUP_IDS", "")),
		ResponderUserIDs:      list(env("RESPONDER_USER_IDS", "")),
		ResponderEmails:       list(env("RESPONDER_EMAILS", "")),
		AckKeywords:           list(env("ACK_KEYWORDS", "ack,acked,ack'd,ackd,acking,looking,looking into it,look into it,on it,on-it,onit,got it,got this,i got it,i've got it,i got this,taking,taking it,taking this,i'll take,ill take,take a look,taking a look,picking up,picking this up,i'll look,ill look,i'll check,checking,investigating,investigate,triaging,triage,handling,i'll handle,on my way,omw,looking now,working on it,on the case,wilco,roger,roger that,copy,copy that,responding,will look,will check,will do,:ack:,:on-it-dani:,:eyes:")),
		AckEmojis:             list(env("ACK_EMOJIS", "white_check_mark,heavy_check_mark,ballot_box_with_check,eyes,+1,thumbsup,raised_hand,raised_hands,wave,ok_hand,saluting_face,muscle,pray,running,fire,hammer_and_wrench,detective,hourglass_flowing_sand,ack,on-it-dani")),
		AckAnyOncallReply:     envBool("ACK_ANY_ONCALL_REPLY", false),
		AckAnyReplyUserIDs:    list(env("ACK_ANY_REPLY_USER_IDS", "")),
		TickInterval:          dur("TICK_INTERVAL", 15*time.Second),
		ResponderRefresh:      dur("RESPONDER_REFRESH", 10*time.Minute),
		ReconcileInterval:     dur("RECONCILE_INTERVAL", 60*time.Second),
		ReconcileWindow:       dur("RECONCILE_WINDOW", 0), // 0 => EscalateAfter+5m in the bot
		ReconcileChannels:     list(env("RECONCILE_CHANNELS", "")),
		LogLevel:              env("LOG_LEVEL", "info"),
	}
	if c.SlackBotToken == "" {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN is required")
	}
	if len(c.WatchUsergroupIDs) == 0 && len(c.WatchKeywords) == 0 {
		return nil, fmt.Errorf("set WATCH_USERGROUP_IDS (or WATCH_KEYWORDS) — nothing to watch")
	}
	if len(c.EscalationTargets) == 0 {
		return nil, fmt.Errorf("ESCALATION_TARGETS is required")
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok {
		switch v {
		case "1", "true", "TRUE", "True", "yes":
			return true
		case "0", "false", "FALSE", "False", "no":
			return false
		}
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// list splits a comma-separated env value, trimming and dropping empties.
func list(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
