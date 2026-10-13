package bot

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"escalator/internal/slackapi"
	"escalator/internal/store"
)

// --- test doubles ---

// fakePoster is a fake Notifier: records thread posts + DMs, exposes a
// configurable on-call member set.
type fakePoster struct {
	mu      sync.Mutex
	posts   []string // thread posts: "channel|thread|text"
	dms     []string // DMs: "user|text"
	members []string // usergroup members returned
	bots    map[string]bool
	history map[string][]slackapi.HistMessage // channel -> messages
	replies map[string][]slackapi.HistMessage // channel|threadTS -> messages
}

func (f *fakePoster) PostThread(_ context.Context, channel, thread, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, channel+"|"+thread+"|"+text)
	return nil
}
func (f *fakePoster) PostDM(_ context.Context, user, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dms = append(f.dms, user+"|"+text)
	return nil
}
func (f *fakePoster) Permalink(_ context.Context, channel, ts string) (string, error) {
	return "https://slack/" + channel + "/" + ts, nil
}
func (f *fakePoster) UsergroupMembers(_ context.Context, _ string) ([]string, error) {
	return f.members, nil
}
func (f *fakePoster) IsBot(_ context.Context, u string) (bool, error) { return f.bots[u], nil }
func (f *fakePoster) ChannelHistory(_ context.Context, channel, _ string) ([]slackapi.HistMessage, error) {
	return f.history[channel], nil
}
func (f *fakePoster) ThreadReplies(_ context.Context, channel, threadTS string) ([]slackapi.HistMessage, error) {
	return f.replies[channel+"|"+threadTS], nil
}

func (f *fakePoster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// escalations counts only escalation posts (not ack confirmations).
func (f *fakePoster) escalations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.posts {
		if strings.Contains(p, "Escalation") {
			n++
		}
	}
	return n
}

// ackConfirms counts ack-confirmation posts.
func (f *fakePoster) ackConfirms() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.posts {
		if strings.Contains(p, "acknowledged by") {
			n++
		}
	}
	return n
}

// pickedUps counts late-ack "picked up" posts.
func (f *fakePoster) pickedUps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.posts {
		if strings.Contains(p, "Picked up by") {
			n++
		}
	}
	return n
}

func (f *fakePoster) dmCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dms)
}

// failingPoster fails the first N thread posts, then succeeds.
type failingPoster struct {
	fakePoster
	failsLeft int
	posts     int
}

func (f *failingPoster) PostThread(context.Context, string, string, string) error {
	if f.failsLeft > 0 {
		f.failsLeft--
		return errFakePost
	}
	f.posts++
	return nil
}

var errFakePost = &postErr{}

type postErr struct{}

func (*postErr) Error() string { return "fake post failure" }

// fakeAuth stubs the Authorizer: canAck = keyword/emoji ack rights,
// oncall = members whose any-reply acks.
type fakeAuth struct {
	canAck map[string]bool
	oncall map[string]bool
}

func (f fakeAuth) CanAck(u string) bool {
	return f.canAck[u] || f.oncall[u]
}
func (f fakeAuth) IsOncall(u string) bool { return f.oncall[u] }

// ack helper: build an Authorizer where the given users can keyword/emoji ack.
func canAck(users ...string) fakeAuth {
	m := map[string]bool{}
	for _, u := range users {
		m[u] = true
	}
	return fakeAuth{canAck: m, oncall: map[string]bool{}}
}

func newTestBot(t *testing.T, post Notifier, auth Authorizer) (*Bot, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	b := New(Config{
		WatchUsergroupIDs:    []string{"S_ONCALL"},
		TriggerIgnoreLeading: []string{"thanks", "thank", "thankyou", "thx", "ty", "cheers", "nvm", "fyi", "cc", "resolved"},
		AckKeywords:          []string{"ack", "looking", "on it"},
		AckEmojis:            map[string]bool{"white_check_mark": true, "eyes": true},
		NudgeAfter:           15 * time.Minute,
		EscalateAfter:        30 * time.Minute,
		NudgeMode:            "dm",
		AckAnyOncallReply:    false, // production default: only explicit acks cancel
		EscalationMention:    func(context.Context) string { return "<!subteam^S_DEVOPS>" },
	}, st, post, auth, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return b, st
}

// controllable clock
func (b *Bot) setNow(f func() time.Time) { b.now = f }

const triggerText = "hey <!subteam^S_ONCALL|@devops-oncall> can someone check prod?"

func TestTriggerArmsAndEscalates(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck())
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})

	// Not yet due.
	b.escalateDue(context.Background())
	if post.count() != 0 {
		t.Fatalf("escalated too early: %v", post.posts)
	}

	// Advance past the deadline.
	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.count() != 1 {
		t.Fatalf("expected 1 escalation, got %d", post.count())
	}
	// Idempotent: a second tick must not double-post.
	b.escalateDue(context.Background())
	if post.count() != 1 {
		t.Fatalf("double-escalated: %d", post.count())
	}
}

