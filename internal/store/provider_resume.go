package store

import (
	"encoding/json"
	"fmt"

	"subsyncd/internal/domain"
)

// ProviderResumeRoute supplies the accepted configuration's exact provider
// identities and opaque route signature. It adds no configuration size limit.
type ProviderResumeRoute struct {
	Signature string
	Providers []string
}

type providerResumeBound struct {
	signature string
	jsonBytes int
	providers map[string]struct{}
}

// WithProviderResumeRoutes bounds persisted resume decoding to the configured
// language route. Stale signatures are invalidated before applying the current
// bound, so removing providers cannot strand a previously valid larger record.
func (r *Repository) WithProviderResumeRoutes(routes map[domain.Language]ProviderResumeRoute) *Repository {
	clone := *r
	clone.resumeRoutes = make(map[domain.Language]providerResumeBound, len(routes))
	for language, route := range routes {
		encoded, _ := json.Marshal(route.Providers)
		bound := providerResumeBound{signature: route.Signature, jsonBytes: len(encoded), providers: make(map[string]struct{}, len(route.Providers))}
		for _, id := range route.Providers {
			bound.providers[id] = struct{}{}
		}
		clone.resumeRoutes[language] = bound
	}
	return &clone
}

func (r *Repository) currentResumeScope(language string, fingerprint domain.MediaFingerprint, signature string) bool {
	if r.resumeRoutes == nil {
		return true
	}
	route, exists := r.resumeRoutes[domain.Language(language)]
	return exists && signature == domain.ProviderResumeSignature(route.signature, fingerprint)
}

func (r *Repository) decodeSearchResume(language, encoded string) ([]string, error) {
	bound, bounded := r.resumeRoutes[domain.Language(language)]
	if bounded && len(encoded) > bound.jsonBytes {
		return nil, fmt.Errorf("invalid search resume providers")
	}
	providers, err := decodeResumeProviders(encoded)
	if err != nil {
		return nil, err
	}
	if bounded {
		if len(providers) > len(bound.providers) {
			return nil, fmt.Errorf("invalid search resume providers")
		}
		for _, id := range providers {
			if _, exists := bound.providers[id]; !exists {
				return nil, fmt.Errorf("invalid search resume providers")
			}
		}
	}
	return providers, nil
}
