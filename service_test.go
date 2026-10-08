package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPasswordFile(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		content  string
		expected string
		wantErr  bool
	}{
		{name: "bare", content: "NghD5", expected: "NghD5"},
		{name: "one trailing newline", content: "a b\n", expected: "a b"},
		{name: "crlf", content: "a\r\n", expected: "a"},
		{name: "spaces kept", content: " a \n", expected: " a "},
		{name: "empty", content: "\n", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "letterboxd-password")
			if err := os.WriteFile(path, []byte(testCase.content), 0o600); err != nil {
				t.Fatalf("write file: %v", err)
			}

			password, err := readPasswordFile(path)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil || password != testCase.expected {
				t.Errorf("unexpected: %q %v", password, err)
			}
		})
	}

	if _, err := readPasswordFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error for a missing file")
	}
}
