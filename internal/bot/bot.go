package bot

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"escalator/internal/slackapi"
	"escalator/internal/store"
)

// Notifier is the Slack surface the bot needs. Abstracted for testing.
type Notifier interface {
	PostThread(ctx context.Context, channel, threadTS, text string) error
	PostDM(ctx context.Context, userID, text string) error
	Permalink(ctx context.Context, channel, ts string) (string, error)
	UsergroupMembers(ctx context.Context, usergroupID string) ([]string, error)
	IsBot(ctx context.Context, userID string) (bool, error)
	ChannelHistory(ctx context.Context, channel, oldest string) ([]slackapi.HistMessage, error)
	ThreadReplies(ctx context.Context, channel, threadTS string) ([]slackapi.HistMessage, error)
}

// Authorizer decides who may acknowledge. Abstracted so the bot doesn't depend
// on live Slack in tests.
//   - CanAck: may ack via an ack keyword or ack reaction (wider responder set).
//   - IsOncall: a member of a watched on-call usergroup — ANY thread reply from
//     them counts as engagement (see AckAnyOncallReply).
type Authorizer interface {
	CanAck(userID string) bool
	IsOncall(userID string) bool
}

// Config is the subset of settings the bot logic needs.
type Config struct {
	WatchUsergroupIDs    []string
	WatchKeywords        []string
	TriggerTopLevelOnly  bool
	TriggerIgnoreLeading []string
	AckKeywords          []string
	AckEmojis            map[string]bool
	NudgeAfter           time.Duration
	EscalateAfter        time.Duration
	NudgeMode            string
	NudgeExclude         map[string]bool
	AckAnyOncallReply    bool
	AckAnyReplyUsers     map[string]bool
	// Reconciliation safety net.
	ReconcileInterval time.Duration
	ReconcileWindow   time.Duration // how far back to scan for missed mentions
	ReconcileChannels []string      // explicit channels (in addition to seen)
	// EscalationMention resolves the rendered @-mention string for the target,
	// e.g. "<!subteam^S123> <@U456>". Called at escalation time.
	EscalationMention func(ctx context.Context) string
}

type Bot struct {
	cfg    Config
	store  *store.Store
	notify Notifier
	auth   Authorizer
	log    *slog.Logger
	now    func() time.Time

	seenMu sync.Mutex
	seen   map[string]bool // channels the bot has received any event from
}

func New(cfg Config, st *store.Store, notify Notifier, auth Authorizer, log *slog.Logger) *Bot {
	b := &Bot{cfg: cfg, store: st, notify: notify, auth: auth, log: log, now: time.Now, seen: map[string]bool{}}
	// Seed with any explicitly-configured reconcile channels.
	for _, ch := range cfg.ReconcileChannels {
		b.seen[ch] = true
	}
	return b
}

// markSeen records a channel the bot has received an event from, so the
// reconciliation safety net knows to scan it.
func (b *Bot) markSeen(channel string) {
	if channel == "" {
		return
	}
	b.seenMu.Lock()
	b.seen[channel] = true
	b.seenMu.Unlock()
}

func (b *Bot) seenChannels() []string {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	out := make([]string, 0, len(b.seen))
	for ch := range b.seen {
		out = append(out, ch)
	}
	return out
}

