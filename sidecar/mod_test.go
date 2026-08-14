package main

import (
	"testing"
)

func TestFirstLine(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"first line\nsecond line\nthird line", "first line"},
		{"  single line  ", "single line"},
		{"\nline after newline", "line after newline"},
		{"", ""},
	}

	for _, tt := range tests {
		got := firstLine(tt.input)
		if got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
