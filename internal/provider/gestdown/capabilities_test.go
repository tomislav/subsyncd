package gestdown

import "testing"

func TestGestdownIsSingleEpisodeOnly(t *testing.T) {
	if !(&Client{}).Capabilities().SingleEpisodeOnly {
		t.Fatal("Gestdown must not be searched for multi-episode files")
	}
}
