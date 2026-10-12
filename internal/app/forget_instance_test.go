package app

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
)

func openForgetInstanceApp(t *testing.T, logs *bytes.Buffer) *App {
	t.Helper()
	application, _ := openForgetInstanceAppWithConfig(t, logs)
	return application
}

func openForgetInstanceAppWithConfig(t *testing.T, logs *bytes.Buffer) (*App, config.Config) {
	t.Helper()
	cfg := testConfig(t)
	events, err := observability.New(logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if err := application.Repository.EnsureInstance(t.Context(), "old", "sonarr", "http://old.invalid", now); err != nil {
		t.Fatal(err)
	}
	media := domain.Media{
		EntityID: 7, Ref: domain.MediaRef{Instance: "old", Kind: domain.MediaEpisode, FileID: 7},
		Fingerprint: domain.MediaFingerprint{Path: "/media/old.mkv", FileID: 7, Size: 1, ModTime: now},
		Title:       "Old", Season: 1, Episode: 1,
	}
	if _, _, err := application.Repository.UpsertMedia(t.Context(), media); err != nil {
		t.Fatal(err)
	}
	return application, cfg
}

func TestForgetInstanceRefusesConfiguredInstance(t *testing.T) {
	var logs bytes.Buffer
	application := openForgetInstanceApp(t, &logs)

	for _, apply := range []bool{false, true} {
		_, err := application.ForgetInstance(t.Context(), "tv", apply)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != control.CodeUsage || !strings.Contains(coded.Message, "still configured; remove it from the configuration first") {
			t.Fatalf("apply=%t: err=%#v, want usage", apply, err)
		}
	}
}

func TestForgetInstancePreviewKeepsRows(t *testing.T) {
	var logs bytes.Buffer
	application := openForgetInstanceApp(t, &logs)

	outputResult, err := application.ForgetInstance(t.Context(), "old", false)
	output := outputResult.Text()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "media=1") || !strings.Contains(output, "--yes") {
		t.Fatalf("output=%q", output)
	}
	if items, err := application.Repository.ListMediaByInstance(t.Context(), "old"); err != nil || len(items) != 1 {
		t.Fatalf("media=%d,%v", len(items), err)
	}
}

func TestForgetInstanceDeletesAndLogsCounts(t *testing.T) {
	var logs bytes.Buffer
	application := openForgetInstanceApp(t, &logs)

	outputResult, err := application.ForgetInstance(t.Context(), "old", true)
	output := outputResult.Text()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "media=1") || !strings.Contains(output, "subtitle files") || !strings.Contains(output, "rollback_copies=0") {
		t.Fatalf("output=%q", output)
	}
	if items, err := application.Repository.ListMediaByInstance(t.Context(), "old"); err != nil || len(items) != 0 {
		t.Fatalf("media=%d,%v", len(items), err)
	}
	if !strings.Contains(logs.String(), `"event":"instance.forgotten"`) || !strings.Contains(logs.String(), `"media_count":1`) {
		t.Fatalf("logs=%s", logs.String())
	}
}

func TestForgetInstancePreviewCommandOpensReadOnly(t *testing.T) {
	if !readOnlyCommand("forget-instance-preview") || readOnlyCommand("forget-instance") {
		t.Fatal("preview must open read-only and forget-instance must not")
	}
}

func TestForgetInstancePreviewRunsWhileDaemonHoldsLock(t *testing.T) {
	var logs bytes.Buffer
	_, cfg := openForgetInstanceAppWithConfig(t, &logs)
	diagnostic, err := New(t.Context(), cfg, Options{ReadOnly: true, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()

	outputResult, err := diagnostic.ForgetInstance(t.Context(), "old", false)
	output := outputResult.Text()
	if err != nil || !strings.Contains(output, "media=1") {
		t.Fatalf("preview output=%q err=%v", output, err)
	}
	if _, err := diagnostic.ForgetInstance(t.Context(), "old", true); err == nil {
		t.Fatal("read-only application deleted records")
	}
}
