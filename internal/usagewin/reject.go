package usagewin

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Rejection is a hard usage-limit rejection seen on one claude run (token
// throttle S5): the run was refused because a subscription window is
// exhausted.
type Rejection struct {
	// Window is the exhausted window; "" when the signal did not say.
	Window Window
	// ResetsAt is when the CLI says the window resets; zero when unknown.
	ResetsAt time.Time
	// Source is "event" (rate_limit_event status rejected) or "text" (the
	// CLI's limit-reached result/stderr text).
	Source string
}

// WindowLabel is the operator-facing name: "5h", "7d" or "usage".
func WindowLabel(w Window) string {
	switch w {
	case Win5h:
		return "5h"
	case Win7d:
		return "7d"
	}
	return "usage"
}

// maxResetHorizon bounds a believable reset time: a parsed reset further
// out than the longest window (plus slack) is treated as unknown.
const maxResetHorizon = SevenDay + 24*time.Hour

type rejInfo struct {
	Status        string   `json:"status"`
	ResetsAt      *float64 `json:"resetsAt"`
	RateLimitType string   `json:"rateLimitType"`
	Unified       map[string]struct {
		ResetsAt *float64 `json:"resetsAt"`
		Status   string   `json:"status"`
	} `json:"unifiedWindows"`
}

// DetectRejection reports a hard rejection from a run's captured signals:
// rateLimitJSON is the rate_limit_event.rate_limit_info JSON (Usage.RateLimit)
// and limitText the CLI's limit-reached text (Usage.Limit). The event wins;
// the text is the fallback. A reset time in the past or beyond the longest
// window is dropped (ResetsAt zero), so a stale replay cannot extend a pause.
func DetectRejection(rateLimitJSON, limitText string, now time.Time) (Rejection, bool) {
	var info rejInfo
	if rateLimitJSON != "" && json.Unmarshal([]byte(rateLimitJSON), &info) == nil {
		rejected := info.Status == "rejected"
		var win Window
		if rejected {
			win = windowOf(info.RateLimitType)
		}
		var reset *float64
		if rejected && info.ResetsAt != nil {
			reset = info.ResetsAt
		}
		for name, e := range info.Unified {
			w := windowOf(name)
			if e.Status == "rejected" {
				rejected = true
				if win == "" {
					win = w
				}
			}
			if w == win && win != "" && reset == nil && e.ResetsAt != nil {
				reset = e.ResetsAt
			}
		}
		if rejected {
			r := Rejection{Window: win, Source: "event"}
			if reset != nil {
				r.ResetsAt = sane(time.Unix(int64(*reset), 0).UTC(), now)
			}
			return r, true
		}
	}
	if limitText != "" && limitTextRe.MatchString(limitText) {
		r := Rejection{Source: "text"}
		low := strings.ToLower(limitText)
		switch {
		case strings.Contains(low, "weekly"), strings.Contains(low, "7-day"), strings.Contains(low, "seven_day"), strings.Contains(low, "7 day"):
			r.Window = Win7d
		case strings.Contains(low, "5-hour"), strings.Contains(low, "5 hour"), strings.Contains(low, "five_hour"), strings.Contains(low, "session limit"):
			r.Window = Win5h
		}
		r.ResetsAt = sane(parseResetText(limitText, now), now)
		return r, true
	}
	return Rejection{}, false
}

// IsLimitText reports whether text reads as a hard limit-reached message
// (the strict subset of S1's capture regex: no bare "rate limit" or "429").
func IsLimitText(text string) bool { return limitTextRe.MatchString(text) }

var limitTextRe = regexp.MustCompile(`(?i)usage limit reached|limit reached|hit your (?:\w+ )?limit|reached your (?:\w+ )?(?:usage )?limit`)

var (
	pipeEpochRe = regexp.MustCompile(`\|\s*(\d{9,11})\b`)
	clockRe     = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([A-Za-z_/+\-0-9]+)\))?`)
)

// parseResetText extracts a reset time from limit text: the legacy
// "...limit reached|<epoch>" form, or a clock time ("resets 11:30pm",
// "resets 23:30 (UTC)"; the next such time after now, in the named zone,
// else UTC). Zero when nothing parses.
func parseResetText(text string, now time.Time) time.Time {
	if m := pipeEpochRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Unix(n, 0).UTC()
		}
	}
	m := clockRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	h, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	switch strings.ToLower(m[3]) {
	case "pm":
		if h < 12 {
			h += 12
		}
	case "am":
		if h == 12 {
			h = 0
		}
	}
	if h > 23 || min > 59 {
		return time.Time{}
	}
	loc := time.UTC
	if m[4] != "" && !strings.EqualFold(m[4], "utc") {
		l, err := time.LoadLocation(m[4])
		if err != nil {
			return time.Time{} // unknown zone: do not guess
		}
		loc = l
	}
	n := now.In(loc)
	t := time.Date(n.Year(), n.Month(), n.Day(), h, min, 0, 0, loc)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t.UTC()
}

func sane(t, now time.Time) time.Time {
	if t.IsZero() || !t.After(now) || t.Sub(now) > maxResetHorizon {
		return time.Time{}
	}
	return t
}

func windowOf(name string) Window {
	switch name {
	case string(Win5h):
		return Win5h
	case string(Win7d), "seven_day_opus", "seven_day_sonnet":
		return Win7d
	}
	return ""
}
