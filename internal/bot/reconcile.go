package bot

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// RunReconcile is the safety net: it periodically scans the channels the bot
// has seen for (a) on-call mentions that never armed an escalation — because
// the live `message` event was dropped/undelivered — and back-fills them, and
// (b) open escalations whose ack (reply/reaction) event was missed or raced,
// acking them. This makes the bot self-heal against Slack's at-least-once (and
// occasionally at-most-once) event delivery.
func (b *Bot) RunReconcile(ctx context.Context) {
	if b.cfg.ReconcileInterval <= 0 {
		b.log.Info("reconcile disabled")
		return
	}
	b.log.Info("reconcile started", "interval", b.cfg.ReconcileInterval.String(),
		"window", b.cfg.ReconcileWindow.String())
	t := time.NewTicker(b.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reconcile(ctx)
		}
	}
}

func (b *Bot) reconcile(ctx context.Context) {
	window := b.cfg.ReconcileWindow
	if window <= 0 {
		window = b.cfg.EscalateAfter + 5*time.Minute
	}
	oldest := strconv.FormatInt(b.now().Add(-window).Unix(), 10)

	for _, ch := range b.seenChannels() {
		msgs, err := b.notify.ChannelHistory(ctx, ch, oldest)
		if err != nil {
			b.log.Error("reconcile: history fetch failed", "channel", ch, "err", err)
			continue
		}
		// (a) Back-fill missed triggers.
		for i := range msgs {
			m := msgs[i]
			if m.BotID != "" || m.User == "" {
				continue
			}
			if m.Subtype != "" && m.Subtype != "thread_broadcast" {
				continue
			}
			if !b.isTrigger(m.Text) {
				continue
			}
			exists, err := b.store.ExistsByTrigger(ctx, ch, m.TS)
			if err != nil {
				b.log.Error("reconcile: exists check failed", "err", err)
				continue
			}
			if exists {
				continue
			}
			b.tryArm(ctx, ch, m.TS, m.ThreadTS, m.Text, m.User, tsToTime(m.TS), "reconcile")
		}
		// (b) Catch missed acks on still-open escalations.
		open, err := b.store.ListOpen(ctx, ch)
		if err != nil {
			b.log.Error("reconcile: list open failed", "channel", ch, "err", err)
			continue
		}
		for i := range open {
			b.reconcileAck(ctx, open[i].Channel, open[i].TriggerTS, open[i].ThreadTS)
		}
	}
}

// reconcileAck re-reads a thread and acks the escalation if a valid ack signal
// (ack reaction on the trigger message, or an ack reply) is present but was
// never processed live.
func (b *Bot) reconcileAck(ctx context.Context, channel, triggerTS, threadTS string) {
	msgs, err := b.notify.ThreadReplies(ctx, channel, threadTS)
	if err != nil {
		b.log.Error("reconcile: replies fetch failed", "channel", channel, "err", err)
		return
	}
	for i := range msgs {
		m := msgs[i]
		// Ack reaction on the triggering message.
		if m.TS == triggerTS {
			for _, r := range m.Reactions {
				if !b.cfg.AckEmojis[r.Name] {
					continue
				}
				for _, u := range r.Users {
					if b.auth.CanAck(u) {
						b.log.Warn("reconcile: acking via missed reaction", "channel", channel, "by", u, "emoji", r.Name)
						b.recordAck(ctx, channel, triggerTS, u, "reaction", true)
						return
					}
				}
			}
		}
		// Ack reply in the thread (skip the root). Only messages posted AFTER the
		// trigger count — older thread chatter that happens to contain an ack
		// keyword must not ack a newer escalation in the same thread.
		if m.TS != threadTS && m.User != "" && tsAfter(m.TS, triggerTS) {
			ackByKeyword := b.auth.CanAck(m.User) && b.isAckText(m.Text)
			ackByNamed := b.cfg.AckAnyReplyUsers[m.User] && strings.TrimSpace(m.Text) != ""
			ackByOncall := b.cfg.AckAnyOncallReply && b.auth.IsOncall(m.User)
			if ackByKeyword || ackByNamed || ackByOncall {
				via := "keyword"
				if !ackByKeyword {
					via = "reply"
				}
				b.log.Warn("reconcile: acking via missed reply", "channel", channel, "by", m.User, "via", via)
				b.recordAck(ctx, channel, threadTS, m.User, via, false)
				return
			}
		}
	}
}

// tsAfter reports whether Slack timestamp a is strictly later than b.
// Unparseable timestamps compare as NOT after (safe: skip, don't ack).
func tsAfter(a, b string) bool {
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA != nil || errB != nil {
		return false
	}
	return fa > fb
}

// tsToTime parses a Slack ts ("1784711022.003239") into a time.Time.
func tsToTime(ts string) time.Time {
	sec := ts
	if i := indexByte(ts, '.'); i >= 0 {
		sec = ts[:i]
	}
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.Unix(n, 0)
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
