package workflow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
)

type providerPhaseEvidence struct {
	applicable map[string]struct{}
	empty      map[string]struct{}
	candidates map[string]struct{}
	errors     map[string]struct{}
}

type providerResumeAccumulator struct {
	exact providerPhaseEvidence
	broad providerPhaseEvidence
}

func (s *Service) RouteSignature(language domain.Language) string {
	return providerRouteSignature(language, s.ProviderOrder, s.FallbackProviderOrder)
}

func providerRouteSignature(language domain.Language, preferred, fallback []string) string {
	digest := sha256.New()
	writeRoutePart(digest, language.String())
	for _, id := range preferred {
		writeRoutePart(digest, "preferred")
		writeRoutePart(digest, id)
	}
	for _, id := range fallback {
		writeRoutePart(digest, "fallback")
		writeRoutePart(digest, id)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeRoutePart(target hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = target.Write(size[:])
	_, _ = target.Write([]byte(value))
}

func (s *Service) validatedResumeRequest(request Request) (Request, error) {
	if request.Manual || request.ResumeRouteSignature != s.RouteSignature(request.Language) {
		request.ResumeProviders = nil
		request.ResumeRouteSignature = ""
		return request, nil
	}

	membership := append(slices.Clone(s.ProviderOrder), s.FallbackProviderOrder...)
	seen := make(map[string]struct{}, len(request.ResumeProviders))
	for _, id := range request.ResumeProviders {
		if id == "" || !slices.Contains(membership, id) {
			return Request{}, fmt.Errorf("invalid provider resume state")
		}
		if _, duplicate := seen[id]; duplicate {
			return Request{}, fmt.Errorf("invalid provider resume state")
		}
		seen[id] = struct{}{}
	}
	request.ResumeProviders = slices.Clone(request.ResumeProviders)
	return request, nil
}

func (a *providerResumeAccumulator) consume(mode provider.SearchMode, result provider.SearchResult) {
	phase := &a.broad
	if mode == provider.SearchExactHash {
		phase = &a.exact
	}
	phase.ensure()
	for _, id := range result.ApplicableProviders {
		phase.applicable[id] = struct{}{}
	}
	for _, id := range result.EmptyProviders {
		phase.empty[id] = struct{}{}
	}
	for _, candidate := range result.Candidates {
		if candidate.ProviderID != "" {
			phase.candidates[candidate.ProviderID] = struct{}{}
		}
	}
	for id := range result.Errors {
		phase.errors[id] = struct{}{}
	}
}

func (p *providerPhaseEvidence) ensure() {
	if p.applicable == nil {
		p.applicable = make(map[string]struct{})
		p.empty = make(map[string]struct{})
		p.candidates = make(map[string]struct{})
		p.errors = make(map[string]struct{})
	}
}

func (a providerResumeAccumulator) cleanEmpty(providerOrder []string) []string {
	clean := make([]string, 0, len(providerOrder))
	for _, id := range providerOrder {
		if !a.broad.hasCleanEmpty(id) || a.broad.hasCandidateOrError(id) || a.exact.hasCandidateOrError(id) {
			continue
		}
		if _, exactApplicable := a.exact.applicable[id]; exactApplicable && !a.exact.hasCleanEmpty(id) {
			continue
		}
		clean = append(clean, id)
	}
	return clean
}

func (p providerPhaseEvidence) hasCleanEmpty(id string) bool {
	_, applicable := p.applicable[id]
	_, empty := p.empty[id]
	return applicable && empty
}

func (p providerPhaseEvidence) hasCandidateOrError(id string) bool {
	_, candidate := p.candidates[id]
	_, failed := p.errors[id]
	return candidate || failed
}

func (s *Service) mergeResumeProviders(groups ...[]string) []string {
	membership := make(map[string]struct{})
	for _, group := range groups {
		for _, id := range group {
			membership[id] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(membership))
	for _, id := range append(slices.Clone(s.ProviderOrder), s.FallbackProviderOrder...) {
		if _, found := membership[id]; found {
			ordered = append(ordered, id)
		}
	}
	return ordered
}

func (s *Service) finishProviderResume(request Request, installed bool, result Result, runErr error) Result {
	clean := result.cleanEmptyProviders
	result.cleanEmptyProviders = nil
	result.ResumeProviders = nil
	result.ResumeRouteSignature = ""
	if installed || runErr == nil && result.Outcome != OutcomeThrottled {
		return result
	}
	result.ResumeProviders = s.mergeResumeProviders(request.ResumeProviders, clean)
	if len(result.ResumeProviders) != 0 {
		result.ResumeRouteSignature = s.RouteSignature(request.Language)
	}
	return result
}