func TestAckReplyCancelsEscalation(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck("U_ONCALL"))
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	// Authorized responder replies "looking" in-thread.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_ONCALL", Text: "looking", Channel: "C1", TS: "100.2", ThreadTS: "100.1",
	})

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.escalations() != 0 {
		t.Fatalf("escalated despite ack: %v", post.posts)
	}
	if post.ackConfirms() != 1 {
		t.Fatalf("expected 1 ack confirmation, got %d", post.ackConfirms())
	}
}

func TestAckByUnauthorizedIgnored(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck("U_ONCALL"))
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	// Random user (not a responder) says "ack" — must NOT cancel.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_RANDOM", Text: "ack", Channel: "C1", TS: "100.3", ThreadTS: "100.1",
	})

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.count() != 1 {
		t.Fatalf("expected escalation (unauthorized ack ignored), got %d", post.count())
	}
}

func TestAckAnyReplyUserCancelsWithoutKeyword(t *testing.T) {
	post := &fakePoster{}
	// Deliberately NOT a configured responder: the named list stands on its own,
	// so a rota change that drops them from the usergroup can't silently break it.
	b, _ := newTestBot(t, post, canAck("U_ONCALL"))
	b.cfg.AckAnyReplyUsers = map[string]bool{"U_ALICE": true}
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	// No ack keyword anywhere in it — the reply itself is the acknowledgement.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_ALICE", Text: "which cluster is this on?", Channel: "C1", TS: "100.3", ThreadTS: "100.1",
	})

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.escalations() != 0 {
		t.Fatalf("escalated despite named-user reply: %v", post.posts)
	}
	if post.ackConfirms() != 1 {
		t.Fatalf("expected 1 ack confirmation, got %d", post.ackConfirms())
	}
}

func TestAckAnyReplyUserIgnoresEmptyReply(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck("U_ONCALL"))
	b.cfg.AckAnyReplyUsers = map[string]bool{"U_ALICE": true}
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	// A bare file/image share carries no text; treating it as an ack would let
	// an accidental paste silence a real page.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_ALICE", Text: "   ", Channel: "C1", TS: "100.3", ThreadTS: "100.1",
	})

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.escalations() != 1 {
		t.Fatalf("expected escalation (empty reply is not an ack): %v", post.posts)
	}
	if post.ackConfirms() != 0 {
		t.Fatalf("empty reply must not ack, got %d confirmations", post.ackConfirms())
	}
}

func TestAckByReactionCancels(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck("U_ONCALL"))
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	e := &slackapi.ReactionEvent{User: "U_ONCALL", Reaction: "eyes"}
	e.Item.Channel = "C1"
	e.Item.TS = "100.1"
	b.HandleReaction(context.Background(), e)

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.escalations() != 0 {
		t.Fatalf("reaction ack did not cancel: %v", post.posts)
	}
	if post.ackConfirms() != 1 {
		t.Fatalf("expected 1 ack confirmation from reaction, got %d", post.ackConfirms())
	}
}

func TestWrongEmojiDoesNotAck(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck("U_ONCALL"))
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	e := &slackapi.ReactionEvent{User: "U_ONCALL", Reaction: "tada"} // not an ack emoji
	e.Item.Channel = "C1"
	e.Item.TS = "100.1"
	b.HandleReaction(context.Background(), e)

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.count() != 1 {
		t.Fatalf("non-ack emoji wrongly cancelled")
	}
}

func TestBotMessageIgnored(t *testing.T) {
	post := &fakePoster{}
	b, st := newTestBot(t, post, canAck())
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		BotID: "B1", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	due, _ := st.DueForEscalation(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("bot message armed an escalation")
	}
}

