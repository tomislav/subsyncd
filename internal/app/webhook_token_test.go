package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
)

func openWebhookTokenApp(t *testing.T, mutate func(*config.Config)) (*App, config.Config, *bytes.Buffer) {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Events: events, redaction: &redactionSecrets{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	return application, cfg, &logs
}

func openReadOnlyWebhookApp(t *testing.T, cfg config.Config) *App {
	t.Helper()
	application, err := New(t.Context(), cfg, Options{ReadOnly: true, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	return application
}

func TestWebhookURLCommandOpensReadOnly(t *testing.T) {
	if !readOnlyCommand("webhook-url") || readOnlyCommand("rotate-webhook-token") {
		t.Fatal("webhook-url must open read-only and rotate-webhook-token must not")
	}
}

func TestWebhookURLPrintsStoredToken(t *testing.T) {
	mutable, cfg, _ := openWebhookTokenApp(t, func(c *config.Config) { c.Server.WebhookBaseURL = "http://subsyncd:8097/p" })
	token := mutable.WebhookTokens()["tv"]
	reader := openReadOnlyWebhookApp(t, cfg)

	gotResult, err := reader.WebhookURL(t.Context(), "tv", "")
	got := gotResult.Text()
	if err != nil || got != "http://subsyncd:8097/p/webhooks/tv?token="+token {
		t.Fatalf("url=%q err=%v", got, err)
	}
	gotResult, err = reader.WebhookURL(t.Context(), "tv", "https://other.example/")
	got = gotResult.Text()
	if err != nil || got != "https://other.example/webhooks/tv?token="+token {
		t.Fatalf("override url=%q err=%v", got, err)
	}
	if _, err := reader.WebhookURL(t.Context(), "tv", "ftp://x"); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("bad base err=%v", err)
	}
}

func TestWebhookURLWithoutBaseIsPathOnly(t *testing.T) {
	mutable, cfg, _ := openWebhookTokenApp(t, nil)
	token := mutable.WebhookTokens()["tv"]
	gotResult, err := openReadOnlyWebhookApp(t, cfg).WebhookURL(t.Context(), "tv", "")
	got := gotResult.Text()
	if err != nil || got != "/webhooks/tv?token="+token {
		t.Fatalf("url=%q err=%v", got, err)
	}
}

func TestWebhookURLBeforeFirstStart(t *testing.T) {
	mutable, cfg, _ := openWebhookTokenApp(t, nil)
	// An instance added to the configuration but not yet started has a row
	// without a token.
	if err := mutable.Repository.EnsureInstance(t.Context(), "late", "sonarr", "http://late.invalid", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	cfg.Instances = append(cfg.Instances, config.InstanceConfig{Name: "late", Type: "sonarr", URL: "http://late.invalid", APIKey: "api"})
	reader, err := New(t.Context(), cfg, Options{ReadOnly: true, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}, "late": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, err = reader.WebhookURL(t.Context(), "late", "")
	if err == nil || !strings.Contains(err.Error(), "start subsyncd once") {
		t.Fatalf("err=%v", err)
	}
}

func TestWebhookURLUnknownInstance(t *testing.T) {
	_, cfg, _ := openWebhookTokenApp(t, nil)
	_, err := openReadOnlyWebhookApp(t, cfg).WebhookURL(t.Context(), "nope", "")
	if err == nil || !strings.Contains(err.Error(), `unknown instance "nope"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestRotateWebhookTokenChangesTokenAndPrintsNoSecret(t *testing.T) {
	application, _, logs := openWebhookTokenApp(t, nil)
	old := application.WebhookTokens()["tv"]
	outputResult, err := application.RotateWebhookToken(t.Context(), "tv")
	output := outputResult.Text()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := application.Repository.WebhookToken(t.Context(), "tv")
	if err != nil || fresh == old || fresh == "" {
		t.Fatalf("token unchanged or empty: %v", err)
	}
	for _, text := range []string{output, logs.String()} {
		if strings.Contains(text, old) || strings.Contains(text, fresh) {
			t.Fatalf("secret leaked in %q", text)
		}
	}
	if !strings.Contains(output, "start subsyncd") || !strings.Contains(output, "subsyncd webhook-url --instance tv --base") || strings.Contains(output, "restart") || strings.Contains(output, "running daemon") {
		t.Fatalf("output=%q", output)
	}
	if !strings.Contains(logs.String(), `"event":"webhook.token_rotated"`) {
		t.Fatalf("logs=%s", logs.String())
	}
	if !containsValue(application.redaction.Values(), fresh) {
		t.Fatal("new token is not registered for redaction")
	}
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRotateWebhookTokenManagedMessage(t *testing.T) {
	application, _, _ := openWebhookTokenApp(t, func(c *config.Config) { c.Server.WebhookBaseURL = "http://subsyncd:8097" })
	outputResult, err := application.RotateWebhookToken(t.Context(), "tv")
	output := outputResult.Text()
	if err != nil || !strings.Contains(output, "start subsyncd") || !strings.Contains(output, "updated in Arr at that start") || strings.Contains(output, "restart") || strings.Contains(output, "running daemon") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestRotateWebhookTokenUnknownInstance(t *testing.T) {
	application, _, _ := openWebhookTokenApp(t, nil)
	if _, err := application.RotateWebhookToken(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), `unknown instance "nope"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestRotateWebhookTokenRefusedWhileLocked(t *testing.T) {
	_, cfg, _ := openWebhookTokenApp(t, nil)
	_, err := New(t.Context(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err == nil || !strings.Contains(err.Error(), "another subsyncd mutation process is active") {
		t.Fatalf("err=%v", err)
	}
}
