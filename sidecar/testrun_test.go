package main

import (
	"testing"
)

func TestFailLocRe(t *testing.T) {
	tests := []struct {
		input    string
		wantFile string
		wantLine int
	}{
		{
			input:    "    /usr/local/go/src/pkg/foo_test.go:42: assertion failed",
			wantFile: "/usr/local/go/src/pkg/foo_test.go",
			wantLine: 42,
		},
		{
			input:    "    my-file_v1.0@v1.0.0/foo.go:100: failed",
			wantFile: "my-file_v1.0@v1.0.0/foo.go",
			wantLine: 100,
		},
		{
			input:    "    C:\\Users\\dev\\project+app\\main_test.go:15: error",
			wantFile: "C:\\Users\\dev\\project+app\\main_test.go",
			wantLine: 15,
		},
	}

	for _, tt := range tests {
		m := failLocRe.FindStringSubmatch(tt.input)
		if m == nil {
			t.Errorf("failLocRe failed to match %q", tt.input)
			continue
		}
		if m[1] != tt.wantFile {
			t.Errorf("failLocRe file = %q, want %q", m[1], tt.wantFile)
		}
		if atoi(m[2]) != tt.wantLine {
			t.Errorf("failLocRe line = %v, want %v", m[2], tt.wantLine)
		}
	}
}

func TestNoiseLine(t *testing.T) {
	noise := []string{
		"=== RUN   TestSomething",
		"--- PASS: TestSomething (0.00s)",
		"=== PAUSE TestSomething",
		"=== CONT  TestSomething",
		"PASS",
		"FAIL",
	}
	for _, s := range noise {
		if !noiseLine.MatchString(s) {
			t.Errorf("noiseLine expected to match %q", s)
		}
	}

	validOutput := []string{
		"t.Log(\"hello world\")",
		"    foo_test.go:12: error message",
	}
	for _, s := range validOutput {
		if noiseLine.MatchString(s) {
			t.Errorf("noiseLine should NOT match %q", s)
		}
	}
}
