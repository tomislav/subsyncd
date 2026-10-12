package store

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestEnsureWebhookTokensGeneratesOnceAndKeeps(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Unix(100, 0)
	for _, name := range []string{"sonarr", "radarr"} {
		if err := repo.EnsureInstance(ctx, name, "sonarr", "http://arr.invalid", now); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"sonarr", "radarr"}
	first, err := repo.EnsureWebhookTokens(ctx, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !tokenPattern.MatchString(first["sonarr"]) || !tokenPattern.MatchString(first["radarr"]) || first["sonarr"] == first["radarr"] {
		t.Fatalf("tokens = %v", first)
	}
	second, err := repo.EnsureWebhookTokens(ctx, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second["sonarr"] != first["sonarr"] || second["radarr"] != first["radarr"] {
		t.Fatal("second call changed tokens")
	}
	if err := repo.EnsureInstance(ctx, "sonarr", "sonarr", "http://other.invalid", now); err != nil {
		t.Fatal(err)
	}
	got, err := repo.WebhookToken(ctx, "sonarr")
	if err != nil || got != first["sonarr"] {
		t.Fatalf("token after EnsureInstance = %v", err)
	}
}

func TestEnsureWebhookTokensRandomFailureWritesNothing(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	if err := repo.EnsureInstance(ctx, "sonarr", "sonarr", "http://arr.invalid", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnsureWebhookTokens(ctx, []string{"sonarr"}, failingReader{}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := repo.WebhookToken(ctx, "sonarr"); !errors.Is(err, ErrNoWebhookToken) {
		t.Fatalf("err = %v, want ErrNoWebhookToken", err)
	}
}

func TestRotateWebhookTokenReplaces(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	if err := repo.EnsureInstance(ctx, "sonarr", "sonarr", "http://arr.invalid", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	tokens, err := repo.EnsureWebhookTokens(ctx, []string{"sonarr"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RotateWebhookToken(ctx, "sonarr", nil); err != nil {
		t.Fatal(err)
	}
	rotated, err := repo.WebhookToken(ctx, "sonarr")
	if err != nil || rotated == tokens["sonarr"] || !tokenPattern.MatchString(rotated) {
		t.Fatalf("rotated token invalid: %v", err)
	}
	if err := repo.RotateWebhookToken(ctx, "missing", nil); !errors.Is(err, ErrUnknownInstance) {
		t.Fatalf("err = %v, want ErrUnknownInstance", err)
	}
}

func TestWebhookConnectionRoundTrip(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	if err := repo.EnsureInstance(ctx, "sonarr", "sonarr", "http://arr.invalid", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	zero, err := repo.WebhookConnection(ctx, "sonarr")
	if err != nil || !zero.CheckedAt.IsZero() || zero.ID != 0 || zero.ArrURL != "" {
		t.Fatalf("fresh connection = %+v, %v", zero, err)
	}
	want := WebhookConnection{ID: 7, ArrURL: "http://arr.invalid", CheckedAt: time.Unix(1700000000, 123456789).UTC()}
	if err := repo.RecordWebhookConnection(ctx, "sonarr", want); err != nil {
		t.Fatal(err)
	}
	got, err := repo.WebhookConnection(ctx, "sonarr")
	if err != nil || got.ID != want.ID || got.ArrURL != want.ArrURL || !got.CheckedAt.Equal(want.CheckedAt) {
		t.Fatalf("connection = %+v, %v", got, err)
	}
}

func TestForgetInstanceRemovesWebhookState(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	if err := repo.EnsureInstance(ctx, "sonarr", "sonarr", "http://arr.invalid", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnsureWebhookTokens(ctx, []string{"sonarr"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ForgetInstance(ctx, "sonarr", true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.WebhookToken(ctx, "sonarr"); !errors.Is(err, ErrUnknownInstance) {
		t.Fatalf("err = %v, want ErrUnknownInstance", err)
	}
}
