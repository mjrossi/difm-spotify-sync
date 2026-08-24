package difm

import (
	"net/http"
	"testing"
	"time"
)

// parseRetryAfter is deliberately duplicated between pkg/difm and
// pkg/spotify rather than shared. Both are independently usable libraries,
// and pkg/difm may not import internal/, so sharing eighteen lines of stdlib
// glue would mean a new exported package and a new import edge — API surface
// this project would then be committed to at v1.0.
//
// What that duplication costs is the risk of the two drifting, and only
// pkg/spotify's copy was tested (rotation_test.go). This is the mirror, kept
// deliberately identical to it: if a fix lands on one side only, one of the
// two fails.
//
// The behavior that matters to the caller: an unusable header yields 0
// rather than an error, so a rate limit is never misread as a transport
// failure — and never as an empty result, which is what would turn one 429
// into a permanent wrong verdict for every track it touched.
func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"", 0},
		{"not-a-number", 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	// The HTTP-date form is the other documented spelling. http.TimeFormat
	// is the GMT spelling RFC 7231 requires and real servers send.
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 30*time.Second || got > 50*time.Second {
		t.Errorf("parseRetryAfter(%q) = %s, want ~45s", future, got)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(past) = %s, want 0", got)
	}
}
