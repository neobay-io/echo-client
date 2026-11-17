package core

import (
	"fmt"
	"strings"
	"time"
)

// upgradeWindow is a daily local-time range in minutes since midnight.
// end < start means the window wraps midnight.
type upgradeWindow struct {
	start, end int
}

// parseUpgradeWindow parses "HH:MM-HH:MM". An empty string means no window.
func parseUpgradeWindow(s string) (*upgradeWindow, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", s)
	}
	start, err := parseClockMinutes(from)
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", s, err)
	}
	end, err := parseClockMinutes(to)
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", s, err)
	}
	if start == end {
		return nil, fmt.Errorf("window %q is empty", s)
	}
	return &upgradeWindow{start: start, end: end}, nil
}

func parseClockMinutes(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (w *upgradeWindow) contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

// next returns t if it falls inside the window, otherwise the next start.
func (w *upgradeWindow) next(t time.Time) time.Time {
	if w == nil || w.contains(t) {
		return t
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), w.start/60, w.start%60, 0, 0, t.Location())
	if !start.After(t) {
		start = start.AddDate(0, 0, 1)
	}
	return start
}

// computeNextAutoRun decides when the next automatic run happens. The
// schedule survives restarts: it follows the persisted next run or the last
// run plus interval, not process start. A run missed while the service was
// down happens after grace.
func computeNextAutoRun(now, persistedNext, lastRun time.Time, interval, grace time.Duration, window *upgradeWindow) time.Time {
	var next time.Time
	switch {
	case !lastRun.IsZero():
		next = lastRun.Add(interval)
	case !persistedNext.IsZero():
		// No run yet: keep the first schedule instead of restarting the
		// countdown on every restart.
		next = persistedNext
	default:
		next = now.Add(interval)
	}
	if next.Before(now) {
		next = now.Add(grace)
	}
	return window.next(next)
}
