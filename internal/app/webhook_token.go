package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

func (a *App) configuredInstance(instance string) error {
	for _, configured := range a.Config.Instances {
		if configured.Name == instance {
			return nil
		}
	}
	return fmt.Errorf("unknown instance %q", instance)
}

func (a *App) storedWebhookToken(ctx context.Context, instance string) (string, error) {
	token, err := a.Repository.WebhookToken(ctx, instance)
	switch {
	case errors.Is(err, store.ErrNoWebhookToken):
		return "", fmt.Errorf("instance %s has no webhook token yet; start subsyncd once to create it", instance)
	case errors.Is(err, store.ErrUnknownInstance):
		return "", fmt.Errorf("instance %s has no webhook token yet; start subsyncd once to create it", instance)
	case err != nil:
		return "", err
	}
	return token, nil
}

// WebhookURL returns the URL to enter in Arr for an instance's webhook, using
// base, then server.webhook_base_url, then no base (path and query only). It
// is the one place the token is shown, and it never logs it.
func (a *App) WebhookURL(ctx context.Context, instance, base string) (string, error) {
	if err := a.configuredInstance(instance); err != nil {
		return "", err
	}
	if base != "" {
		base = config.TrimWebhookBaseURL(base)
		if !config.ValidWebhookBaseURL(base) {
			return "", errors.New("--base must be an absolute http or https URL without credentials, query or fragment")
		}
	} else {
		base = a.Config.Server.WebhookBaseURL
	}
	token, err := a.storedWebhookToken(ctx, instance)
	if err != nil {
		return "", err
	}
	return catalog.WebhookURLFor(base, instance, token), nil
}

// RotateWebhookToken replaces an instance's webhook token. The old token stops
// authenticating at the next start; the output never contains either token.
func (a *App) RotateWebhookToken(ctx context.Context, instance string) (string, error) {
	if err := a.configuredInstance(instance); err != nil {
		return "", err
	}
	release, err := a.acquireMutationLock()
	if err != nil {
		return "", err
	}
	defer release()
	if err := a.Repository.RotateWebhookToken(ctx, instance, nil); err != nil {
		if errors.Is(err, store.ErrUnknownInstance) {
			return "", fmt.Errorf("instance %s has no webhook token yet; start subsyncd once to create it", instance)
		}
		return "", err
	}
	if token, err := a.Repository.WebhookToken(ctx, instance); err == nil && a.redaction != nil {
		a.redaction.Add(token)
	}
	a.Events.For("app").Log(ctx, slog.LevelInfo, "webhook.token_rotated", "rotated webhook token",
		slog.String("instance", observability.SafeText(instance)))
	if a.Config.Server.WebhookBaseURL != "" {
		return fmt.Sprintf("webhook token rotated for instance %s; the Arr connection is updated in Arr at the next start. A running daemon keeps using the old token until it restarts: restart subsyncd now.", instance), nil
	}
	return fmt.Sprintf("webhook token rotated for instance %s; run subsyncd webhook-url --instance %s and update the URL in Arr by hand. A running daemon keeps using the old token until it restarts: restart subsyncd now.", instance, instance), nil
}
