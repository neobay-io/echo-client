package core

import (
	"fmt"
	"strings"
	"time"
)

// One-shot timers (T5.1). A timer is a cron job of kind "timer": it runs
// once at RunAt through the same delivery as cron (queued behind a busy
// chat, isolated agent session, output back to the chat), then stays in the
// list as fired for a day.

// maxTimerDelay bounds how far ahead a timer can be set.
const maxTimerDelay = 366 * 24 * time.Hour

// parseTimerWhen reads when a timer fires from the start of args: a delay
// ("10m", "1h30m", "2d"), a clock time ("14:30", the next one), or a date
// and time ("2026-11-20 09:00", "2026-11-20T09:00", RFC 3339). It returns
// the time and how many args it used.
func parseTimerWhen(args []string, now time.Time) (time.Time, int, error) {
	if len(args) == 0 {
		return time.Time{}, 0, fmt.Errorf("missing time")
	}
	first := strings.TrimSpace(args[0])
	at, used, err := func() (time.Time, int, error) {
		if d, ok := parseTimerDelay(first); ok {
			return now.Add(d), 1, nil
		}
		if t, err := time.ParseInLocation("15:04", first, now.Location()); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
			return at, 1, nil
		}
		if t, err := time.Parse(time.RFC3339, first); err == nil {
			return t, 1, nil
		}
		for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
			if t, err := time.ParseInLocation(layout, first, now.Location()); err == nil {
				return t, 1, nil
			}
		}
		if len(args) > 1 {
			joined := first + " " + strings.TrimSpace(args[1])
			for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
				if t, err := time.ParseInLocation(layout, joined, now.Location()); err == nil {
					return t, 2, nil
				}
			}
		}
		return time.Time{}, 0, fmt.Errorf("invalid time %q (use a delay like 30m, 2h, 1d, a time like 14:30, or a date like 2026-11-20 09:00)", first)
	}()
	if err != nil {
		return time.Time{}, 0, err
	}
	if !at.After(now) {
		return time.Time{}, 0, fmt.Errorf("%s is in the past", at.Format("2006-01-02 15:04"))
	}
	if at.Sub(now) > maxTimerDelay {
		return time.Time{}, 0, fmt.Errorf("timers can be set at most a year ahead")
	}
	return at, used, nil
}

// parseTimerDelay parses "45s", "10m", "1h30m" and whole days ("2d").
func parseTimerDelay(s string) (time.Duration, bool) {
	s = strings.ToLower(s)
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, true
	}
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil && days > 0 && fmt.Sprintf("%dd", days) == s {
			return time.Duration(days) * 24 * time.Hour, true
		}
	}
	return 0, false
}

// formatTimerRemaining is the time left until at, rounded for a countdown.
func formatTimerRemaining(at, now time.Time) string {
	d := at.Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int((d+time.Second-1)/time.Second))
	}
	mins := int((d + time.Minute - 1) / time.Minute)
	days, hours, m := mins/(24*60), (mins/60)%24, mins%60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if m > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if len(parts) == 0 {
		return "1m"
	}
	return strings.Join(parts, "")
}

func (e *Engine) cmdTimer(p Platform, msg *Message, args []string) {
	if e.cronScheduler == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronNotAvailable))
		return
	}
	if len(args) == 0 {
		e.cmdTimerList(p, msg)
		return
	}
	switch strings.ToLower(args[0]) {
	case "list", "ls":
		e.cmdTimerList(p, msg)
		return
	case "del", "delete", "rm", "remove", "cancel":
		e.cmdTimerDel(p, msg, args[1:])
		return
	case "exec":
		e.cmdTimerAdd(p, msg, args[1:], true)
		return
	case "help":
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("timer_usage")))
		return
	}
	e.cmdTimerAdd(p, msg, args, false)
}

