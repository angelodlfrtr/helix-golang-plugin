package main

import (
	"reflect"
	"testing"
)

func TestSplitCamel(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"FooBarID", []string{"Foo", "Bar", "ID"}},
		{"HTTPServer", []string{"HTTP", "Server"}},
		{"UserURL", []string{"User", "URL"}},
		{"avatar_url", []string{"avatar", "url"}},
		{"Simple", []string{"Simple"}},
		{"APIKeyHeader", []string{"API", "Key", "Header"}},
	}

	for _, tt := range tests {
		got := splitCamel(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitCamel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestTransformName(t *testing.T) {
	tests := []struct {
		name      string
		transform string
		want      string
	}{
		{"AvatarURL", "snakecase", "avatar_url"},
		{"AvatarURL", "camelcase", "avatarUrl"},
		{"AvatarURL", "pascalcase", "AvatarUrl"},
		{"AvatarURL", "lispcase", "avatar-url"},
		{"AvatarURL", "keep", "AvatarURL"},
		{"UserFirstName", "snakecase", "user_first_name"},
	}

	for _, tt := range tests {
		got := transformName(tt.name, tt.transform)
		if got != tt.want {
			t.Errorf("transformName(%q, %q) = %q, want %q", tt.name, tt.transform, got, tt.want)
		}
	}
}

func TestTagEntryOperations(t *testing.T) {
	entries := []tagEntry{
		{key: "json", value: "foo"},
		{key: "xml", value: "bar"},
	}

	// Update existing
	entries = setTagKey(entries, "json", "foo,omitempty")
	if entries[0].value != "foo,omitempty" {
		t.Errorf("setTagKey update failed: %v", entries[0])
	}

	// Add new
	entries = setTagKey(entries, "db", "col_1")
	if len(entries) != 3 || entries[2].key != "db" {
		t.Errorf("setTagKey add failed: %v", entries)
	}

	// Delete
	entries = delTagKey(entries, "xml")
	if len(entries) != 2 || entries[1].key != "db" {
		t.Errorf("delTagKey failed: %v", entries)
	}
}
