package run

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBrief checks that a tag's error is kept whole up to maxTagError bytes, and beyond that cut short, at a
// character's start, saying so.
func TestBrief(t *testing.T) {
	short := strings.Repeat("a", maxTagError)
	if got := brief(short); got != short {
		t.Errorf("brief of %d bytes = %d bytes, want it whole", len(short), len(got))
	}
	long := strings.Repeat("a", maxTagError-1) + strings.Repeat("é", maxTagError)
	got := brief(long)
	if !strings.HasSuffix(got, "(cut short)") || len(got) > maxTagError+len(" … (cut short)") ||
		!utf8.ValidString(got) || !strings.HasPrefix(got, strings.Repeat("a", maxTagError-1)) {
		t.Errorf("brief of %d bytes = %q, want its first %d bytes or fewer, whole characters, cut short",
			len(long), got, maxTagError)
	}
}
