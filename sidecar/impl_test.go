package main

import (
	"testing"
)

func TestReceiverBaseType(t *testing.T) {
	tests := []struct {
		recv string
		want string
	}{
		{"s *Server", "Server"},
		{"s Server", "Server"},
		{"*Server", "Server"},
		{"Server", "Server"},
		{"c *Client[T]", "Client"},
		{"invalid syntax !", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := receiverBaseType(tt.recv)
		if got != tt.want {
			t.Errorf("receiverBaseType(%q) = %q, want %q", tt.recv, got, tt.want)
		}
	}
}

func TestIsIdent(t *testing.T) {
	valid := []string{"Server", "_foo", "Bar123", "a_b_c"}
	for _, s := range valid {
		if !isIdent(s) {
			t.Errorf("isIdent(%q) = false, want true", s)
		}
	}

	invalid := []string{"123Bar", "foo-bar", "*Server", "", "a.b"}
	for _, s := range invalid {
		if isIdent(s) {
			t.Errorf("isIdent(%q) = true, want false", s)
		}
	}
}