// HandleMessage processes a Slack `message` event: it may start a new timer
// (trigger) or acknowledge an existing one (ack reply).
func (b *Bot) HandleMessage(ctx context.Context, m *slackapi.MessageEvent) {
	b.markSeen(m.Channel)
	b.log.Debug("event: message", "channel", m.Channel, "ts", m.TS,
		"user", m.User, "bot_id", m.BotID, "subtype", m.Subtype, "thread_ts", m.ThreadTS)

	if m.BotID != "" || m.User == "" {
		return
	}
	if m.Subtype != "" && m.Subtype != "thread_broadcast" {
		if b.isTrigger(m.Text) {
			b.log.Info("mention skipped: message subtype", "channel", m.Channel, "ts", m.TS, "subtype", m.Subtype)
		}
		return
	}

	// 1) Trigger (mentions the watched on-call group)?
	if b.isTrigger(m.Text) {
		b.tryArm(ctx, m.Channel, m.TS, m.ThreadTS, m.Text, m.User, b.now(), "event")
		return // a trigger message isn't also an ack
	}

	// 2) Ack of an existing escalation? Only replies can ack.
	if m.ThreadTS == "" {
		return
	}
	if b.auth.IsOncall(m.User) {
		if err := b.store.RecordOncallEngagement(ctx, m.Channel, m.ThreadTS, m.User); err != nil {
			b.log.Error("record engagement failed", "err", err)
		}
	}
	isAck := b.auth.CanAck(m.User) && b.isAckText(m.Text)
	via := "keyword"
	// A reply from a named responder is an ack regardless of what it says —
	// they are visibly on it, and asking them to also type "ack" is noise.
	if !isAck && b.cfg.AckAnyReplyUsers[m.User] && strings.TrimSpace(m.Text) != "" {
		isAck, via = true, "reply"
	}
	if !isAck && b.cfg.AckAnyOncallReply && b.auth.IsOncall(m.User) {
		isAck, via = true, "reply"
	}
	if !isAck {
		// Near-miss visibility: looked like an ack but the user isn't a responder.
		if b.isAckText(m.Text) && !b.auth.CanAck(m.User) {
			b.log.Info("ack ignored: not an authorized responder", "channel", m.Channel, "user", m.User)
		}
		return
	}
	b.recordAck(ctx, m.Channel, m.ThreadTS, m.User, via, false)
}

// tryArm evaluates the gratitude/top-level/dedup gates and arms an escalation
// if appropriate. armedAt sets the timer origin (message time for reconciled
// back-fills, now for live events). src is for logging ("event"|"reconcile").
// Returns true if a new escalation was created.
func (b *Bot) tryArm(ctx context.Context, channel, ts, threadTS, text, user string, armedAt time.Time, src string) bool {
	isReply := threadTS != "" && threadTS != ts
	if b.isNonRequestMention(text) {
		b.log.Info("mention skipped: gratitude/closing (leading word)", "channel", channel, "ts", ts)
		return false
	}
	if b.cfg.TriggerTopLevelOnly && isReply {
		b.log.Info("mention skipped: reply in top-level-only mode", "channel", channel, "ts", ts)
		return false
	}
	thread := threadTS
	if thread == "" {
		thread = ts
	}
	if open, err := b.store.OpenByThread(ctx, channel, thread); err != nil {
		b.log.Error("dedup check failed", "err", err)
		return false
	} else if open != nil {
		b.log.Debug("not re-arming: thread already has an open escalation", "channel", channel, "thread", thread)
		return false
	}
	created, err := b.store.Create(ctx, &store.Pending{
		Channel:       channel,
		TriggerTS:     ts,
		ThreadTS:      thread,
		Text:          text,
		RequestedBy:   user,
		NudgeDeadline: armedAt.Add(b.cfg.NudgeAfter),
		Deadline:      armedAt.Add(b.cfg.EscalateAfter),
	})
	if err != nil {
		b.log.Error("create pending failed", "err", err)
		return false
	}
	if created {
		lvl := b.log.Info
		msg := "escalation armed"
		if src == "reconcile" {
			// A reconcile-armed trigger means the live event was missed — surface loudly.
			lvl = b.log.Warn
			msg = "escalation armed via RECONCILE (live event was missed/undelivered)"
		}
		lvl(msg, "channel", channel, "ts", ts, "reply", isReply, "src", src)
	}
	return created
}

// recordAck cancels an escalation, tagging who acked (oncall vs responder), and
// posts a one-time in-thread confirmation.
func (b *Bot) recordAck(ctx context.Context, channel, ts, user, via string, matchTrigger bool) {
	kind := store.AckerResponder
	if b.auth.IsOncall(user) {
		kind = store.AckerOncall
	}
	acked, threadTS, err := b.store.Ack(ctx, channel, ts, user, via, kind, matchTrigger)
	if err != nil {
		b.log.Error("ack failed", "err", err)
		return
	}
	if !acked {
		// Already acked (dedupe: stay silent) — or already ESCALATED, where a
		// valid ack signal is still worth surfacing: record + post it once.
		late, lateThread, lerr := b.store.LateAck(ctx, channel, ts, user, via, matchTrigger)
		if lerr != nil {
			b.log.Error("late-ack record failed", "err", lerr)
			return
		}
		if late {
			b.log.Info("late ack (after escalation)", "via", via, "channel", channel, "by", user)
			if err := b.notify.PostThread(ctx, channel, lateThread, "☑️ Picked up by <@"+user+"> after escalation."); err != nil {
				b.log.Error("late-ack post failed", "channel", channel, "thread", lateThread, "err", err)
			}
		}
		return
	}
	b.log.Info("acknowledged", "via", via, "kind", kind, "channel", channel, "by", user)
	if err := b.notify.PostThread(ctx, channel, threadTS, "✅ Request acknowledged by <@"+user+">."); err != nil {
		b.log.Error("ack confirmation post failed", "channel", channel, "thread", threadTS, "err", err)
	}
}