func (e *Engine) cmdTimerAdd(p Platform, msg *Message, args []string, shell bool) {
	now := time.Now()
	at, used, err := parseTimerWhen(args, now)
	if err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v\n\n%s", err, e.i18n.T(MsgKey("timer_usage"))))
		return
	}
	text := strings.TrimSpace(strings.Join(args[used:], " "))
	if text == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("timer_usage")))
		return
	}
	if len(text) > maxCronPromptLen {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ prompt too long (max %d characters)", maxCronPromptLen))
		return
	}
	job := &CronJob{
		ID:         GenerateCronID(),
		Project:    e.name,
		SessionKey: msg.SessionKey,
		Kind:       "timer",
		RunAt:      at,
		Enabled:    true,
		CreatedAt:  now,
	}
	if shell {
		job.Exec = text
	} else {
		job.Prompt = text
	}
	if err := e.cronScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ %v", err))
		return
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgKey("timer_added")), job.ID, at.Format(cronTimeFormat(at, now)), formatTimerRemaining(at, now), truncateStr(text, 60)))
}

func (e *Engine) cmdTimerList(p Platform, msg *Message) {
	jobs := e.jobsForSessionByKind(msg.SessionKey, "timer")
	if len(jobs) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("timer_empty")))
		return
	}
	now := time.Now()
	var sb strings.Builder
	fmt.Fprintf(&sb, e.i18n.T(MsgKey("timer_list_title")), len(jobs))
	for _, j := range jobs {
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = "🖥 " + firstNonEmptyLine(j.Exec)
			} else {
				desc = firstNonEmptyLine(j.Prompt)
			}
		}
		state := "⏰ " + formatTimerRemaining(j.RunAt, now)
		switch {
		case !j.Enabled && j.LastError != "":
			state = "❌ " + truncateStr(j.LastError, 80)
		case !j.Enabled:
			state = "✅"
		}
		fmt.Fprintf(&sb, "\n\n%s %s\nID: %s · %s", state, truncateStr(desc, 60), j.ID, j.RunAt.Format(cronTimeFormat(j.RunAt, now)))
	}
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdTimerDel(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("timer_usage")))
		return
	}
	id := args[0]
	job := e.cronScheduler.Store().Get(id)
	if job == nil || !job.IsTimerJob() || job.SessionKey != msg.SessionKey {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("timer_not_found")))
		return
	}
	if !e.cronScheduler.RemoveJob(id) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf("❌ failed to delete `%s`", id))
		return
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgKey("timer_deleted")), id))
}

// normalizeJobSettings checks a scheduled job's own agent settings (T5.3)
// against what this project's agent offers and stores them in the agent's
// form. Unknown values are refused rather than dropped.
func (e *Engine) normalizeJobSettings(job *CronJob) error {
	if id := job.AgentSessionID; id != "" {
		if len(id) > 256 || strings.IndexFunc(id, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
			return fmt.Errorf("invalid agent_session_id")
		}
	}
	if job.Model != "" || job.ReasoningLevel != "" || job.Mode != "" {
		// Settings reach a job's session only through SessionOptionsInjector;
		// accepting them for other agents would run the job with the
		// project's settings instead (e.g. bypass for a job asking for plan).
		if _, ok := e.agent.(SessionOptionsInjector); !ok {
			return fmt.Errorf("agent %s cannot give a job its own model, effort or mode", e.agent.Name())
		}
	}
	if job.Mode != "" {
		switcher, ok := e.agent.(ModeSwitcher)
		if !ok {
			return fmt.Errorf("agent %s has no permission modes", e.agent.Name())
		}
		mode, ok := resolveModeForSwitcher(e.agent.Name(), switcher, job.Mode)
		if !ok {
			return fmt.Errorf("unknown mode %q for %s", job.Mode, e.agent.Name())
		}
		job.Mode = mode
	}
	if job.ReasoningLevel != "" {
		switcher, ok := e.agent.(ReasoningLevelSwitcher)
		if !ok {
			return fmt.Errorf("agent %s has no reasoning levels", e.agent.Name())
		}
		want := strings.ToLower(job.ReasoningLevel)
		found := false
		for _, l := range switcher.AvailableReasoningLevels() {
			if strings.EqualFold(l.Name, want) {
				job.ReasoningLevel, found = l.Name, true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown reasoning_level %q for %s", job.ReasoningLevel, e.agent.Name())
		}
	}
	if job.Model != "" && (len(job.Model) > 200 || strings.ContainsAny(job.Model, " \t\r\n")) {
		return fmt.Errorf("invalid model %q", job.Model)
	}
	return nil
}
