//go:build darwin

package connector

import "testing"

func TestPosixQuote(t *testing.T) {
	cases := map[string]string{
		`/Users/me/project`:    `'/Users/me/project'`,
		`/Users/me/my project`: `'/Users/me/my project'`,
		`/Users/me/it's`:       `'/Users/me/it'\''s'`,
		`/tmp/$(whoami)`:       `'/tmp/$(whoami)'`,
		"/tmp/a\nb":            `'/tmp/a` + "\n" + `b'`,
	}
	for in, want := range cases {
		if got := posixQuote(in); got != want {
			t.Fatalf("posixQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppleScriptStringEscapesBackslashBeforeQuote(t *testing.T) {
	cases := map[string]string{
		`plain`: `"plain"`,
		`a"b`:   `"a\"b"`,
		`a\b`:   `"a\\b"`,
		`a\"b`:  `"a\\\"b"`,
		"a\nb":  `"a\nb"`,
		"a\tb":  `"a\tb"`,
	}
	for in, want := range cases {
		if got := appleScriptString(in); got != want {
			t.Fatalf("appleScriptString(%q) = %q, want %q", in, got, want)
		}
	}
}
