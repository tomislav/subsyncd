package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCandidateJSONOmitsFalseForcedFlag(t *testing.T) {
	normal, err := json.Marshal(Candidate{ProviderID: "provider", ResultID: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(normal), `"forced"`) {
		t.Fatalf("ordinary candidate unexpectedly changed legacy JSON shape: %s", normal)
	}

	forced, err := json.Marshal(Candidate{ProviderID: "provider", ResultID: "forced", Forced: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(forced), `"forced":true`) {
		t.Fatalf("forced candidate did not persist its safety flag: %s", forced)
	}
}
