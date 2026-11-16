package claudecli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Usage is the token and cost spend the CLI reports for one run
// (`--output-format json`). The zero value means "nothing parsed".
type Usage struct {
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheCreate int     `json:"cache_create"`
	CacheRead   int     `json:"cache_read"`
	CostUSD     float64 `json:"cost_usd"`
	Model       string  `json:"model,omitempty"`
	SessionID   string  `json:"session_id,omitempty"`
	// Limit is the raw rate-limit / usage-limit text the CLI emitted
	// (e.g. "Claude AI usage limit reached|1760000000"), "" when none.
	Limit string `json:"limit,omitempty"`
}

// IsZero reports whether nothing was parsed.
func (u Usage) IsZero() bool { return u == Usage{} }

// Output is a parsed CLI stdout.
type Output struct {
	// Text is the result text; the raw stdout when it was not parseable.
	Text string
	// Usage is zero when stdout was not a result object.
	Usage Usage
	// Parsed reports whether a result object was found.
	Parsed bool
	// IsError mirrors the result object's is_error.
	IsError bool
}

type resultObj struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       *string `json:"result"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        *struct {
		Input       int `json:"input_tokens"`
		Output      int `json:"output_tokens"`
		CacheCreate int `json:"cache_creation_input_tokens"`
		CacheRead   int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

var limitRe = regexp.MustCompile(`(?i)usage limit|limit reached|hit your limit|rate.?limit|too many requests|\b429\b|limit will reset|resets? (at|in|on)?\s*\d`)

// ParseOutput parses `claude --output-format json` stdout. It accepts one
// result object or an array of events containing one. Anything else
// (garbage, empty, plain text) is returned verbatim as Text with
// Parsed=false: parsing never fails.
func ParseOutput(raw string) Output {
	fallback := Output{Text: raw}
	trim := bytes.TrimSpace([]byte(raw))
	if len(trim) == 0 {
		return fallback
	}
	var obj *resultObj
	switch trim[0] {
	case '{':
		var o resultObj
		if json.Unmarshal(trim, &o) == nil {
			obj = &o
		}
	case '[':
		var arr []resultObj
		if json.Unmarshal(trim, &arr) == nil {
			for i := len(arr) - 1; i >= 0; i-- {
				if arr[i].Type == "result" {
					obj = &arr[i]
					break
				}
			}
		}
	}
	if obj == nil || (obj.Type != "result" && obj.Result == nil && obj.Usage == nil) {
		return fallback
	}
	out := Output{Parsed: true, IsError: obj.IsError, Text: ""}
	if obj.Result != nil {
		out.Text = *obj.Result
	}
	u := Usage{CostUSD: obj.TotalCostUSD, SessionID: obj.SessionID}
	if obj.Usage != nil {
		u.Input, u.Output = obj.Usage.Input, obj.Usage.Output
		u.CacheCreate, u.CacheRead = obj.Usage.CacheCreate, obj.Usage.CacheRead
	}
	if len(obj.ModelUsage) > 0 {
		names := make([]string, 0, len(obj.ModelUsage))
		for k := range obj.ModelUsage {
			names = append(names, k)
		}
		sort.Strings(names)
		u.Model = strings.Join(names, ",")
	}
	if (obj.IsError || strings.Contains(strings.ToLower(obj.Subtype), "limit")) && limitRe.MatchString(out.Text) {
		u.Limit = out.Text
	}
	out.Usage = u
	return out
}

// Text returns the result text of raw CLI stdout (raw itself when not
// parseable).
func Text(raw string) string { return ParseOutput(raw).Text }