// HandleReaction processes a `reaction_added` event as a possible ack.
func (b *Bot) HandleReaction(ctx context.Context, e *slackapi.ReactionEvent) {
	b.markSeen(e.Item.Channel)
	b.log.Debug("event: reaction_added", "channel", e.Item.Channel, "ts", e.Item.TS,
		"user", e.User, "emoji", e.Reaction)
	// Metric: any reaction from the on-call member is engagement.
	if b.auth.IsOncall(e.User) {
		if err := b.store.RecordOncallEngagement(ctx, e.Item.Channel, e.Item.TS, e.User); err != nil {
			b.log.Error("record engagement failed", "err", err)
		}
	}
	if !b.cfg.AckEmojis[e.Reaction] || !b.auth.CanAck(e.User) {
		return
	}
	b.recordAck(ctx, e.Item.Channel, e.Item.TS, e.User, "reaction", true)
}

// RunEscalations is the periodic timer: escalate anything past its deadline.
func (b *Bot) RunEscalations(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.nudgeDue(ctx)
			b.escalateDue(ctx)
		}
	}
}

// nudgeDue does stage 1: DM (or in-thread @-ping) the on-call human(s) after
// NudgeAfter, with a link to the waiting thread.
func (b *Bot) nudgeDue(ctx context.Context) {
	due, err := b.store.DueForNudge(ctx, b.now())
	if err != nil {
		b.log.Error("nudge scan failed", "err", err)
		return
	}
	for i := range due {
		p := due[i]
		claimed, err := b.store.MarkNudged(ctx, p.ID)
		if err != nil || !claimed {
			continue
		}
		humans := b.oncallHumans(ctx)
		if len(humans) == 0 {
			b.log.Warn("no human on-call to nudge", "channel", p.Channel, "thread", p.ThreadTS)
			continue
		}
		link, _ := b.notify.Permalink(ctx, p.Channel, p.TriggerTS)
		nudgeMin := int(b.cfg.NudgeAfter.Minutes())
		remain := int(b.cfg.EscalateAfter.Minutes()) - nudgeMin
		for _, uid := range humans {
			if b.cfg.NudgeMode == "thread_mention" {
				text := "<@" + uid + "> 👋 this request has been waiting ~" + itoa(nudgeMin) +
					"m without an acknowledgement — could you take a look or ack it? It escalates to the wider team in ~" + itoa(remain) + "m."
				if err := b.notify.PostThread(ctx, p.Channel, p.ThreadTS, text); err != nil {
					b.log.Error("nudge thread-mention failed", "user", uid, "err", err)
				}
				continue
			}
			text := "👋 Heads up — a request has been waiting for on-call for ~" + itoa(nudgeMin) +
				"m without an acknowledgement:\n" + link +
				"\nCould you take a look and ack it (reply `ack` or react ✅)? It escalates to the wider DevOps team in ~" + itoa(remain) + "m if not."
			if err := b.notify.PostDM(ctx, uid, text); err != nil {
				b.log.Error("nudge DM failed (is im:write granted?)", "user", uid, "err", err)
			}
		}
		b.log.Info("nudged on-call", "channel", p.Channel, "thread", p.ThreadTS, "humans", len(humans))
	}
}

