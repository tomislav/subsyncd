package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
)

type registryProvider struct {
	id        string
	languages map[domain.Language]bool
	token     string
}

func (p *registryProvider) ID() string                 { return p.id }
func (p *registryProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *registryProvider) SupportsLanguage(language domain.Language) bool {
	return p.languages[language]
}
func (p *registryProvider) Search(context.Context, SearchQuery) ([]domain.Candidate, error) {
	return nil, nil
}
func (p *registryProvider) Download(context.Context, domain.Candidate, io.Writer) (DownloadMetadata, error) {
	return DownloadMetadata{}, nil
}

func strictTestFactory(id string, node yaml.Node, _ Dependencies) (Provider, error) {
	var settings struct {
		Type  string `yaml:"type"`
		Token string `yaml:"token"`
	}
	if err := node.Decode(&settings); err != nil {
		return nil, err
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if key != "type" && key != "token" {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	if settings.Token == "" {
		return nil, fmt.Errorf("token is required")
	}
	return &registryProvider{id: id, token: settings.Token, languages: map[domain.Language]bool{"en": true}}, nil
}

func yamlNode(t *testing.T, value string) yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(value), &node); err != nil {
		t.Fatal(err)
	}
	return *node.Content[0]
}

func TestRegistryRejectsDuplicateInstanceIDsAndUnknownTypes(t *testing.T) {
	registry := NewRegistry(map[string]Factory{"fake": strictTestFactory})
	_, err := registry.Build([]InstanceSpec{{ID: "one", Type: "fake", Settings: yamlNode(t, "type: fake\ntoken: a")}, {ID: "one", Type: "fake", Settings: yamlNode(t, "type: fake\ntoken: b")}}, Dependencies{})
	if err == nil {
		t.Fatal("expected duplicate instance error")
	}
	_, err = registry.Build([]InstanceSpec{{ID: "one", Type: "missing", Settings: yamlNode(t, "type: missing")}}, Dependencies{})
	if err == nil {
		t.Fatal("expected unknown provider type error")
	}
}

func TestRegistryRejectsMalformedProviderYAMLAndUnsupportedRoutes(t *testing.T) {
	registry := NewRegistry(map[string]Factory{"fake": strictTestFactory})
	_, err := registry.Build([]InstanceSpec{{ID: "one", Type: "fake", Settings: yamlNode(t, "type: fake\ntoken: a\nunexpected: true")}}, Dependencies{})
	if err == nil {
		t.Fatal("expected provider-specific YAML error")
	}
	providers, err := registry.Build([]InstanceSpec{{ID: "one", Type: "fake", Settings: yamlNode(t, "type: fake\ntoken: a")}}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLanguageRoutes(map[domain.Language][]string{"hr": {"one"}}, providers); err == nil {
		t.Fatal("expected unsupported language error")
	}
}

func TestRegistryAllowsTwoInstancesOfSameTypeWithDifferentCredentials(t *testing.T) {
	registry := NewRegistry(map[string]Factory{"fake": strictTestFactory})
	providers, err := registry.Build([]InstanceSpec{
		{ID: "first", Type: "fake", Settings: yamlNode(t, "type: fake\ntoken: a")},
		{ID: "second", Type: "fake", Settings: yamlNode(t, "type: fake\ntoken: b")},
	}, Dependencies{HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if providers["first"].(*registryProvider).token == providers["second"].(*registryProvider).token {
		t.Fatal("provider credentials were conflated")
	}
}