func TestNonTriggerIgnored(t *testing.T) {
	post := &fakePoster{}
	b, st := newTestBot(t, post, canAck())
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "just chatting, no mention", Channel: "C1", TS: "100.1",
	})
	due, _ := st.DueForEscalation(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("non-trigger armed an escalation")
	}
}

func TestFailedPostRetries(t *testing.T) {
	post := &failingPoster{failsLeft: 1} // first attempt fails, second succeeds
	b, _ := newTestBot(t, post, canAck())
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	now = now.Add(31 * time.Minute)

	b.escalateDue(context.Background()) // fails -> reset to open
	if post.posts != 0 {
		t.Fatalf("unexpected successful post")
	}
	b.escalateDue(context.Background()) // retry -> succeeds
	if post.posts != 1 {
		t.Fatalf("expected retry to post once, got %d", post.posts)
	}
	b.escalateDue(context.Background()) // now escalated, no more
	if post.posts != 1 {
		t.Fatalf("posted again after success: %d", post.posts)
	}
}

func TestAnyOncallReplyAcks(t *testing.T) {
	post := &fakePoster{}
	// U_ONCALL is a member of the watched on-call group; U_DEV is the requester.
	b, _ := newTestBot(t, post, fakeAuth{canAck: map[string]bool{}, oncall: map[string]bool{"U_ONCALL": true}})
	b.cfg.AckAnyOncallReply = true // opt-in: any on-call reply cancels
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	// On-call member replies with NO ack keyword — just a question. Counts as
	// engagement, so escalation must be cancelled.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_ONCALL", Text: "what's the impact here?", Channel: "C1", TS: "100.2", ThreadTS: "100.1",
	})

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.escalations() != 0 {
		t.Fatalf("escalated despite on-call engagement: %v", post.posts)
	}
}

func TestNonOncallNonKeywordReplyDoesNotAck(t *testing.T) {
	post := &fakePoster{}
	// A bystander dev (not on-call, not a responder) chatting in-thread.
	b, _ := newTestBot(t, post, fakeAuth{canAck: map[string]bool{}, oncall: map[string]bool{"U_ONCALL": true}})
	now := time.Now()
	b.setNow(func() time.Time { return now })

	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "100.1",
	})
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_BYSTANDER", Text: "yeah this happened to me too", Channel: "C1", TS: "100.2", ThreadTS: "100.1",
	})

	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.count() != 1 {
		t.Fatalf("bystander chatter wrongly cancelled escalation")
	}
}

func TestMetricsCapture(t *testing.T) {
	post := &fakePoster{}
	// U_ONCALL is on-call; U_RESP is a wider responder; U_DEV is a requester.
	auth := fakeAuth{canAck: map[string]bool{"U_RESP": true}, oncall: map[string]bool{"U_ONCALL": true}}
	b, st := newTestBot(t, post, auth)
	now := time.Now()
	b.setNow(func() time.Time { return now })
	ctx := context.Background()

	// Incident A: on-call acks with a keyword.
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "1.0"})
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_ONCALL", Text: "on it", Channel: "C1", TS: "1.1", ThreadTS: "1.0"})

	// Incident B: a *different* responder (not the on-call) steps in.
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "2.0"})
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_RESP", Text: "looking", Channel: "C1", TS: "2.1", ThreadTS: "2.0"})

	// Incident C: on-call chats (engagement) but never acks → escalates.
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "3.0"})
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_ONCALL", Text: "hmm what's the error?", Channel: "C1", TS: "3.1", ThreadTS: "3.0"})
	now = now.Add(31 * time.Minute)
	b.escalateDue(ctx)

	m, err := st.Stats(ctx, 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if m.TotalTags != 3 {
		t.Errorf("total_tags=%d want 3", m.TotalTags)
	}
	if m.Acknowledged != 2 {
		t.Errorf("acknowledged=%d want 2", m.Acknowledged)
	}
	if m.Escalated != 1 {
		t.Errorf("escalated=%d want 1", m.Escalated)
	}
	if m.AckedByOncall != 1 {
		t.Errorf("acked_by_oncall=%d want 1", m.AckedByOncall)
	}
	if m.AckedByOther != 1 {
		t.Errorf("acked_by_other=%d want 1", m.AckedByOther)
	}
	// On-call engaged in incidents A and C (replied), not B.
	if m.OncallEngaged != 2 {
		t.Errorf("oncall_engaged=%d want 2", m.OncallEngaged)
	}
}