// oncallHumans resolves the watched usergroup(s) to their human members —
// skipping bot accounts (e.g. incident.io) and any configured excludes.
func (b *Bot) oncallHumans(ctx context.Context) []string {
	seen := map[string]bool{}
	var out []string
	for _, gid := range b.cfg.WatchUsergroupIDs {
		members, err := b.notify.UsergroupMembers(ctx, gid)
		if err != nil {
			b.log.Error("usergroup members lookup failed", "usergroup", gid, "err", err)
			continue
		}
		for _, uid := range members {
			if seen[uid] || b.cfg.NudgeExclude[uid] {
				continue
			}
			isBot, err := b.notify.IsBot(ctx, uid)
			if err != nil {
				b.log.Warn("is-bot check failed; skipping", "user", uid, "err", err)
				continue
			}
			if isBot {
				continue
			}
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out
}

func (b *Bot) escalateDue(ctx context.Context) {
	due, err := b.store.DueForEscalation(ctx, b.now())
	if err != nil {
		b.log.Error("scan due failed", "err", err)
		return
	}
	for i := range due {
		p := due[i]
		// Claim the row first so a concurrent ack or double-tick can't double-post.
		claimed, err := b.store.MarkEscalated(ctx, p.ID)
		if err != nil || !claimed {
			continue
		}
		mention := b.cfg.EscalationMention(ctx)
		mins := int(b.cfg.EscalateAfter.Minutes())
		text := "⚠️ *Escalation* — " + itoa(mins) + " minutes passed with no acknowledgement from on-call. " +
			"Escalating to the wider DevOps team to take a look: " + mention + "."
		if err := b.notify.PostThread(ctx, p.Channel, p.ThreadTS, text); err != nil {
			b.log.Error("escalation post failed; will retry", "channel", p.Channel, "thread", p.ThreadTS, "err", err)
			if rerr := b.store.ResetToOpen(ctx, p.ID); rerr != nil {
				b.log.Error("reset-to-open failed", "id", p.ID, "err", rerr)
			}
			continue
		}
		b.log.Info("escalated", "channel", p.Channel, "thread", p.ThreadTS)
	}
}

// isTrigger reports whether a message mentions a watched usergroup or keyword.
func (b *Bot) isTrigger(text string) bool {
	for _, id := range b.cfg.WatchUsergroupIDs {
		// Slack renders a usergroup mention as <!subteam^S123|@handle>.
		if strings.Contains(text, "<!subteam^"+id) {
			return true
		}
	}
	low := strings.ToLower(text)
	for _, kw := range b.cfg.WatchKeywords {
		if strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// isNonRequestMention reports whether a message that mentions the on-call group
// is actually gratitude/closing/informational rather than a help request —
// judged by its leading word (e.g. "thanks @devops-oncall", "fyi @…", "nvm").
// A bare mention with no other text is treated as a real ping (not ignored).
func (b *Bot) isNonRequestMention(text string) bool {
	s := strings.ToLower(stripMentions(text))
	s = strings.TrimLeft(s, " \t\n,.!:;)-—…\"'")
	if s == "" {
		return false // bare "@devops-oncall" → a genuine ping
	}
	first := s
	if i := strings.IndexAny(s, " \t\n,.!:;?"); i > 0 {
		first = s[:i]
	}
	// strip trailing apostrophe-suffixes like "thanks'" already handled by split
	for _, w := range b.cfg.TriggerIgnoreLeading {
		if first == strings.ToLower(w) {
			return true
		}
	}
	return false
}

// isAckText matches acknowledgement phrases. Single-word keywords ("ack",
// "looking") match on whole-word boundaries so "slack"/"back"/"feedback" don't
// false-trigger; multi-word/punctuated keywords ("on it", "ack'd") match as a
// substring.
func (b *Bot) isAckText(text string) bool {
	low := strings.ToLower(stripMentions(text))
	if strings.TrimSpace(low) == "" {
		return false
	}
	tokens := tokenize(low)
	for _, kw := range b.cfg.AckKeywords {
		k := strings.ToLower(strings.TrimSpace(kw))
		if k == "" {
			continue
		}
		if isSingleWord(k) {
			if tokens[k] {
				return true
			}
		} else if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// tokenize splits text into a set of maximal letter/digit runs.
func tokenize(s string) map[string]bool {
	set := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			set[cur.String()] = true
			cur.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return set
}

func isSingleWord(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// stripMentions removes <@U…> / <!subteam^…> tokens so "looking @someone"
// still matches on the keyword.
func stripMentions(text string) string {
	var b strings.Builder
	depth := 0
	for _, r := range text {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
