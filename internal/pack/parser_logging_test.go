package pack

import (
	"bytes"
	"log"
	"testing"
)

func TestSSAValidationDoesNotLogSubtitleContent(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	for _, extension := range []string{".ass", ".ssa", ""} {

		_, _, err := normalizeSubtitle([]byte("0\n[private-section]\n"), extension)
		if err == nil {
			t.Fatal("invalid subtitle accepted")
		}
	}
	if output.Len() != 0 {
		t.Fatalf("parser emitted raw subtitle content: %q", output.String())
	}
}
