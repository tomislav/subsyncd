package catalog

import "testing"

func TestWebhookURLFor(t *testing.T) {
	for _, test := range []struct{ base, instance, token, want string }{
		{"http://subsyncd:8097", "sonarr-main", "ab", "http://subsyncd:8097/webhooks/sonarr-main?token=ab"},
		{"https://h/p", "my show", "ab", "https://h/p/webhooks/my%20show?token=ab"},
		{"https://h/p//", "tv", "ab", "https://h/p/webhooks/tv?token=ab"},
		{"", "radarr", "ab", "/webhooks/radarr?token=ab"},
	} {
		if got := WebhookURLFor(test.base, test.instance, test.token); got != test.want {
			t.Errorf("WebhookURLFor(%q,%q,%q) = %q, want %q", test.base, test.instance, test.token, got, test.want)
		}
	}
}
