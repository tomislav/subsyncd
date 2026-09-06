package workflow

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestSSAValidationDoesNotLogSubtitleContent(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	for _, extension := range []string{".ass", ".ssa"} {
		path := filepath.Join(t.TempDir(), "subtitle"+extension)
		if err := os.WriteFile(path, []byte("0\n[private-section]\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := validatedSubtitle(path, 0)
		if err == nil {
			t.Fatal("invalid subtitle accepted")
		}
	}
	if output.Len() != 0 {
		t.Fatalf("parser emitted raw subtitle content: %q", output.String())
	}
}
