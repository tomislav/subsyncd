package workflow

import (
	"encoding/json"
	"subsyncd/internal/domain"
	"testing"
)

func TestProviderEvidenceSignatureCompatibility(t *testing.T) {
	decode := func(raw string) domain.Candidate {
		t.Helper()
		var c domain.Candidate
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	base := decode(`{"provider_id":"os","result_id":"42","language":"en"}`)
	converted := decode(`{"provider_id":"os","result_id":"42","language":"en","download_version":"srt-v1"}`)
	a, _ := candidateSignature(base)
	b, _ := candidateSignature(converted)
	if a == b {
		t.Fatal("converted download retains original-format rejection")
	}
	first := decode(`{"provider_id":"titlovi","result_id":"1","alternate_titles":["B","A","B"]}`)
	second := decode(`{"provider_id":"titlovi","result_id":"1","alternate_titles":["A","B"]}`)
	before, _ := json.Marshal(first)
	a, _ = candidateSignature(first)
	b, _ = candidateSignature(second)
	after, _ := json.Marshal(first)
	if a != b || string(before) != string(after) {
		t.Fatal("alternate-title signature is order-sensitive or mutates input")
	}
	third := decode(`{"provider_id":"titlovi","result_id":"1","alternate_titles":["A","C"]}`)
	b, _ = candidateSignature(third)
	if a == b {
		t.Fatal("changed returned title retains old rejection")
	}
}