func armed(t *testing.T, st *store.Store) int {
	t.Helper()
	due, err := st.DueForEscalation(context.Background(), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	return len(due)
}

func TestTwoTierNudgeThenEscalate(t *testing.T) {
	post := &fakePoster{members: []string{"U_HUMAN", "U_BOT"}, bots: map[string]bool{"U_BOT": true}}
	b, _ := newTestBot(t, post, canAck())
	now := time.Now()
	b.setNow(func() time.Time { return now })
	ctx := context.Background()

	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "1.0"})

	// Before 15m: nothing.
	b.nudgeDue(ctx)
	b.escalateDue(ctx)
	if post.dmCount() != 0 || post.count() != 0 {
		t.Fatalf("fired too early: dm=%d post=%d", post.dmCount(), post.count())
	}

	// At 16m: stage-1 DM to the human only (not the incident.io bot), no escalation.
	now = now.Add(16 * time.Minute)
	b.nudgeDue(ctx)
	b.escalateDue(ctx)
	if post.dmCount() != 1 {
		t.Fatalf("expected 1 nudge DM, got %d", post.dmCount())
	}
	if post.dms[0][:len("U_HUMAN|")] != "U_HUMAN|" {
		t.Fatalf("nudge went to wrong user: %s", post.dms[0])
	}
	if post.count() != 0 {
		t.Fatalf("escalated at 16m (too early): %d", post.count())
	}
	// Nudge is idempotent.
	b.nudgeDue(ctx)
	if post.dmCount() != 1 {
		t.Fatalf("nudged twice: %d", post.dmCount())
	}

	// At 31m: escalation posts to the thread.
	now = now.Add(15 * time.Minute)
	b.escalateDue(ctx)
	if post.escalations() != 1 {
		t.Fatalf("expected 1 escalation, got %d", post.escalations())
	}
}

func TestAckBeforeNudgeCancelsBoth(t *testing.T) {
	post := &fakePoster{members: []string{"U_HUMAN"}, bots: map[string]bool{}}
	b, _ := newTestBot(t, post, canAck("U_RESP"))
	now := time.Now()
	b.setNow(func() time.Time { return now })
	ctx := context.Background()

	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "1.0"})
	// Responder acks within 5m.
	now = now.Add(5 * time.Minute)
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_RESP", Text: "on it", Channel: "C1", TS: "1.1", ThreadTS: "1.0"})

	now = now.Add(30 * time.Minute)
	b.nudgeDue(ctx)
	b.escalateDue(ctx)
	if post.dmCount() != 0 {
		t.Fatalf("nudged despite ack: %d", post.dmCount())
	}
	if post.escalations() != 0 {
		t.Fatalf("escalated despite ack: %d", post.escalations())
	}
}

func TestAckConfirmationPostedOnce(t *testing.T) {
	post := &fakePoster{}
	b, _ := newTestBot(t, post, canAck("U_RESP"))
	ctx := context.Background()
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "1.0"})
	// Two acks in a row (same thread) → only the first posts a confirmation.
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_RESP", Text: "ack", Channel: "C1", TS: "1.1", ThreadTS: "1.0"})
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_RESP", Text: "still on it", Channel: "C1", TS: "1.2", ThreadTS: "1.0"})
	if post.ackConfirms() != 1 {
		t.Fatalf("expected exactly 1 ack confirmation, got %d", post.ackConfirms())
	}
	if !strings.Contains(post.posts[0], "<@U_RESP>") {
		t.Fatalf("confirmation should tag the acker: %q", post.posts[0])
	}
}

func TestReconcileArmsMissedTrigger(t *testing.T) {
	// Simulates the real incident: the message event was never delivered, so no
	// escalation armed. Reconcile must back-fill it from channel history.
	msgTS := "1784711022.003239"
	post := &fakePoster{
		history: map[string][]slackapi.HistMessage{
			"C1": {{Type: "message", User: "U_DEV", Text: triggerText, TS: msgTS}},
		},
	}
	b, st := newTestBot(t, post, canAck())
	// Channel is "seen" (bot got some event there before) but this trigger was missed.
	b.markSeen("C1")

	b.reconcile(context.Background())

	if n := armed(t, st); n != 1 {
		t.Fatalf("reconcile did not back-fill the missed trigger: armed=%d", n)
	}
	// Idempotent: a second pass must not double-arm.
	b.reconcile(context.Background())
	if n := armed(t, st); n != 1 {
		t.Fatalf("reconcile double-armed: %d", n)
	}
}

