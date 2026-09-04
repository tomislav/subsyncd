//go:build provider_contract

package providercontract

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/provider/opensubtitles"
	"subsyncd/internal/provider/subdl"
	"subsyncd/internal/provider/titlovi"
	"subsyncd/internal/store"
)

func TestCredentialGatedProviderContracts(t *testing.T) {
	t.Run("OpenSubtitles", func(t *testing.T) {
		adapter := build(t, "opensubtitles-contract", opensubtitles.Factory, map[string]string{
			"type": "opensubtitles", "api_key": requiredEnv(t, "OPENSUBTITLES_API_KEY"), "username": requiredEnv(t, "OPENSUBTITLES_USERNAME"), "password": requiredEnv(t, "OPENSUBTITLES_PASSWORD"), "user_agent": "subsyncd-provider-contract",
		})
		search(t, adapter, "en")
	})
	t.Run("SubDL", func(t *testing.T) {
		adapter := build(t, "subdl-contract", subdl.Factory, map[string]string{"type": "subdl", "api_key": requiredEnv(t, "SUBDL_API_KEY")})
		search(t, adapter, "en")
	})
	t.Run("Titlovi", func(t *testing.T) {
		adapter := build(t, "titlovi-contract", titlovi.Factory, map[string]string{"type": "titlovi", "username": requiredEnv(t, "TITLOVI_USERNAME"), "password": requiredEnv(t, "TITLOVI_PASSWORD")})
		search(t, adapter, "hr")
	})
}

func build(t *testing.T, id string, factory provider.Factory, values map[string]string) provider.Provider {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "contract.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repository := database.Repository()
	clock := provider.SystemClock{}
	gate := provider.NewGate(repository, clock, 1)
	payload, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	adapter, err := factory(id, *document.Content[0], provider.Dependencies{HTTPClient: &http.Client{Timeout: 30 * time.Second}, Store: database, Clock: clock, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func search(t *testing.T, adapter provider.Provider, language domain.Language) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := adapter.Search(ctx, provider.SearchQuery{Mode: provider.SearchBroad, Language: language, Media: domain.Media{Ref: domain.MediaRef{Instance: "contract", Kind: domain.MediaMovie, FileID: 1}, Title: "The Matrix", Year: 1999, ExternalIDs: domain.ExternalIDs{IMDb: "tt0133093", TMDB: 603}}})
	if err != nil {
		t.Fatalf("%s broad search contract failed: %v", adapter.ID(), err)
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is not set", name)
	}
	return value
}
