package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidOutputContentIsDistinctFromTechnicalFailure(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"syntax", "not a subtitle"},
		{"timestamps", "1\n00:00:03,000 --> 00:00:04,000\nLater\n\n2\n00:00:01,000 --> 00:00:02,000\nEarlier\n"},
		{"encoding", string([]byte{255, 254, 0})},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "output.srt")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			err := validateOutput(path)
			var invalid *InvalidOutputError
			if !errors.As(err, &invalid) {
				t.Fatalf("content failure is not typed: %T %v", err, err)
			}
		})
	}
	for _, name := range []string{"missing", "directory"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "output.srt")
			if name == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := validateOutput(path)
			var invalid *InvalidOutputError
			if err == nil || errors.As(err, &invalid) {
				t.Fatalf("technical failure misclassified: %v", err)
			}
		})
	}
}
