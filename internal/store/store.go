package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// Escalation statuses.
const (
	StatusOpen      = "open"
	StatusAcked     = "acked"
	StatusEscalated = "escalated"
)

// Acker kinds — was the person who acknowledged the actual on-call, or another
// responder who stepped in?
const (
	AckerOncall    = "oncall"
	AckerResponder = "responder"
)

// Pending is a tracked help request awaiting acknowledgement.
type Pending struct {
	ID            int64
	Channel       string
	TriggerTS     string
	ThreadTS      string
	Text          string
	RequestedBy   string
	NudgeDeadline time.Time // when to DM the on-call human (stage 1)
	Deadline      time.Time // when to escalate publicly (stage 2)
	Status        string
	AckedBy       string
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS pending_escalations (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		channel      TEXT NOT NULL,
		trigger_ts   TEXT NOT NULL,
		thread_ts    TEXT NOT NULL,
		text         TEXT NOT NULL DEFAULT '',
		requested_by TEXT NOT NULL DEFAULT '',
		deadline     INTEGER NOT NULL,
		status       TEXT NOT NULL DEFAULT 'open',
		acked_by     TEXT NOT NULL DEFAULT '',
		created      INTEGER NOT NULL,
		UNIQUE(channel, trigger_ts)
	);
	CREATE INDEX IF NOT EXISTS pending_due ON pending_escalations(status, deadline);`); err != nil {
		return err
	}
	// Additive metrics columns (idempotent).
	for _, col := range []string{
		"acked_at INTEGER",
		"ack_via TEXT NOT NULL DEFAULT ''",    // keyword | reaction
		"acker_kind TEXT NOT NULL DEFAULT ''", // oncall | responder
		"escalated_at INTEGER",
		"oncall_reply_by TEXT NOT NULL DEFAULT ''",
		"oncall_reply_at INTEGER",
		"nudge_deadline INTEGER",
		"nudged_at INTEGER",
		"late_ack_by TEXT NOT NULL DEFAULT ''", // ack signal that arrived AFTER escalation
		"late_ack_at INTEGER",
		"late_ack_via TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := s.db.Exec(`ALTER TABLE pending_escalations ADD COLUMN ` + col); err != nil {
			// Ignore "duplicate column" on re-run; surface anything else.
			if !isDupColumn(err) {
				return err
			}
		}
	}
	return nil
}

func isDupColumn(err error) bool {
	return err != nil && (contains(err.Error(), "duplicate column") || contains(err.Error(), "already exists"))
}
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

func (s *Store) Create(ctx context.Context, p *Pending) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO pending_escalations
		 (channel, trigger_ts, thread_ts, text, requested_by, deadline, nudge_deadline, status, created)
		 VALUES (?,?,?,?,?,?,?, 'open', ?)`,
		p.Channel, p.TriggerTS, p.ThreadTS, p.Text, p.RequestedBy,
		p.Deadline.Unix(), p.NudgeDeadline.Unix(), time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DueForNudge returns open incidents whose nudge (stage 1) is due and not yet sent.
func (s *Store) DueForNudge(ctx context.Context, now time.Time) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel, trigger_ts, thread_ts, text, requested_by
		 FROM pending_escalations
		 WHERE status='open' AND nudged_at IS NULL AND nudge_deadline IS NOT NULL AND nudge_deadline <= ?
		 ORDER BY nudge_deadline ASC LIMIT 50`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.Channel, &p.TriggerTS, &p.ThreadTS, &p.Text, &p.RequestedBy); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkNudged claims an incident's stage-1 nudge (returns false if already done).
func (s *Store) MarkNudged(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_escalations SET nudged_at=? WHERE id=? AND nudged_at IS NULL`,
		time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Ack marks the matching open escalation acknowledged. Returns whether a row
// was updated (false if already acked/escalated — used to avoid duplicate
// confirmations) and the thread to post the confirmation into.
// matchTrigger=true matches the reacted message against trigger_ts OR
// thread_ts — people react to the thread root (the request) as often as to
// the mention message itself. Safe for live reaction events only: the event
// fires when the reaction is added, so it's inherently post-trigger.
func (s *Store) Ack(ctx context.Context, channel, ts, ackedBy, via, kind string, matchTrigger bool) (acked bool, threadTS string, err error) {
	where := `thread_ts=?`
	args := []any{ackedBy, time.Now().Unix(), via, kind, channel, ts}
	if matchTrigger {
		where = `(trigger_ts=? OR thread_ts=?)`
		args = append(args, ts)
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE pending_escalations
		 SET status='acked', acked_by=?, acked_at=?, ack_via=?, acker_kind=?
		 WHERE channel=? AND `+where+` AND status='open'
		 RETURNING thread_ts`,
		args...)
	if scanErr := row.Scan(&threadTS); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", scanErr
	}
	return true, threadTS, nil
}

// LateAck records an ack signal that arrived after the escalation already
// fired. Status stays 'escalated'; this is visibility only. Only the FIRST
// late ack is recorded (returns false afterwards) so the "picked up" message
// posts once.
func (s *Store) LateAck(ctx context.Context, channel, ts, ackedBy, via string, matchTrigger bool) (recorded bool, threadTS string, err error) {
	where := `thread_ts=?`
	args := []any{ackedBy, time.Now().Unix(), via, channel, ts}
	if matchTrigger {
		where = `(trigger_ts=? OR thread_ts=?)`
		args = append(args, ts)
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE pending_escalations
		 SET late_ack_by=?, late_ack_at=?, late_ack_via=?
		 WHERE channel=? AND `+where+` AND status='escalated' AND late_ack_by=''
		 RETURNING thread_ts`,
		args...)
	if scanErr := row.Scan(&threadTS); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", scanErr
	}
	return true, threadTS, nil
}

