package usagewin

import (
	"strconv"
	"testing"
	"time"
)

func TestDetectRejection(t *testing.T) {
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	reset := now.Add(3*time.Hour + 30*time.Minute)
	epoch := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	cases := []struct {
		name, rl, text string
		ok             bool
		win            Window
		at             time.Time
		src            string
	}{
		{"event top-level", `{"status":"rejected","rateLimitType":"five_hour","resetsAt":` + epoch(reset) + `}`, "", true, Win5h, reset, "event"},
		{"event unified only", `{"status":"rejected","rateLimitType":"seven_day","unifiedWindows":{"seven_day":{"utilization":1,"resetsAt":` + epoch(reset) + `,"status":"rejected"}}}`, "", true, Win7d, reset, "event"},
		{"event no reset", `{"status":"rejected","rateLimitType":"five_hour"}`, "", true, Win5h, time.Time{}, "event"},
		{"event allowed", `{"status":"allowed","rateLimitType":"five_hour","resetsAt":` + epoch(reset) + `}`, "", false, "", time.Time{}, ""},
		{"event stale reset dropped", `{"status":"rejected","rateLimitType":"five_hour","resetsAt":` + epoch(now.Add(-time.Hour)) + `}`, "", true, Win5h, time.Time{}, "event"},
		{"text pipe epoch", "", "Claude AI usage limit reached|" + epoch(reset), true, "", reset, "text"},
		{"text clock utc", "", "You've hit your limit · resets 11:30pm (UTC)", true, "", time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC), "text"},
		{"text clock rolls to tomorrow", "", "5-hour limit reached - resets 7:00am", true, Win5h, time.Date(2026, 11, 17, 7, 0, 0, 0, time.UTC), "text"},
		{"text weekly", "", "Weekly limit reached, resets Nov 20", true, Win7d, time.Time{}, "text"},
		{"text unrelated 429", "", "API error 429 too many requests", false, "", time.Time{}, ""},
		{"garbage", "{nope", "", false, "", time.Time{}, ""},
	}
	for _, c := range cases {
		r, ok := DetectRejection(c.rl, c.text, now)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if r.Window != c.win || !r.ResetsAt.Equal(c.at) || r.Source != c.src {
			t.Errorf("%s: got %+v want win=%q at=%v src=%s", c.name, r, c.win, c.at, c.src)
		}
	}
}
