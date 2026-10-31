package main

import "testing"

func TestParseServePort(t *testing.T) {
	cases := map[string]struct {
		port int
		ok   bool
	}{
		"Serving HTTP on http://127.0.0.1:32773/": {32773, true},
		" Serving HTTP on http://127.0.0.1:8080/": {8080, true},
		"Serving HTTP on http://127.0.0.1:8080":   {8080, true},
		"Serving HTTP on http://0.0.0.0:8080/":    {0, false},
		"Serving HTTP on http://127.0.0.1:nope/":  {0, false},
		"unrelated output":                        {0, false},
	}
	for line, want := range cases {
		port, ok := parseServePort(line)
		if port != want.port || ok != want.ok {
			t.Errorf("parseServePort(%q) = %d, %v; want %d, %v", line, port, ok, want.port, want.ok)
		}
	}
}
