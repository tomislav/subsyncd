package catalog

import (
	"net/url"
	"strings"
)

// WebhookURLFor returns the URL Arr must call for an instance's webhooks:
// base (trailing slashes trimmed) + /webhooks/<escaped name>?token=<token>.
// An empty base yields only the path and query.
func WebhookURLFor(base, instance, token string) string {
	return strings.TrimRight(base, "/") + "/webhooks/" + url.PathEscape(instance) + "?token=" + token
}
