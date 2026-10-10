package workflow

import (
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

func TestSerbianSubtitleTaggedCroatianIsRejectedBeforeLapse(t *testing.T) {
	serbianSRT := "1\n00:00:01,000 --> 00:00:04,000\nGde si bio? Ovde je lepo vreme.\n\n2\n00:00:05,000 --> 00:00:08,000\nUvek isto, deca su na mestu. Nigde ne idem, ovde mi je lepo.\n"
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("serbian"), broadCandidate("croatian")}},
	}}
	sync := &fakeSynchronizer{}
	service := testService(t, inventory.Inventory{}, searcher, nil, sync, &fakeInstaller{})
	providerFake := &fakeProvider{id: "provider", payloads: map[string][]byte{"serbian": []byte(serbianSRT)}}
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.LapsePolicy.Mode = "always"
	request := serviceRequest(t)
	request.Language = "hr"
	for i := range searcher.results[provider.SearchBroad].Candidates {
		searcher.results[provider.SearchBroad].Candidates[i].Language = "hr"
	}
	result, err := service.Run(t.Context(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "croatian" {
		t.Fatalf("Run() = %s (%q), %v; want the Croatian candidate installed", result.Outcome, result.Candidate.ResultID, err)
	}
	for _, id := range sync.synchronized {
		if id == "serbian" {
			t.Fatal("the Serbian candidate reached LAPSE")
		}
	}
	rejected := false
	for _, rejection := range service.Repository.(*workflowRepository).rejections {
		if rejection.ResultID == "serbian" && rejection.ReasonCode == "language_mismatch" {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("rejections %+v; want serbian recorded as language_mismatch", service.Repository.(*workflowRepository).rejections)
	}
}
