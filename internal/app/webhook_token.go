package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

func (a *App) configuredInstance(instance string) error {
	for _, configured := range a.Config.Instances {
		if configured.Name == instance {
			return nil
		}
	}
	return unknownInstance(instance)
}

// noWebhookToken reports a configured instance the database does not know
// yet: the daemon creates its row and token at its first start. Like
// forget-instance for an instance without records, it is unknown_instance.
func noWebhookToken(instance string) error {
	return &control.Error{Code: control.CodeUnknownInstance, Message: fmt.Sprintf("instance %s has no webhook token yet; start subsyncd once to create it", instance)}
}

func (a *App) storedWebhookToken(ctx context.Context, instance string) (string, error) {
	token, err := a.Repository.WebhookToken(ctx, instance)
	switch {
	case errors.Is(err, store.ErrNoWebhookToken), errors.Is(err, store.ErrUnknownInstance):
		return "", noWebhookToken(instance)
	case err != nil:
		return "", err
	}
	return token, nil
}

// WebhookURL returns the URL to enter in Arr for an instance's webhook, using
// base, then server.webhook_base_url, then no base (path and query only). It
// is the one place the token is shown, and it never logs it.
func (a *App) WebhookURL(ctx context.Context, instance, base string) (control.WebhookURLResult, error) {
	if err := a.configuredInstance(instance); err != nil {
		return control.WebhookURLResult{}, err
	}
	if base != "" {
		base = config.TrimWebhookBaseURL(base)
		if !config.ValidWebhookBaseURL(base) {
			return control.WebhookURLResult{}, &control.Error{Code: control.CodeUsage, Message: "--base must be an absolute http or https URL without credentials, query or fragment"}
		}
	} else {
		base = a.Config.Server.WebhookBaseURL
	}
	token, err := a.storedWebhookToken(ctx, instance)
	if err != nil {
		return control.WebhookURLResult{}, err
	}
	return control.WebhookURLResult{Instance: instance, URL: catalog.WebhookURLFor(base, instance, token)}, nil
}

// RotateWebhookToken replaces an instance's webhook token while holding the
// mutation lock, so no daemon is running; the new token takes effect at the
// next start. The output never contains either token.
func (a *App) RotateWebhookToken(ctx context.Context, instance string) (control.RotateTokenResult, error) {
	if err := a.configuredInstance(instance); err != nil {
		return control.RotateTokenResult{}, err
	}
	release, err := a.acquireMutationLock()
	if err != nil {
		return control.RotateTokenResult{}, err
	}
	defer release()
	if err := a.Repository.RotateWebhookToken(ctx, instance, nil); err != nil {
		if errors.Is(err, store.ErrUnknownInstance) {
			return control.RotateTokenResult{}, noWebhookToken(instance)
		}
		return control.RotateTokenResult{}, err
	}
	if token, err := a.Repository.WebhookToken(ctx, instance); err == nil && a.redaction != nil {
		a.redaction.Add(token)
	}
	a.Events.For("app").Log(ctx, slog.LevelInfo, "webhook.token_rotated", "rotated webhook token",
		slog.String("instance", observability.SafeText(instance)))
	return control.RotateTokenResult{Instance: instance, Managed: a.Config.Server.WebhookBaseURL != ""}, nil
}
