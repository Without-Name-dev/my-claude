package store

import "testing"

// The server runs on Linux but receives Windows paths, so BaseName must not
// depend on the host separator.
func TestBaseNameHandlesBothSeparators(t *testing.T) {
	cases := map[string]string{
		`C:\Users\me\repo`:  "repo",
		`C:/Users/me/repo/`: "repo",
		"/home/me/repo":     "repo",
		`D:\`:               `D:`,
		"repo":              "repo",
		`C:\Users\me\`:      "me",
	}
	for input, want := range cases {
		if got := BaseName(input); got != want {
			t.Errorf("BaseName(%q) = %q, want %q", input, got, want)
		}
	}
}
