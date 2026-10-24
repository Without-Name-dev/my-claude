package connector

import "testing"

func TestAdvanceReportsOnlyNewText(t *testing.T) {
	cases := []struct {
		name string
		prev string
		next string
		want string
	}{
		{"first chunk", "", "hello", "hello"},
		{"extension", "hello", "hello world", " world"},
		{"unchanged", "hello", "hello", ""},
		{"head dropped", "abcdefgh", "defghij", "ij"},
		{"no overlap", "abc", "xyz", "xyz"},
		{"shorter with overlap", "abcdef", "efg", "g"},
		{"only truncation", "abcdef", "cdef", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := advance(tc.prev, tc.next); got != tc.want {
				t.Fatalf("advance(%q, %q) = %q, want %q", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

func TestAdvanceRebuildsTextAfterTruncation(t *testing.T) {
	// A turn longer than the plugin's 20,000 character tail: the head disappears
	// and the remaining text must still arrive without duplication.
	head := "0123456789"
	full := head + head + head
	tail := full[len(head):]

	var got string
	prev := ""
	for _, step := range []string{head, full, tail} {
		got += advance(prev, step)
		prev = step
	}
	if got != full {
		t.Fatalf("rebuilt text = %q, want %q", got, full)
	}
}
