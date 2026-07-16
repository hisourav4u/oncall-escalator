package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"time"
)

// handleStats returns aggregate on-call metrics as JSON. Gated by StatsToken
// (?token=…). Disabled (404) when no token is configured.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.statsToken == "" {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("token") != s.statsToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var window time.Duration
	label := "all-time"
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			window = time.Duration(n) * 24 * time.Hour
			label = d + "d"
		}
	}

	m, err := s.stats(r.Context(), window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// time-to-ack stats (Go-side; datasets are small).
	secs := append([]int(nil), m.AckSeconds...)
	sort.Ints(secs)
	median, p90, avg := 0, 0, 0
	if n := len(secs); n > 0 {
		median = secs[n/2]
		p90 = secs[(n*90)/100-boolToInt((n*90)%100 == 0)]
		if p90 < 0 {
			p90 = secs[n-1]
		}
		sum := 0
		for _, v := range secs {
			sum += v
		}
		avg = sum / n
	}

	// Resolve acker IDs → names for a shareable breakdown.
	byActer := make([]map[string]any, 0, len(m.ByAcker))
	for id, count := range m.ByAcker {
		byActer = append(byActer, map[string]any{"user": s.nameFor(r.Context(), id), "acks": count})
	}
	sort.Slice(byActer, func(i, j int) bool { return byActer[i]["acks"].(int) > byActer[j]["acks"].(int) })

	rate := func(n int) float64 {
		if m.TotalTags == 0 {
			return 0
		}
		return float64(n) / float64(m.TotalTags)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window":                label,
		"total_tags":            m.TotalTags,
		"acknowledged":          m.Acknowledged,
		"escalated":             m.Escalated,
		"open":                  m.Open,
		"ack_rate":              round2(rate(m.Acknowledged)),
		"escalation_rate":       round2(rate(m.Escalated)),
		"acked_by_oncall":       m.AckedByOncall,
		"acked_by_other":        m.AckedByOther,
		"late_acked":            m.LateAcked,
		"oncall_engaged":        m.OncallEngaged,
		"oncall_engaged_rate":   round2(rate(m.OncallEngaged)),
		"time_to_ack_seconds":   map[string]int{"median": median, "avg": avg, "p90": p90},
		"acks_by_person":        byActer,
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
