package wire

import "testing"

func TestSessionUsageValidate(t *testing.T) {
	base := SessionUsage{SessionID: "ses_a", Role: SessionRoleWorker}
	cases := []struct {
		name string
		mut  func(*SessionUsage)
		want bool // want valid
	}{
		{"valid worker session", func(*SessionUsage) {}, true},
		{"valid planner session", func(u *SessionUsage) { u.Role = SessionRolePlanner }, true},
		{"valid brain session", func(u *SessionUsage) { u.Role = SessionRoleForemanBrain }, true},
		{"missing session id", func(u *SessionUsage) { u.SessionID = "" }, false},
		{"unknown role", func(u *SessionUsage) { u.Role = "boss" }, false},
		{"negative cost", func(u *SessionUsage) { u.CostUSD = -0.01 }, false},
		{"negative tokens", func(u *SessionUsage) { u.InputTokens = -1 }, false},
		{"negative cache read", func(u *SessionUsage) { u.CacheReadTokens = -1 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := base
			tc.mut(&u)
			err := u.Validate()
			if tc.want && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tc.want && err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}
