package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfterHeader(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  *time.Duration
	}{
		{name: "absent", value: "", want: nil},
		{name: "seconds", value: "8094", want: durationPtr(8094 * time.Second)},
		{name: "zero", value: "0", want: nil},
		{name: "negative", value: "-5", want: nil},
		{name: "absurd", value: "999999999", want: nil},
		{name: "garbage", value: "soon", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.value != "" {
				header.Set("Retry-After", tt.value)
			}
			got := parseRetryAfterHeader(header)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("parseRetryAfterHeader(%q) = %v, want nil", tt.value, *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("parseRetryAfterHeader(%q) = %v, want %v", tt.value, got, *tt.want)
			}
		})
	}
}

func TestParseRetryAfterHeaderHTTPDate(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", time.Now().Add(90*time.Minute).UTC().Format(http.TimeFormat))
	got := parseRetryAfterHeader(header)
	if got == nil {
		t.Fatal("parseRetryAfterHeader(http-date) = nil, want ~90m")
	}
	if *got < 85*time.Minute || *got > 90*time.Minute {
		t.Fatalf("parseRetryAfterHeader(http-date) = %v, want ~90m", *got)
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }
