package app

import (
	"context"
	"net/http"
	"testing"

	"gopkg.in/yaml.v3"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
)

func TestBuildKeylessGestdownOffline(t *testing.T) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("type: gestdown\n"), &document); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Providers: map[string]config.ProviderSpec{"gestdown-main": {Type: "gestdown", Settings: *document.Content[0]}}}
	adapters, err := buildProviders(cfg, nil, nil, provider.SystemClock{}, &http.Client{}, nil, observability.Discard())
	if err != nil {
		t.Fatalf("build keyless provider: %v", err)
	}
	p := adapters["gestdown-main"]
	if err := provider.ValidateLanguageRoutes(map[domain.Language][]string{"en": {"gestdown-main"}, "hr": {"gestdown-main"}}, adapters); err != nil {
		t.Fatal(err)
	}
	result, err := p.Search(context.Background(), provider.SearchQuery{Mode: provider.SearchBroad, Language: "en", Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}})
	if err != nil || len(result) != 0 {
		t.Fatalf("movie search = %v, %v", result, err)
	}
}