func TestReconcileCatchesMissedReactionAck(t *testing.T) {
	// Escalation is armed but the ack reaction event was dropped. Reconcile must
	// find the reaction in the thread and ack it.
	trig := "200.0"
	post := &fakePoster{
		replies: map[string][]slackapi.HistMessage{
			"C1|" + trig: {{
				TS: trig, User: "U_DEV", Text: triggerText,
				Reactions: []struct {
					Name  string   `json:"name"`
					Users []string `json:"users"`
				}{{Name: "eyes", Users: []string{"U_ONCALL"}}},
			}},
		},
	}
	b, st := newTestBot(t, post, canAck("U_ONCALL"))
	now := time.Now()
	b.setNow(func() time.Time { return now })
	b.markSeen("C1")
	// Arm directly (as if the message event had been received).
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: trig})
	if armed(t, st) != 1 {
		t.Fatal("setup: expected 1 armed")
	}

	b.reconcile(context.Background()) // should detect the missed reaction ack

	if post.ackConfirms() != 1 {
		t.Fatalf("reconcile did not ack via missed reaction (confirms=%d)", post.ackConfirms())
	}
	now = now.Add(31 * time.Minute)
	b.escalateDue(context.Background())
	if post.escalations() != 0 {
		t.Fatalf("escalated despite reconciled ack: %d", post.escalations())
	}
}

func TestLateAckPostsPickedUpOnce(t *testing.T) {
	// Real case (2026-07-28, C_EXAMPLE): a responder 👀'd the trigger AFTER the 30m
	// escalation had fired — silently ignored. Now: record it and post a
	// one-time "picked up" note, without flipping the escalated status.
	post := &fakePoster{}
	b, st := newTestBot(t, post, canAck("U_ALICE", "U_BOB"))
	now := time.Now()
	b.setNow(func() time.Time { return now })
	ctx := context.Background()

	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_DEV", Text: triggerText, Channel: "C1", TS: "300.0"})
	now = now.Add(31 * time.Minute)
	b.escalateDue(ctx)
	if post.escalations() != 1 {
		t.Fatal("setup: expected escalation")
	}

	// Late reaction ack → one "picked up" post, no ack confirmation.
	re := &slackapi.ReactionEvent{User: "U_ALICE", Reaction: "eyes"}
	re.Item.Channel = "C1"
	re.Item.TS = "300.0"
	b.HandleReaction(ctx, re)
	if got := post.pickedUps(); got != 1 {
		t.Fatalf("expected 1 picked-up post, got %d", got)
	}
	if post.ackConfirms() != 0 {
		t.Fatal("late ack must not post a normal ack confirmation")
	}

	// A second late signal (keyword reply this time) must not re-post.
	b.HandleMessage(ctx, &slackapi.MessageEvent{User: "U_BOB", Text: "on it", Channel: "C1", TS: "300.5", ThreadTS: "300.0"})
	if got := post.pickedUps(); got != 1 {
		t.Fatalf("picked-up posted twice: %d", got)
	}

	// Status must remain escalated in stats.
	m, err := st.Stats(ctx, 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if m.Escalated != 1 || m.LateAcked != 1 || m.Acknowledged != 0 {
		t.Fatalf("stats wrong: escalated=%d late_acked=%d acked=%d", m.Escalated, m.LateAcked, m.Acknowledged)
	}
}

func TestRootReactionAcks(t *testing.T) {
	// Real case (2026-08-03, C_EXAMPLE): the trigger was a reply in a thread
	// (bare @devops-oncall under a workflow-bot request); a responder 👀'd the thread
	// ROOT — the natural place to react — and it didn't count. Live reaction
	// events now ack against trigger_ts OR thread_ts.
	post := &fakePoster{}
	b, st := newTestBot(t, post, canAck("U_ALICE"))
	ctx := context.Background()

	// Trigger is a REPLY: thread root 50.0, mention reply 60.0.
	b.HandleMessage(ctx, &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "60.0", ThreadTS: "50.0",
	})
	if armed(t, st) != 1 {
		t.Fatal("setup: expected 1 armed")
	}

	// Reaction on an unrelated message: no ack.
	re := &slackapi.ReactionEvent{User: "U_ALICE", Reaction: "eyes"}
	re.Item.Channel = "C1"
	re.Item.TS = "99.9"
	b.HandleReaction(ctx, re)
	if post.ackConfirms() != 0 {
		t.Fatal("unrelated-message reaction must not ack")
	}

	// Reaction on the thread ROOT: acks.
	re2 := &slackapi.ReactionEvent{User: "U_ALICE", Reaction: "eyes"}
	re2.Item.Channel = "C1"
	re2.Item.TS = "50.0"
	b.HandleReaction(ctx, re2)
	if post.ackConfirms() != 1 {
		t.Fatalf("root reaction did not ack (confirms=%d)", post.ackConfirms())
	}
	if armed(t, st) != 0 {
		t.Fatal("escalation should be cancelled")
	}
}

