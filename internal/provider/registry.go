package provider

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
)

type InstanceSpec struct {
	ID       string
	Type     string
	Settings yaml.Node
}

type Registry struct {
	factories map[string]Factory
}

func NewRegistry(factories map[string]Factory) *Registry {
	copyOfFactories := make(map[string]Factory, len(factories))
	for providerType, factory := range factories {
		copyOfFactories[strings.ToLower(providerType)] = factory
	}
	return &Registry{factories: copyOfFactories}
}

func (r *Registry) Build(specs []InstanceSpec, dependencies Dependencies) (map[string]Provider, error) {
	providers := make(map[string]Provider, len(specs))
	for _, spec := range specs {
		if spec.ID == "" {
			return nil, fmt.Errorf("provider instance ID is empty")
		}
		if _, duplicate := providers[spec.ID]; duplicate {
			return nil, fmt.Errorf("duplicate provider instance %q", spec.ID)
		}
		factory, ok := r.factories[strings.ToLower(spec.Type)]
		if !ok {
			return nil, fmt.Errorf("unknown provider type %q for instance %q", spec.Type, spec.ID)
		}
		provider, err := factory(spec.ID, spec.Settings, dependencies)
		if err != nil {
			return nil, fmt.Errorf("configure provider %s: %w", spec.ID, err)
		}
		if provider == nil || provider.ID() != spec.ID {
			return nil, fmt.Errorf("provider factory %q returned an invalid instance", spec.Type)
		}
		providers[spec.ID] = provider
	}
	return providers, nil
}

func ValidateLanguageRoutes(routes map[domain.Language][]string, providers map[string]Provider) error {
	for language, providerIDs := range routes {
		if len(providerIDs) == 0 {
			return fmt.Errorf("language %s has no providers", language)
		}
		for _, providerID := range providerIDs {
			provider, ok := providers[providerID]
			if !ok {
				return fmt.Errorf("language %s references unknown provider %q", language, providerID)
			}
			if !provider.SupportsLanguage(language) {
				return fmt.Errorf("provider %q does not support language %s", providerID, language)
			}
		}
	}
	return nil
}
