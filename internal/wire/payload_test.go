package wire

import "testing"

func TestMarshalPayload(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"object stays object", map[string]any{"text": "hi"}, `{"text":"hi"}`},
		{"json-object string becomes object", `{"text": "hi"}`, `{"text":"hi"}`},
		{"padded json-object string", " \n{\"a\":1} ", `{"a":1}`},
		{"json-array string becomes array", `[1, 2]`, `[1,2]`},
		{"plain string unchanged", "hello", `"hello"`},
		{"invalid json-ish string unchanged", `{not json`, `"{not json"`},
		{"scalar-looking string unchanged", `123`, `"123"`},
		{"empty string unchanged", ``, `""`},
	}
	for _, c := range cases {
		got, err := MarshalPayload(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}
