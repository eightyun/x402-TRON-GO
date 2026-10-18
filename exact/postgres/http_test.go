package postgres

import "testing"

func TestUniqueHTTPJSON(t *testing.T) {
	for _, test := range []struct {
		input string
		valid bool
	}{
		{`{"a":{"b":[{"c":1},null,"value"]}}`, true},
		{`{"a":{},"b":[]}`, true},
		{`{"a":1,"a":2}`, false},
		{`{"a":{"amount":"1","Amount":"2"}}`, false},
		{`{"a":1,"\u0061":2}`, false},
		{`{"a":[{"b":1,"b":2}]}`, false},
		{`{"a":1} null`, false},
		{`{"a":]}`, false},
		{`{"a"`, false},
		{`[1`, false},
		{`[1,]`, false},
		{``, false},
	} {
		if got := uniqueHTTPJSON([]byte(test.input)); got != test.valid {
			t.Fatalf("JSON 歧义检测不符: %q", test.input)
		}
	}
}