// RecordOncallEngagement notes the first time an on-call member replied/reacted
// on an incident's thread or trigger message — even if it wasn't a formal ack.
// This is a metric only; it does not change status. ts may be a thread_ts or a
// trigger_ts.
func (s *Store) RecordOncallEngagement(ctx context.Context, channel, ts, user string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pending_escalations
		 SET oncall_reply_by=?, oncall_reply_at=?
		 WHERE channel=? AND (thread_ts=? OR trigger_ts=?) AND (oncall_reply_at IS NULL OR oncall_reply_at=0)`,
		user, time.Now().Unix(), channel, ts, ts)
	return err
}

func (s *Store) DueForEscalation(ctx context.Context, now time.Time) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel, trigger_ts, thread_ts, text, requested_by, deadline, status, acked_by
		 FROM pending_escalations WHERE status='open' AND deadline <= ? ORDER BY deadline ASC LIMIT 50`,
		now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		var deadline int64
		if err := rows.Scan(&p.ID, &p.Channel, &p.TriggerTS, &p.ThreadTS, &p.Text,
			&p.RequestedBy, &deadline, &p.Status, &p.AckedBy); err != nil {
			return nil, err
		}
		p.Deadline = time.Unix(deadline, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) MarkEscalated(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_escalations SET status='escalated', escalated_at=? WHERE id=? AND status='open'`,
		time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) ResetToOpen(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pending_escalations SET status='open', escalated_at=NULL WHERE id=? AND status='escalated'`, id)
	return err
}

// Metrics is an aggregate view over incidents for a time window.
type Metrics struct {
	TotalTags     int
	Acknowledged  int
	Escalated     int
	Open          int
	AckedByOncall int            // the actual on-call acknowledged
	AckedByOther  int            // another responder stepped in
	LateAcked     int            // escalated, but someone picked it up afterwards
	OncallEngaged int            // on-call replied/reacted at all (even w/o ack)
	AckSeconds    []int          // time-to-ack per acknowledged incident
	ByAcker       map[string]int // acker userID -> count
}

// Stats aggregates incidents created within the last `since` (0 = all time).
func (s *Store) Stats(ctx context.Context, since time.Duration) (*Metrics, error) {
	var minCreated int64
	if since > 0 {
		minCreated = time.Now().Add(-since).Unix()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, acked_by, acker_kind, created, acked_at, oncall_reply_at, late_ack_by
		 FROM pending_escalations WHERE created >= ?`, minCreated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := &Metrics{ByAcker: map[string]int{}}
	for rows.Next() {
		var status, ackedBy, ackerKind, lateAckBy string
		var created int64
		var ackedAt, oncallReplyAt sql.NullInt64
		if err := rows.Scan(&status, &ackedBy, &ackerKind, &created, &ackedAt, &oncallReplyAt, &lateAckBy); err != nil {
			return nil, err
		}
		m.TotalTags++
		switch status {
		case StatusAcked:
			m.Acknowledged++
			if ackedBy != "" {
				m.ByAcker[ackedBy]++
			}
			if ackerKind == AckerOncall {
				m.AckedByOncall++
			} else {
				m.AckedByOther++
			}
			if ackedAt.Valid {
				m.AckSeconds = append(m.AckSeconds, int(ackedAt.Int64-created))
			}
		case StatusEscalated:
			m.Escalated++
			if lateAckBy != "" {
				m.LateAcked++
			}
		case StatusOpen:
			m.Open++
		}
		if oncallReplyAt.Valid && oncallReplyAt.Int64 > 0 {
			m.OncallEngaged++
		}
	}
	return m, rows.Err()
}

// ExistsByTrigger reports whether ANY row exists for a channel+trigger message
// (any status) — used by reconciliation to avoid re-arming a handled message.
func (s *Store) ExistsByTrigger(ctx context.Context, channel, triggerTS string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pending_escalations WHERE channel=? AND trigger_ts=?`,
		channel, triggerTS).Scan(&n)
	return n > 0, err
}

// ListOpen returns open escalations in a channel (for ack reconciliation).
func (s *Store) ListOpen(ctx context.Context, channel string) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel, trigger_ts, thread_ts FROM pending_escalations
		 WHERE channel=? AND status='open'`, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.Channel, &p.TriggerTS, &p.ThreadTS); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OpenByThread finds an open escalation for a thread.
func (s *Store) OpenByThread(ctx context.Context, channel, threadTS string) (*Pending, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id FROM pending_escalations WHERE channel=? AND thread_ts=? AND status='open'`,
		channel, threadTS)
	var id int64
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &Pending{ID: id}, nil
}