func TestReconcileIgnoresPreTriggerKeywordReply(t *testing.T) {
	// Regression (2026-07-28, C_EXAMPLE): a thread already contained an old
	// responder message with an ack keyword ("the creator is checking..."). A NEW
	// on-call mention later in the same thread armed a fresh escalation, and
	// reconcile acked it from the OLD message. Pre-trigger replies must not ack.
	thread := "100.0"
	oldReply := "150.0" // responder banter BEFORE the new trigger
	trig := "200.0"     // the new on-call mention (a reply in the thread)
	post := &fakePoster{
		replies: map[string][]slackapi.HistMessage{
			"C1|" + thread: {
				{TS: thread, User: "U_DEV", Text: "original request"},
				{TS: oldReply, User: "U_RESP", Text: "the creator is checking...", ThreadTS: thread},
				{TS: trig, User: "U_DEV", Text: triggerText, ThreadTS: thread},
			},
		},
	}
	b, st := newTestBot(t, post, canAck("U_RESP"))
	b.markSeen("C1")
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: trig, ThreadTS: thread,
	})
	if armed(t, st) != 1 {
		t.Fatal("setup: expected 1 armed")
	}

	b.reconcile(context.Background())

	if post.ackConfirms() != 0 {
		t.Fatalf("reconcile acked from a pre-trigger reply (confirms=%d)", post.ackConfirms())
	}
	if armed(t, st) != 1 {
		t.Fatal("escalation should still be open")
	}

	// A keyword reply AFTER the trigger must still ack.
	post.replies["C1|"+thread] = append(post.replies["C1|"+thread],
		slackapi.HistMessage{TS: "250.0", User: "U_RESP", Text: "on it", ThreadTS: thread})
	b.reconcile(context.Background())
	if post.ackConfirms() != 1 {
		t.Fatalf("reconcile missed a post-trigger keyword ack (confirms=%d)", post.ackConfirms())
	}
}

func TestGratitudeMentionIgnored(t *testing.T) {
	b, st := newTestBot(t, &fakePoster{}, canAck())
	// A reply thanking the group must NOT arm an escalation.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "thank you <!subteam^S_ONCALL|@devops-oncall> for the fix!",
		Channel: "C1", TS: "5.1", ThreadTS: "5.0",
	})
	if n := armed(t, st); n != 0 {
		t.Fatalf("gratitude mention armed %d escalations", n)
	}
}

func TestTrailingThanksStillArms(t *testing.T) {
	b, st := newTestBot(t, &fakePoster{}, canAck())
	// Leading word is a real request; trailing "thanks" must not suppress it.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "<!subteam^S_ONCALL> prod is down, can you look? thanks",
		Channel: "C1", TS: "6.1",
	})
	if n := armed(t, st); n != 1 {
		t.Fatalf("real request with trailing thanks armed %d (want 1)", n)
	}
}

func TestFyiMentionIgnored(t *testing.T) {
	b, st := newTestBot(t, &fakePoster{}, canAck())
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "fyi <!subteam^S_ONCALL> we deployed", Channel: "C1", TS: "7.1",
	})
	if n := armed(t, st); n != 0 {
		t.Fatalf("fyi mention armed %d escalations", n)
	}
}

