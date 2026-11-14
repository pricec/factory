package wire

import "testing"

func TestSmoke(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "identity", in: "wire", want: "wire"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in; got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