func TestPerThreadDedup(t *testing.T) {
	b, st := newTestBot(t, &fakePoster{}, canAck())
	// First mention starts the thread and arms.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "<!subteam^S_ONCALL> help", Channel: "C1", TS: "8.0",
	})
	// A second mention in the SAME thread must not create a second escalation.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV2", Text: "<!subteam^S_ONCALL> still need help", Channel: "C1", TS: "8.1", ThreadTS: "8.0",
	})
	if n := armed(t, st); n != 1 {
		t.Fatalf("expected 1 escalation for the thread, got %d", n)
	}
}

func TestTopLevelOnlyMode(t *testing.T) {
	b, st := newTestBot(t, &fakePoster{}, canAck())
	b.cfg.TriggerTopLevelOnly = true
	// A mention that is a reply in an existing thread must be ignored.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "<!subteam^S_ONCALL> can you help here", Channel: "C1", TS: "9.1", ThreadTS: "9.0",
	})
	if n := armed(t, st); n != 0 {
		t.Fatalf("top-level-only armed on a reply: %d", n)
	}
	// A top-level mention still arms.
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: "<!subteam^S_ONCALL> prod down", Channel: "C1", TS: "10.0",
	})
	if n := armed(t, st); n != 1 {
		t.Fatalf("top-level-only failed to arm a root mention: %d", n)
	}
}

func TestAckTextMatching(t *testing.T) {
	b, _ := newTestBot(t, &fakePoster{}, canAck())
	cases := map[string]bool{
		"ack":                  true,
		"ACK":                  true,
		"looking":              true,
		"on it":                true,
		"I'm looking now":      true,
		"<@U123> looking":      true, // mention stripped, keyword remains
		"no idea what this is": false,
		"":                     false,
		"back to work":         false, // "ack" substring guard: "back" contains no standalone... (documents behavior)
	}
	for text, want := range cases {
		if got := b.isAckText(text); got != want {
			t.Errorf("isAckText(%q)=%v want %v", text, got, want)
		}
	}
}

func TestFileShareMentionArms(t *testing.T) {
	// Real miss (2026-10-13): on-call tagged in a thread reply with a screenshot
	// attached arrives as subtype "file_share" and was dropped.
	b, st := newTestBot(t, &fakePoster{}, canAck())
	b.HandleMessage(context.Background(), &slackapi.MessageEvent{
		User: "U_DEV", Text: triggerText, Channel: "C1", TS: "200.2", ThreadTS: "200.0", Subtype: "file_share",
	})
	if n := armed(t, st); n != 1 {
		t.Fatalf("file_share mention did not arm: %d", n)
	}
}

func TestNoiseSubtypeStillSkipped(t *testing.T) {
	b, st := newTestBot(t, &fakePoster{}, canAck())
	for _, sub := range []string{"message_changed", "message_deleted", "channel_join", "bot_message"} {
		b.HandleMessage(context.Background(), &slackapi.MessageEvent{
			User: "U_DEV", Text: triggerText, Channel: "C1", TS: "300.1", Subtype: sub,
		})
	}
	if n := armed(t, st); n != 0 {
		t.Fatalf("noise subtype armed: %d", n)
	}
}

func TestReconcileArmsMissedThreadReplyTrigger(t *testing.T) {
	// The mention is a reply, so conversations.history only shows the root;
	// reconcile must read the thread to find it.
	now := time.Now()
	root := strconv.FormatInt(now.Add(-2*time.Minute).Unix(), 10) + ".000100"
	reply := strconv.FormatInt(now.Add(-1*time.Minute).Unix(), 10) + ".000200"
	post := &fakePoster{
		history: map[string][]slackapi.HistMessage{
			"C1": {{Type: "message", BotID: "B_FORM", Text: "Unable to access stage", TS: root, ReplyCount: 2, LatestReply: reply}},
		},
		replies: map[string][]slackapi.HistMessage{
			"C1|" + root: {
				{Type: "message", BotID: "B_FORM", Text: "Unable to access stage", TS: root},
				{Type: "message", User: "U_DEV", Text: triggerText, TS: reply, ThreadTS: root, Subtype: "file_share"},
			},
		},
	}
	b, st := newTestBot(t, post, canAck())
	b.setNow(func() time.Time { return now })
	b.markSeen("C1")

	b.reconcile(context.Background())
	if n := armed(t, st); n != 1 {
		t.Fatalf("reconcile missed the thread-reply trigger: armed=%d", n)
	}
	b.reconcile(context.Background())
	if n := armed(t, st); n != 1 {
		t.Fatalf("reconcile double-armed: %d", n)
	}
}
